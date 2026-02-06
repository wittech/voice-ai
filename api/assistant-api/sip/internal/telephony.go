// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package internal_sip

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/rapidaai/api/assistant-api/config"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/types"
	"github.com/rapidaai/pkg/utils"
	"github.com/rapidaai/protos"
)

// sipTelephony implements the Telephony interface for native SIP
type sipTelephony struct {
	mu      sync.RWMutex
	appCfg  *config.AssistantConfig
	logger  commons.Logger
	servers map[string]*Server
}

// NewSIPTelephony creates a new SIP telephony provider
func NewSIPTelephony(cfg *config.AssistantConfig, logger commons.Logger) (internal_type.Telephony, error) {
	return &sipTelephony{
		appCfg:  cfg,
		logger:  logger,
		servers: make(map[string]*Server),
	}, nil
}

// ParseConfig parses SIP configuration from vault credentials
func ParseConfig(vaultCredential *protos.VaultCredential) (*Config, error) {
	if vaultCredential == nil || vaultCredential.GetValue() == nil {
		return nil, fmt.Errorf("vault credential is required")
	}

	credMap := vaultCredential.GetValue().AsMap()

	cfg := DefaultConfig()

	// Required fields
	if server, ok := credMap["sip_server"].(string); ok {
		cfg.Server = server
	}
	if username, ok := credMap["sip_username"].(string); ok {
		cfg.Username = username
	}
	if password, ok := credMap["sip_password"].(string); ok {
		cfg.Password = password
	}

	// Optional fields with defaults
	if port, ok := credMap["sip_port"].(float64); ok {
		cfg.Port = int(port)
	}
	if transport, ok := credMap["sip_transport"].(string); ok {
		cfg.Transport = Transport(transport)
	}
	if realm, ok := credMap["sip_realm"].(string); ok {
		cfg.Realm = realm
	}
	if domain, ok := credMap["sip_domain"].(string); ok {
		cfg.Domain = domain
	}
	if rtpStart, ok := credMap["rtp_port_range_start"].(float64); ok {
		cfg.RTPPortRangeStart = int(rtpStart)
	}
	if rtpEnd, ok := credMap["rtp_port_range_end"].(float64); ok {
		cfg.RTPPortRangeEnd = int(rtpEnd)
	}
	if srtp, ok := credMap["srtp_enabled"].(bool); ok {
		cfg.SRTPEnabled = srtp
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// getOrCreateServer gets an existing server or creates a new one for the tenant
func (t *sipTelephony) getOrCreateServer(ctx context.Context, tenantID string, cfg *Config) (*Server, error) {
	t.mu.RLock()
	server, exists := t.servers[tenantID]
	t.mu.RUnlock()

	if exists && server.IsRunning() {
		return server, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Double-check after acquiring write lock
	if server, exists := t.servers[tenantID]; exists && server.IsRunning() {
		return server, nil
	}

	// Create ListenConfig from tenant config
	listenConfig := &ListenConfig{
		Address:   cfg.Server,
		Port:      cfg.Port,
		Transport: cfg.Transport,
	}

	// Config resolver returns the tenant config for all calls on this server
	tenantConfig := cfg
	configResolver := func(inviteCtx *InviteContext) (*InviteResult, error) {
		return &InviteResult{
			Config:      tenantConfig,
			ShouldAllow: true,
		}, nil
	}

	// Create new server
	newServer, err := NewServer(ctx, &ServerConfig{
		ListenConfig:   listenConfig,
		ConfigResolver: configResolver,
		Logger:         t.logger,
	})
	if err != nil {
		return nil, err
	}

	t.servers[tenantID] = newServer
	return newServer, nil
}

// StatusCallback handles status callbacks from SIP events
func (t *sipTelephony) StatusCallback(
	c *gin.Context,
	auth types.SimplePrinciple,
	assistantId uint64,
	assistantConversationId uint64,
) ([]types.Telemetry, error) {
	body, err := c.GetRawData()
	if err != nil {
		t.logger.Error("Failed to read SIP status callback body", "error", err)
		return nil, fmt.Errorf("failed to read request body")
	}

	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.logger.Error("Failed to parse SIP status callback", "error", err)
		return nil, fmt.Errorf("failed to parse request body")
	}

	// Extract event type
	eventType, _ := payload["event"].(string)
	callID, _ := payload["call_id"].(string)

	t.logger.Debug("SIP status callback received",
		"event", eventType,
		"call_id", callID,
		"assistant_id", assistantId,
		"conversation_id", assistantConversationId)

	return []types.Telemetry{
		types.NewMetric("STATUS", eventType, utils.Ptr("SIP event status")),
		types.NewEvent(eventType, payload),
	}, nil
}

// CatchAllStatusCallback handles catch-all status callbacks
func (t *sipTelephony) CatchAllStatusCallback(ctx *gin.Context) ([]types.Telemetry, error) {
	return nil, nil
}

// OutboundCall initiates an outbound SIP call
func (t *sipTelephony) OutboundCall(
	auth types.SimplePrinciple,
	toPhone string,
	fromPhone string,
	assistantId, assistantConversationId uint64,
	vaultCredential *protos.VaultCredential,
	opts utils.Option,
) ([]types.Telemetry, error) {
	mtds := []types.Telemetry{
		types.NewMetadata("telephony.toPhone", toPhone),
		types.NewMetadata("telephony.fromPhone", fromPhone),
		types.NewMetadata("telephony.provider", "sip"),
	}

	cfg, err := ParseConfig(vaultCredential)
	if err != nil {
		return append(mtds,
			types.NewMetadata("telephony.error", fmt.Sprintf("config error: %s", err.Error())),
			types.NewMetric("STATUS", "FAILED", utils.Ptr("Status of telephony api")),
		), err
	}

	// Get or create server for this tenant
	tenantID := fmt.Sprintf("%d", assistantId)
	server, err := t.getOrCreateServer(context.Background(), tenantID, cfg)
	if err != nil {
		return append(mtds,
			types.NewMetadata("telephony.error", fmt.Sprintf("server error: %s", err.Error())),
			types.NewMetric("STATUS", "FAILED", utils.Ptr("Status of telephony api")),
		), err
	}

	// Start server if not running
	if !server.IsRunning() {
		if err := server.Start(); err != nil {
			return append(mtds,
				types.NewMetadata("telephony.error", fmt.Sprintf("server start error: %s", err.Error())),
				types.NewMetric("STATUS", "FAILED", utils.Ptr("Status of telephony api")),
			), err
		}
	}

	// Note: Actual outbound call initiation requires INVITE sending
	// This is a placeholder for the full implementation
	t.logger.Info("SIP outbound call initiated",
		"to", toPhone,
		"from", fromPhone,
		"assistant_id", assistantId,
		"conversation_id", assistantConversationId)

	return append(mtds,
		types.NewMetadata("telephony.status", "initiated"),
		types.NewEvent("initiated", map[string]interface{}{
			"to":              toPhone,
			"from":            fromPhone,
			"assistant_id":    assistantId,
			"conversation_id": assistantConversationId,
		}),
		types.NewMetric("STATUS", "SUCCESS", utils.Ptr("Status of telephony api")),
	), nil
}

// InboundCall handles incoming SIP calls
func (t *sipTelephony) InboundCall(
	c *gin.Context,
	auth types.SimplePrinciple,
	assistantId uint64,
	clientNumber string,
	assistantConversationId uint64,
) error {
	// For native SIP, inbound calls are handled directly by the SIP server
	// This endpoint just returns a confirmation
	c.JSON(http.StatusOK, gin.H{
		"status":          "ready",
		"assistant_id":    assistantId,
		"conversation_id": assistantConversationId,
		"client_number":   clientNumber,
		"message":         "SIP inbound call ready - connect via SIP signaling",
	})
	return nil
}

// ReceiveCall processes incoming call webhook data
func (t *sipTelephony) ReceiveCall(c *gin.Context) (*string, []types.Telemetry, error) {
	queryParams := make(map[string]string)
	telemetry := []types.Telemetry{}

	for key, values := range c.Request.URL.Query() {
		if len(values) > 0 {
			queryParams[key] = values[0]
		}
	}

	// Extract caller information
	clientNumber, ok := queryParams["from"]
	if !ok || clientNumber == "" {
		// Try alternative parameter names
		clientNumber, ok = queryParams["caller"]
		if !ok || clientNumber == "" {
			return nil, telemetry, fmt.Errorf("missing caller information")
		}
	}

	if callID, ok := queryParams["call_id"]; ok && callID != "" {
		telemetry = append(telemetry, types.NewMetadata("telephony.uuid", callID))
	}

	return utils.Ptr(clientNumber), append(telemetry,
		types.NewEvent("webhook", queryParams),
		types.NewMetric("STATUS", "SUCCESS", utils.Ptr("Status of telephony api")),
	), nil
}

// StopServer stops a specific tenant's SIP server
func (t *sipTelephony) StopServer(tenantID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if server, exists := t.servers[tenantID]; exists {
		server.Stop()
		delete(t.servers, tenantID)
	}
}

// StopAllServers stops all SIP servers
func (t *sipTelephony) StopAllServers() {
	t.mu.Lock()
	defer t.mu.Unlock()

	for tenantID, server := range t.servers {
		server.Stop()
		delete(t.servers, tenantID)
	}
}
