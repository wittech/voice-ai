// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package assistant_sip

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"sync"

	"github.com/rapidaai/api/assistant-api/config"
	internal_adapter "github.com/rapidaai/api/assistant-api/internal/adapters"
	internal_assistant_entity "github.com/rapidaai/api/assistant-api/internal/entity/assistants"
	internal_conversation_entity "github.com/rapidaai/api/assistant-api/internal/entity/conversations"
	internal_services "github.com/rapidaai/api/assistant-api/internal/services"
	internal_assistant_service "github.com/rapidaai/api/assistant-api/internal/services/assistant"
	internal_telephony "github.com/rapidaai/api/assistant-api/internal/telephony"
	internal_sip "github.com/rapidaai/api/assistant-api/sip/internal"
	web_client "github.com/rapidaai/pkg/clients/web"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/connectors"
	"github.com/rapidaai/pkg/storages"
	storage_files "github.com/rapidaai/pkg/storages/file-storage"
	"github.com/rapidaai/pkg/types"
	type_enums "github.com/rapidaai/pkg/types/enums"
	"github.com/rapidaai/pkg/utils"
)

// SIPManager manages SIP connections for voice conversations
// SIP uses native signaling (UDP/TCP/TLS) and RTP for audio - no WebSocket
// Multi-tenant: Single shared server, config resolved per-call from assistant's deployment
type SIPManager struct {
	mu       sync.RWMutex
	cfg      *config.AssistantConfig
	logger   commons.Logger
	server   *internal_sip.Server
	sessions map[string]*internal_sip.SIPSession

	ctx    context.Context
	cancel context.CancelFunc

	postgres   connectors.PostgresConnector
	redis      connectors.RedisConnector
	opensearch connectors.OpenSearchConnector
	storage    storages.Storage

	assistantConversationService internal_services.AssistantConversationService
	assistantService             internal_services.AssistantService
	vaultClient                  web_client.VaultClient
	authClient                   web_client.AuthClient
}

// NewSIPManager creates a new SIP manager
// Multi-tenant: single server, config resolved per-call via ConfigResolver
func NewSIPManager(config *config.AssistantConfig, logger commons.Logger,
	postgres connectors.PostgresConnector,
	redis connectors.RedisConnector,
	opensearch connectors.OpenSearchConnector,
	vectordb connectors.VectorConnector) *SIPManager {
	return &SIPManager{
		cfg:                          config,
		logger:                       logger,
		postgres:                     postgres,
		redis:                        redis,
		opensearch:                   opensearch,
		assistantConversationService: internal_assistant_service.NewAssistantConversationService(logger, postgres, storage_files.NewStorage(config.AssetStoreConfig, logger)),
		assistantService:             internal_assistant_service.NewAssistantService(config, logger, postgres, opensearch),
		storage:                      storage_files.NewStorage(config.AssetStoreConfig, logger),
		vaultClient:                  web_client.NewVaultClientGRPC(&config.AppConfig, logger, redis),
		authClient:                   web_client.NewAuthenticator(&config.AppConfig, logger, redis),
		sessions:                     make(map[string]*internal_sip.SIPSession),
	}
}

// NewSIPConfigFromAppConfig creates internal SIP config from app config parameters
// Used for tenant-specific config (RTP ports, credentials, etc.)
func NewSIPConfigFromAppConfig(server string, port int, transport string, rtpStart, rtpEnd int) *internal_sip.Config {
	transportType := internal_sip.TransportUDP
	switch transport {
	case "tcp":
		transportType = internal_sip.TransportTCP
	case "tls":
		transportType = internal_sip.TransportTLS
	}
	return &internal_sip.Config{
		Server:            server,
		Port:              port,
		Transport:         transportType,
		RTPPortRangeStart: rtpStart,
		RTPPortRangeEnd:   rtpEnd,
	}
}

// NewSIPListenConfig creates a ListenConfig for the shared SIP server
// Multi-tenant: Server listens on this address, tenant config resolved per-call
func (m *SIPManager) listenConfig() *internal_sip.ListenConfig {
	transportType := internal_sip.TransportUDP
	switch m.cfg.SIPConfig.Transport {
	case "tcp":
		transportType = internal_sip.TransportTCP
	case "tls":
		transportType = internal_sip.TransportTLS
	}
	return &internal_sip.ListenConfig{
		Address:   m.cfg.SIPConfig.Server,
		Port:      m.cfg.SIPConfig.Port,
		Transport: transportType,
	}
}

// Start initializes the shared SIP server with multi-tenant config resolution
func (m *SIPManager) Start(ctx context.Context) error {
	m.ctx, m.cancel = context.WithCancel(ctx)
	server, err := internal_sip.NewServer(m.ctx, &internal_sip.ServerConfig{
		ListenConfig:   m.listenConfig(),
		ConfigResolver: m.resolveConfig,
		Logger:         m.logger,
	})
	if err != nil {
		return fmt.Errorf("failed to create SIP server: %w", err)
	}

	// Set up handlers for incoming calls
	server.SetOnInvite(m.handleInvite)
	server.SetOnBye(m.handleBye)
	server.SetOnCancel(m.handleCancel)

	// Start the server
	if err := server.Start(); err != nil {
		return fmt.Errorf("failed to start SIP server: %w", err)
	}
	m.server = server
	return nil
}

// resolveConfig resolves tenant-specific config from SIP INVITE context
// Multi-tenant: Extracts assistant/tenant from To URI and validates API key
// URI format: sip:{assistantID}.rapid-sip@in.rapida.ai
// Auth: X-API-Key header required for authentication
func (m *SIPManager) resolveConfig(inviteCtx *internal_sip.InviteContext) (*internal_sip.InviteResult, error) {
	// 1. Validate API key is present
	if inviteCtx.APIKey == "" {
		m.logger.Warn("SIP call rejected: missing API key",
			"call_id", inviteCtx.CallID,
			"from", inviteCtx.FromURI,
			"to", inviteCtx.ToURI)
		return &internal_sip.InviteResult{
			ShouldAllow: false,
			RejectCode:  401, // Unauthorized
			RejectMsg:   "Missing X-API-Key header",
		}, nil
	}

	// 2. Parse assistant ID from To URI (already extracted by server)
	assistantIDStr := inviteCtx.AssistantID
	if assistantIDStr == "" {
		m.logger.Warn("SIP call rejected: missing assistant ID in URI",
			"call_id", inviteCtx.CallID,
			"to_uri", inviteCtx.ToURI)
		return &internal_sip.InviteResult{
			ShouldAllow: false,
			RejectCode:  404, // Not Found
			RejectMsg:   "Invalid SIP URI format, expected: sip:{assistantID}.rapid-sip@in.rapida.ai",
		}, nil
	}

	// 3. Parse assistant ID to uint64
	assistantID, err := strconv.ParseUint(assistantIDStr, 10, 64)
	if err != nil {
		m.logger.Warn("SIP call rejected: invalid assistant ID format",
			"call_id", inviteCtx.CallID,
			"assistant_id", assistantIDStr,
			"error", err)
		return &internal_sip.InviteResult{
			ShouldAllow: false,
			RejectCode:  404, // Not Found
			RejectMsg:   "Invalid assistant ID format",
		}, nil
	}

	// 4. Validate API key and get auth context
	auth, err := m.validateAPIKey(inviteCtx.APIKey)
	if err != nil {
		m.logger.Warn("SIP call rejected: invalid API key",
			"call_id", inviteCtx.CallID,
			"assistant_id", assistantID,
			"error", err)
		return &internal_sip.InviteResult{
			ShouldAllow: false,
			RejectCode:  403, // Forbidden
			RejectMsg:   "Invalid API key",
		}, nil
	}

	// 5. Load assistant to verify it exists and get config
	assistant, err := m.assistantService.Get(m.ctx, auth, assistantID, utils.GetVersionDefinition("latest"),
		&internal_services.GetAssistantOption{InjectPhoneDeployment: true})
	if err != nil {
		m.logger.Error("SIP call rejected: assistant not found",
			"call_id", inviteCtx.CallID,
			"assistant_id", assistantID,
			"error", err)
		return &internal_sip.InviteResult{
			ShouldAllow: false,
			RejectCode:  404, // Not Found
			RejectMsg:   "Assistant not found",
		}, nil
	}

	// 6. Verify API key has access to this assistant
	if !m.hasAccessToAssistant(auth, assistant) {
		m.logger.Warn("SIP call rejected: API key does not have access to assistant",
			"call_id", inviteCtx.CallID,
			"assistant_id", assistantID)
		return &internal_sip.InviteResult{
			ShouldAllow: false,
			RejectCode:  403, // Forbidden
			RejectMsg:   "API key does not have access to this assistant",
		}, nil
	}

	// 7. Extract SIP config from assistant's deployment settings
	var sipConfig *internal_sip.Config
	if assistant.AssistantPhoneDeployment != nil {
		opts := assistant.AssistantPhoneDeployment.GetOptions()
		sipConfig, _ = GetSIPConfigFromDeployment(opts)
	}
	if sipConfig == nil {
		sipConfig = internal_sip.DefaultConfig()
	}

	tenantID := fmt.Sprintf("%d", *auth.GetCurrentOrganizationId())
	m.logger.Info("SIP call authenticated",
		"call_id", inviteCtx.CallID,
		"assistant_id", assistantID,
		"tenant_id", tenantID,
		"from", inviteCtx.FromURI)

	return &internal_sip.InviteResult{
		Config:      sipConfig,
		ShouldAllow: true,
	}, nil
}

// validateAPIKey validates the API key and returns the auth context
func (m *SIPManager) validateAPIKey(apiKey string) (types.SimplePrinciple, error) {
	// TODO: Implement actual API key validation against the database
	// This should look up the API key in the credentials/tokens table
	// and return the associated organization/project context

	if apiKey == "" {
		return nil, fmt.Errorf("empty API key")
	}

	// Placeholder: In production, validate against database and return proper auth
	return &types.ServiceScope{
		ProjectId:      utils.Ptr(uint64(2257831930382778368)),
		OrganizationId: utils.Ptr(uint64(2257831925018263552)),
		CurrentToken:   apiKey,
	}, nil
}

// hasAccessToAssistant checks if the auth context has access to the assistant
func (m *SIPManager) hasAccessToAssistant(auth types.SimplePrinciple, assistant *internal_assistant_entity.Assistant) bool {
	// Check if the assistant belongs to the same project/organization
	if auth.GetCurrentProjectId() == nil || assistant.ProjectId == 0 {
		return true // Skip check if project info not available
	}
	return *auth.GetCurrentProjectId() == assistant.ProjectId
}

// handleInvite processes incoming SIP INVITE requests
func (m *SIPManager) handleInvite(session *internal_sip.Session, fromURI, toURI string) error {
	m.logger.Info("Incoming SIP INVITE",
		"from", fromURI,
		"to", toURI,
		"call_id", session.GetInfo().CallID)

	// Resolve auth, assistant, and SIP config (multi-tenant)
	auth, assistant, conversation, callerID, sipConfig, err := m.resolveCallContext(m.ctx, fromURI)
	if err != nil {
		m.logger.Error("SIP context resolution failed", "error", err)
		return err
	}

	// Start the call in a goroutine with tenant-specific config
	go m.startCall(m.ctx, session, auth, assistant, conversation, callerID, sipConfig)

	return nil
}

// resolveCallContext resolves auth, assistant, and SIP config for inbound calls
// Multi-tenant: extracts SIP config from assistant's deployment settings
func (m *SIPManager) resolveCallContext(ctx context.Context, fromURI string) (types.SimplePrinciple, *internal_assistant_entity.Assistant, *internal_conversation_entity.AssistantConversation, string, *internal_sip.Config, error) {
	// Hardcoded auth - same as AudioSocket (will be resolved from SIP headers in production)
	auth := &types.ServiceScope{
		ProjectId:      utils.Ptr(uint64(2257831930382778368)),
		OrganizationId: utils.Ptr(uint64(2257831925018263552)),
		CurrentToken:   "3dd5c2eef53d27942bccd892750fda23ea0b92965d4699e73d8e754ab882955f",
	}

	// Hardcoded assistant ID - same as AudioSocket (will be resolved from SIP URI in production)
	assistantID := uint64(2263072539095859200)

	assistant, err := m.assistantService.Get(ctx, auth, assistantID, utils.GetVersionDefinition("latest"), &internal_services.GetAssistantOption{InjectPhoneDeployment: true})
	if err != nil {
		return nil, nil, nil, "", nil, fmt.Errorf("failed to get assistant: %w", err)
	}

	// Extract SIP config from assistant's deployment settings (multi-tenant)
	var sipConfig *internal_sip.Config
	if assistant.AssistantPhoneDeployment != nil {
		opts := assistant.AssistantPhoneDeployment.GetOptions()
		sipConfig, _ = GetSIPConfigFromDeployment(opts)
	}
	if sipConfig == nil {
		// Use defaults if no deployment config
		sipConfig = internal_sip.DefaultConfig()
	}

	callerID := fromURI

	conversation, err := m.assistantConversationService.CreateConversation(
		ctx,
		auth,
		internal_adapter.Identifier(utils.SIP, ctx, auth, callerID),
		assistant.Id,
		assistant.AssistantProviderId,
		type_enums.DIRECTION_INBOUND,
		utils.SIP,
	)
	if err != nil {
		return nil, nil, nil, "", nil, fmt.Errorf("failed to create conversation: %w", err)
	}

	_, _ = m.assistantConversationService.ApplyConversationMetadata(ctx, auth, assistant.Id, conversation.Id,
		[]*types.Metadata{types.NewMetadata("sip.caller_uri", fromURI)})

	return auth, assistant, conversation, callerID, sipConfig, nil
}

// startCall starts the SIP conversation with the assistant
// Multi-tenant: receives config specific to this call/tenant
func (m *SIPManager) startCall(ctx context.Context, session *internal_sip.Session, auth types.SimplePrinciple, assistant *internal_assistant_entity.Assistant, conversation *internal_conversation_entity.AssistantConversation, callerID string, sipConfig *internal_sip.Config) {
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	callID := session.GetInfo().CallID
	tenantID := fmt.Sprintf("%d", *auth.GetCurrentOrganizationId())

	// Store session with tenant-specific config
	m.mu.Lock()
	m.sessions[callID] = &internal_sip.SIPSession{
		CallID:      callID,
		AssistantID: assistant.Id,
		TenantID:    tenantID,
		Auth:        auth,
		Config:      sipConfig,
		Cancel:      cancel,
	}
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		delete(m.sessions, callID)
		m.mu.Unlock()
	}()

	// Create SIP streamer for this inbound call (uses existing session, no new server)
	streamer, err := internal_telephony.Telephony(internal_telephony.SIP).SipStreamer()
	// 	allCtx, &internal_sip_telephony.InboundStreamerConfig{
	// 	Config:       sipConfig,
	// 	Logger:       m.logger,
	// 	Session:      session,
	// 	Assistant:    assistant,
	// 	Conversation: conversation,
	// })
	if err != nil {
		m.logger.Error("Failed to create SIP streamer", "error", err, "call_id", callID)
		return
	}

	identifier := internal_adapter.Identifier(utils.SIP, callCtx, auth, callerID)
	talker, err := internal_adapter.GetTalker(
		utils.PhoneCall,
		callCtx,
		m.cfg,
		m.logger,
		m.postgres,
		m.opensearch,
		m.redis,
		m.storage,
		streamer,
	)
	if err != nil {
		if closeable, ok := streamer.(io.Closer); ok {
			closeable.Close()
		}
		m.logger.Error("Failed to create SIP talker", "error", err, "call_id", callID)
		return
	}

	m.logger.Info("SIP call started",
		"call_id", callID,
		"assistant_id", assistant.Id,
		"conversation_id", conversation.Id,
		"caller", callerID)

	if err := talker.Talk(callCtx, auth, identifier); err != nil {
		m.logger.Warn("SIP talker exited", "error", err, "call_id", callID)
	}

	m.logger.Info("SIP call ended", "call_id", callID)
}

// handleBye processes SIP BYE requests
func (m *SIPManager) handleBye(session *internal_sip.Session) error {
	callID := session.GetInfo().CallID
	m.logger.Info("SIP BYE received", "call_id", callID)

	// Cancel the call context
	m.mu.Lock()
	if sipSession, exists := m.sessions[callID]; exists {
		if sipSession.Cancel != nil {
			sipSession.Cancel()
		}
		delete(m.sessions, callID)
	}
	m.mu.Unlock()

	return nil
}

// handleCancel processes SIP CANCEL requests
func (m *SIPManager) handleCancel(session *internal_sip.Session) error {
	callID := session.GetInfo().CallID
	m.logger.Info("SIP CANCEL received", "call_id", callID)

	// Cancel the call context
	m.mu.Lock()
	if sipSession, exists := m.sessions[callID]; exists {
		if sipSession.Cancel != nil {
			sipSession.Cancel()
		}
		delete(m.sessions, callID)
	}
	m.mu.Unlock()

	return nil
}

// HandleIncomingCall processes an incoming SIP INVITE
// This is called when a SIP call arrives for an assistant
func (m *SIPManager) HandleIncomingCall(
	ctx context.Context,
	auth types.SimplePrinciple,
	assistantID uint64,
	callerID string,
	sipConfig *internal_sip.Config,
) error {
	m.logger.Info("Incoming SIP call",
		"assistant", assistantID,
		"caller", callerID)

	// Load assistant
	assistant, err := m.assistantService.Get(ctx, auth, assistantID, nil, nil)
	if err != nil {
		m.logger.Errorf("Failed to load assistant for SIP call: %v", err)
		return fmt.Errorf("failed to load assistant: %w", err)
	}

	// Create identifier for the conversation
	identifier := internal_adapter.Identifier(utils.SIP, ctx, auth, callerID)

	// Create new conversation for SIP session
	conversation, err := m.assistantConversationService.CreateConversation(
		ctx, auth, identifier, assistantID, assistant.AssistantProviderId,
		type_enums.DIRECTION_INBOUND, utils.SIP,
	)
	if err != nil {
		m.logger.Errorf("Failed to create conversation for SIP call: %v", err)
		return fmt.Errorf("failed to create conversation: %w", err)
	}

	// Create SIP streamer
	sipCtx, cancel := context.WithCancel(ctx)
	streamer, err := internal_telephony.Telephony(internal_telephony.SIP).SipStreamer()
	// streamer, err := internal_sip_telephony.NewStreamer(sipCtx, &internal_sip_telephony.StreamerConfig{
	// 	Config:       sipConfig,
	// 	Logger:       m.logger,
	// 	TenantID:     fmt.Sprintf("%d", *auth.GetCurrentOrganizationId()),
	// 	Assistant:    assistant,
	// 	Conversation: conversation,
	// })
	if err != nil {
		cancel()
		m.assistantConversationService.ApplyConversationMetrics(ctx, auth, assistantID, conversation.Id,
			[]*types.Metric{{Name: type_enums.STATUS.String(), Value: type_enums.RECORD_FAILED.String(), Description: "SIP setup failed"}})
		m.logger.Errorf("Failed to create SIP streamer: %v", err)
		return fmt.Errorf("failed to create SIP streamer: %w", err)
	}

	// Create talker with SIP source
	talker, err := internal_adapter.GetTalker(
		utils.SIP,
		ctx,
		m.cfg,
		m.logger,
		m.postgres,
		m.opensearch,
		m.redis,
		m.storage,
		streamer,
	)
	if err != nil {
		if closeable, ok := streamer.(io.Closer); ok {
			closeable.Close()
		}
		cancel()
		m.assistantConversationService.ApplyConversationMetrics(ctx, auth, assistantID, conversation.Id,
			[]*types.Metric{{Name: type_enums.STATUS.String(), Value: type_enums.RECORD_FAILED.String(), Description: "Talker creation failed"}})
		m.logger.Errorf("Failed to create SIP talker: %v", err)
		return fmt.Errorf("failed to create talker: %w", err)
	}

	// Store session with tenant-specific config
	tenantID := fmt.Sprintf("%d", *auth.GetCurrentOrganizationId())
	callID := fmt.Sprintf("sip-%s-%d-%d", tenantID, assistantID, conversation.Id)
	m.mu.Lock()
	m.sessions[callID] = &internal_sip.SIPSession{
		CallID:      callID,
		AssistantID: assistantID,
		TenantID:    tenantID,
		Auth:        auth,
		Config:      sipConfig,
		Cancel:      cancel,
	}
	m.mu.Unlock()

	m.logger.Infof("SIP session started: assistant=%d, conversation=%d, caller=%s",
		assistantID, conversation.Id, callerID)

	// Start the conversation in a goroutine
	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.sessions, callID)
			m.mu.Unlock()
			cancel()
		}()

		if err := talker.Talk(sipCtx, auth, identifier); err != nil {
			m.logger.Errorf("SIP conversation error: %v", err)
		}

		m.logger.Infof("SIP session ended: assistant=%d, conversation=%d",
			assistantID, conversation.Id)
	}()

	return nil
}

// EndCall terminates an active SIP call
func (m *SIPManager) EndCall(callID string) error {
	m.mu.Lock()
	session, exists := m.sessions[callID]
	if !exists {
		m.mu.Unlock()
		return fmt.Errorf("call not found: %s", callID)
	}
	delete(m.sessions, callID)
	m.mu.Unlock()

	if session.Cancel != nil {
		session.Cancel()
	}

	m.logger.Info("SIP call ended", "callID", callID)
	return nil
}

// GetActiveCalls returns the number of active SIP calls
func (m *SIPManager) GetActiveCalls() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// Stop stops the SIP manager and terminates all active calls
func (m *SIPManager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}

	m.mu.Lock()
	// Stop the shared server
	if m.server != nil {
		m.server.Stop()
		m.server = nil
	}

	// Cancel all active sessions
	for callID, session := range m.sessions {
		if session.Cancel != nil {
			session.Cancel()
		}
		delete(m.sessions, callID)
	}
	m.mu.Unlock()

	m.logger.Info("SIP Manager stopped")
}

// Close implements the closeable interface for graceful shutdown
func (m *SIPManager) Close(ctx context.Context) error {
	m.Stop()
	return nil
}

// GetSIPConfigFromDeployment extracts SIP configuration from assistant deployment
func GetSIPConfigFromDeployment(opts map[string]interface{}) (*internal_sip.Config, error) {
	config := &internal_sip.Config{
		Transport:         internal_sip.TransportUDP,
		RTPPortRangeStart: 10000,
		RTPPortRangeEnd:   20000,
	}

	if server, ok := opts["sip_server"].(string); ok {
		config.Server = server
	}
	if port, ok := opts["sip_port"].(float64); ok {
		config.Port = int(port)
	}
	if transport, ok := opts["sip_transport"].(string); ok {
		config.Transport = internal_sip.Transport(transport)
	}
	if username, ok := opts["sip_username"].(string); ok {
		config.Username = username
	}
	if password, ok := opts["sip_password"].(string); ok {
		config.Password = password
	}
	if realm, ok := opts["sip_realm"].(string); ok {
		config.Realm = realm
	}
	if rtpStart, ok := opts["rtp_port_range_start"].(float64); ok {
		config.RTPPortRangeStart = int(rtpStart)
	}
	if rtpEnd, ok := opts["rtp_port_range_end"].(float64); ok {
		config.RTPPortRangeEnd = int(rtpEnd)
	}

	return config, nil
}

// // SIPCallReceiver handles incoming SIP call webhooks (for SIP trunks that support webhooks)
// // This is similar to telephony providers like Twilio that use webhooks for call events
// // Multi-tenant: Config is passed per-call, resolved from assistant's deployment settings
// // Note: This doesn't start a SIP server - it handles webhook-based calls where the provider manages SIP
// func (cApi *ConversationApi) SIPCallReceiver(ctx context.Context, auth types.SimplePrinciple, assistantID uint64, callerID string, sipConfig *internal_sip.Config) error {
// 	manager := NewSIPManager(cApi)
// 	manager.ctx, manager.cancel = context.WithCancel(ctx)
// 	return manager.HandleIncomingCall(ctx, auth, assistantID, callerID, sipConfig)
// }

// // SIPCallWebhookRequest represents an incoming SIP call webhook
// type SIPCallWebhookRequest struct {
// 	CallID    string                 `json:"call_id"`
// 	From      string                 `json:"from"`
// 	To        string                 `json:"to"`
// 	Direction string                 `json:"direction"`
// 	SIPConfig map[string]interface{} `json:"sip_config,omitempty"`
// }

// // SIPEventWebhookRequest represents a SIP event webhook
// type SIPEventWebhookRequest struct {
// 	CallID    string                 `json:"call_id"`
// 	EventType string                 `json:"event_type"` // answered, hangup, dtmf, etc.
// 	Timestamp string                 `json:"timestamp"`
// 	Data      map[string]interface{} `json:"data,omitempty"`
// }

// // SIPCallWebhook handles incoming SIP call webhooks from SIP trunks
// // POST /v1/talk/sip/call/:assistantId
// // This endpoint is called by SIP providers (Telnyx, SignalWire, etc.) when a call arrives
// func (cApi *ConversationApi) SIPCallWebhook(c *gin.Context) {
// 	auth, isAuthenticated := types.GetAuthPrinciple(c)
// 	if !isAuthenticated {
// 		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthenticated request"})
// 		return
// 	}

// 	assistantIdStr := c.Param("assistantId")
// 	assistantId, err := strconv.ParseUint(assistantIdStr, 10, 64)
// 	if err != nil {
// 		logger.Errorf("Invalid assistantId: %v", err)
// 		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid assistantId"})
// 		return
// 	}

// 	var req SIPCallWebhookRequest
// 	if err := c.ShouldBindJSON(&req); err != nil {
// 		logger.Errorf("Invalid SIP webhook request: %v", err)
// 		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
// 		return
// 	}

// 	// Extract SIP config from request or use defaults
// 	sipConfig, err := GetSIPConfigFromDeployment(req.SIPConfig)
// 	if err != nil {
// 		logger.Errorf("Invalid SIP config: %v", err)
// 		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid SIP configuration"})
// 		return
// 	}

// 	// Handle the incoming call
// 	if err := SIPCallReceiver(c.Request.Context(), auth, assistantId, req.From, sipConfig); err != nil {
// 		logger.Errorf("Failed to handle SIP call: %v", err)
// 		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to handle call"})
// 		return
// 	}

// 	logger.Infof("SIP call webhook received: assistant=%d, from=%s, callId=%s",
// 		assistantId, req.From, req.CallID)

// 	c.JSON(http.StatusOK, gin.H{
// 		"status":  "accepted",
// 		"call_id": req.CallID,
// 	})
// }

// // SIPEventWebhook handles SIP event webhooks (hangup, dtmf, etc.)
// // POST /v1/talk/sip/event/:assistantId/:conversationId
// func (cApi *ConversationApi) SIPEventWebhook(c *gin.Context) {
// 	auth, isAuthenticated := types.GetAuthPrinciple(c)
// 	if !isAuthenticated {
// 		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthenticated request"})
// 		return
// 	}

// 	assistantIdStr := c.Param("assistantId")
// 	assistantId, err := strconv.ParseUint(assistantIdStr, 10, 64)
// 	if err != nil {
// 		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid assistantId"})
// 		return
// 	}

// 	conversationIdStr := c.Param("conversationId")
// 	conversationId, err := strconv.ParseUint(conversationIdStr, 10, 64)
// 	if err != nil {
// 		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversationId"})
// 		return
// 	}

// 	var req SIPEventWebhookRequest
// 	if err := c.ShouldBindJSON(&req); err != nil {
// 		logger.Errorf("Invalid SIP event webhook: %v", err)
// 		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
// 		return
// 	}

// 	logger.Infof("SIP event webhook: assistant=%d, conversation=%d, event=%s",
// 		assistantId, conversationId, req.EventType)

// 	// Process event based on type
// 	switch req.EventType {
// 	case "hangup", "bye":
// 		// Apply end metrics
// 		assistantConversationService.ApplyConversationMetrics(c, auth, assistantId, conversationId,
// 			[]*types.Metric{{Name: type_enums.STATUS.String(), Value: type_enums.RECORD_COMPLETE.String(), Description: "SIP call ended"}})
// 	case "answered":
// 		// Apply connected metrics
// 		assistantConversationService.ApplyConversationMetrics(c, auth, assistantId, conversationId,
// 			[]*types.Metric{{Name: "sip_answered", Value: "true", Description: "SIP call answered"}})
// 	}

// 	c.JSON(http.StatusOK, gin.H{"status": "processed"})
// }
