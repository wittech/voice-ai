// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package assistant_talk_api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/gin-gonic/gin"
	internal_adapter "github.com/rapidaai/api/assistant-api/internal/adapters"
	internal_sip "github.com/rapidaai/api/assistant-api/internal/sip"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/types"
	type_enums "github.com/rapidaai/pkg/types/enums"
	"github.com/rapidaai/pkg/utils"
)

// SIPManager manages SIP connections for voice conversations
// SIP uses native signaling (UDP/TCP/TLS) and RTP for audio - no WebSocket
type SIPManager struct {
	mu     sync.RWMutex
	logger commons.Logger
	cApi   *ConversationApi

	// SIP server configuration
	config *internal_sip.Config

	// Active SIP sessions mapped by call ID
	sessions map[string]*SIPSession
}

// SIPSession represents an active SIP call session
type SIPSession struct {
	CallID      string
	AssistantID uint64
	Auth        types.SimplePrinciple
	Streamer    *internal_sip.Streamer
	Cancel      context.CancelFunc
}

// NewSIPManager creates a new SIP manager
func NewSIPManager(cApi *ConversationApi, config *internal_sip.Config) *SIPManager {
	return &SIPManager{
		logger:   cApi.logger,
		cApi:     cApi,
		config:   config,
		sessions: make(map[string]*SIPSession),
	}
}

// Start begins the SIP server and listens for incoming calls
func (m *SIPManager) Start(ctx context.Context) error {
	if m.config == nil {
		return fmt.Errorf("SIP config is nil")
	}

	if err := m.config.Validate(); err != nil {
		return fmt.Errorf("invalid SIP config: %w", err)
	}

	m.logger.Info("SIP Manager started",
		"server", m.config.Server,
		"port", m.config.Port,
		"transport", m.config.Transport)

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
	assistant, err := m.cApi.assistantService.Get(ctx, auth, assistantID, nil, nil)
	if err != nil {
		m.logger.Errorf("Failed to load assistant for SIP call: %v", err)
		return fmt.Errorf("failed to load assistant: %w", err)
	}

	// Create identifier for the conversation
	identifier := internal_adapter.Identifier(utils.SIP, ctx, auth, callerID)

	// Create new conversation for SIP session
	conversation, err := m.cApi.assistantConversationService.CreateConversation(
		ctx, auth, identifier, assistantID, assistant.AssistantProviderId,
		type_enums.DIRECTION_INBOUND, utils.SIP,
	)
	if err != nil {
		m.logger.Errorf("Failed to create conversation for SIP call: %v", err)
		return fmt.Errorf("failed to create conversation: %w", err)
	}

	// Create SIP streamer
	sipCtx, cancel := context.WithCancel(ctx)
	streamer, err := internal_sip.NewStreamer(sipCtx, &internal_sip.StreamerConfig{
		Config:       sipConfig,
		Logger:       m.logger,
		TenantID:     fmt.Sprintf("%d", *auth.GetCurrentOrganizationId()),
		Assistant:    assistant,
		Conversation: conversation,
	})
	if err != nil {
		cancel()
		m.cApi.assistantConversationService.ApplyConversationMetrics(ctx, auth, assistantID, conversation.Id,
			[]*types.Metric{{Name: type_enums.STATUS.String(), Value: type_enums.RECORD_FAILED.String(), Description: "SIP setup failed"}})
		m.logger.Errorf("Failed to create SIP streamer: %v", err)
		return fmt.Errorf("failed to create SIP streamer: %w", err)
	}

	// Create talker with SIP source
	talker, err := internal_adapter.GetTalker(
		utils.SIP,
		ctx,
		m.cApi.cfg,
		m.cApi.logger,
		m.cApi.postgres,
		m.cApi.opensearch,
		m.cApi.redis,
		m.cApi.storage,
		streamer,
	)
	if err != nil {
		if closeable, ok := streamer.(io.Closer); ok {
			closeable.Close()
		}
		cancel()
		m.cApi.assistantConversationService.ApplyConversationMetrics(ctx, auth, assistantID, conversation.Id,
			[]*types.Metric{{Name: type_enums.STATUS.String(), Value: type_enums.RECORD_FAILED.String(), Description: "Talker creation failed"}})
		m.logger.Errorf("Failed to create SIP talker: %v", err)
		return fmt.Errorf("failed to create talker: %w", err)
	}

	// Store session
	callID := fmt.Sprintf("sip-%d-%d", assistantID, conversation.Id)
	m.mu.Lock()
	m.sessions[callID] = &SIPSession{
		CallID:      callID,
		AssistantID: assistantID,
		Auth:        auth,
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
	m.mu.Lock()
	for callID, session := range m.sessions {
		if session.Cancel != nil {
			session.Cancel()
		}
		delete(m.sessions, callID)
	}
	m.mu.Unlock()

	m.logger.Info("SIP Manager stopped")
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

// SIPCallReceiver handles incoming SIP call webhooks (for SIP trunks that support webhooks)
// This is similar to telephony providers like Twilio that use webhooks for call events
func (cApi *ConversationApi) SIPCallReceiver(ctx context.Context, auth types.SimplePrinciple, assistantID uint64, callerID string, sipConfig *internal_sip.Config) error {
	manager := NewSIPManager(cApi, sipConfig)
	return manager.HandleIncomingCall(ctx, auth, assistantID, callerID, sipConfig)
}

// SIPCallWebhookRequest represents an incoming SIP call webhook
type SIPCallWebhookRequest struct {
	CallID    string                 `json:"call_id"`
	From      string                 `json:"from"`
	To        string                 `json:"to"`
	Direction string                 `json:"direction"`
	SIPConfig map[string]interface{} `json:"sip_config,omitempty"`
}

// SIPEventWebhookRequest represents a SIP event webhook
type SIPEventWebhookRequest struct {
	CallID    string                 `json:"call_id"`
	EventType string                 `json:"event_type"` // answered, hangup, dtmf, etc.
	Timestamp string                 `json:"timestamp"`
	Data      map[string]interface{} `json:"data,omitempty"`
}

// SIPCallWebhook handles incoming SIP call webhooks from SIP trunks
// POST /v1/talk/sip/call/:assistantId
// This endpoint is called by SIP providers (Telnyx, SignalWire, etc.) when a call arrives
func (cApi *ConversationApi) SIPCallWebhook(c *gin.Context) {
	auth, isAuthenticated := types.GetAuthPrinciple(c)
	if !isAuthenticated {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthenticated request"})
		return
	}

	assistantIdStr := c.Param("assistantId")
	assistantId, err := strconv.ParseUint(assistantIdStr, 10, 64)
	if err != nil {
		cApi.logger.Errorf("Invalid assistantId: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid assistantId"})
		return
	}

	var req SIPCallWebhookRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		cApi.logger.Errorf("Invalid SIP webhook request: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	// Extract SIP config from request or use defaults
	sipConfig, err := GetSIPConfigFromDeployment(req.SIPConfig)
	if err != nil {
		cApi.logger.Errorf("Invalid SIP config: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid SIP configuration"})
		return
	}

	// Handle the incoming call
	if err := cApi.SIPCallReceiver(c.Request.Context(), auth, assistantId, req.From, sipConfig); err != nil {
		cApi.logger.Errorf("Failed to handle SIP call: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to handle call"})
		return
	}

	cApi.logger.Infof("SIP call webhook received: assistant=%d, from=%s, callId=%s",
		assistantId, req.From, req.CallID)

	c.JSON(http.StatusOK, gin.H{
		"status":  "accepted",
		"call_id": req.CallID,
	})
}

// SIPEventWebhook handles SIP event webhooks (hangup, dtmf, etc.)
// POST /v1/talk/sip/event/:assistantId/:conversationId
func (cApi *ConversationApi) SIPEventWebhook(c *gin.Context) {
	auth, isAuthenticated := types.GetAuthPrinciple(c)
	if !isAuthenticated {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthenticated request"})
		return
	}

	assistantIdStr := c.Param("assistantId")
	assistantId, err := strconv.ParseUint(assistantIdStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid assistantId"})
		return
	}

	conversationIdStr := c.Param("conversationId")
	conversationId, err := strconv.ParseUint(conversationIdStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid conversationId"})
		return
	}

	var req SIPEventWebhookRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		cApi.logger.Errorf("Invalid SIP event webhook: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request body"})
		return
	}

	cApi.logger.Infof("SIP event webhook: assistant=%d, conversation=%d, event=%s",
		assistantId, conversationId, req.EventType)

	// Process event based on type
	switch req.EventType {
	case "hangup", "bye":
		// Apply end metrics
		cApi.assistantConversationService.ApplyConversationMetrics(c, auth, assistantId, conversationId,
			[]*types.Metric{{Name: type_enums.STATUS.String(), Value: type_enums.RECORD_COMPLETE.String(), Description: "SIP call ended"}})
	case "answered":
		// Apply connected metrics
		cApi.assistantConversationService.ApplyConversationMetrics(c, auth, assistantId, conversationId,
			[]*types.Metric{{Name: "sip_answered", Value: "true", Description: "SIP call answered"}})
	}

	c.JSON(http.StatusOK, gin.H{"status": "processed"})
}
