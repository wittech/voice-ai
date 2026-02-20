// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package internal_asterisk_telephony

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/rapidaai/api/assistant-api/config"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/types"
	"github.com/rapidaai/pkg/utils"
	"github.com/rapidaai/protos"
)

const asteriskProvider = "asterisk"

// asteriskTelephony implements the Telephony interface for Asterisk
type asteriskTelephony struct {
	appCfg *config.AssistantConfig
	logger commons.Logger
}

// NewAsteriskTelephony creates a new Asterisk telephony provider
func NewAsteriskTelephony(config *config.AssistantConfig, logger commons.Logger) (internal_type.Telephony, error) {
	return &asteriskTelephony{
		appCfg: config,
		logger: logger,
	}, nil
}

// StatusCallback handles ARI status callback events from Asterisk.
// Asterisk ARI sends JSON payloads with a "type" field indicating the event type.
func (apt *asteriskTelephony) StatusCallback(
	c *gin.Context,
	auth types.SimplePrinciple,
	assistantId uint64,
	assistantConversationId uint64,
) (*internal_type.StatusInfo, error) {
	var eventDetails map[string]interface{}
	if err := c.ShouldBindJSON(&eventDetails); err != nil {
		apt.logger.Errorf("failed to parse ARI event body: %+v", err)
		return nil, fmt.Errorf("failed to parse ARI event body: %w", err)
	}

	eventType := "unknown"
	if v, ok := eventDetails["type"]; ok {
		eventType = fmt.Sprintf("%v", v)
	}

	return &internal_type.StatusInfo{Event: eventType, Payload: eventDetails}, nil
}

// CatchAllStatusCallback handles catch-all status callbacks
func (apt *asteriskTelephony) CatchAllStatusCallback(ctx *gin.Context) (*internal_type.StatusInfo, error) {
	return nil, nil
}

// ReceiveCall handles incoming call webhooks from Asterisk.
// The caller number is passed as the `from` query parameter by the Asterisk dialplan.
// Channel ID (if present) is captured as ChannelUUID.
func (apt *asteriskTelephony) ReceiveCall(c *gin.Context) (*internal_type.CallInfo, error) {
	clientNumber := c.Query("from")
	if clientNumber == "" {
		clientNumber = c.Query("callerid")
	}
	if clientNumber == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing caller information — provide 'from' query parameter"})
		return nil, fmt.Errorf("missing caller information in query params")
	}

	info := &internal_type.CallInfo{
		CallerNumber: clientNumber,
		Provider:     asteriskProvider,
		Status:       "SUCCESS",
		StatusInfo:   internal_type.StatusInfo{Event: "webhook", Payload: map[string]string{"from": clientNumber}},
	}
	if channelID := c.Query("channel_id"); channelID != "" {
		info.ChannelUUID = channelID
	}
	return info, nil
}

// OutboundCall initiates an outbound call via Asterisk ARI
func (apt *asteriskTelephony) OutboundCall(
	auth types.SimplePrinciple,
	toPhone string,
	fromPhone string,
	assistantId, assistantConversationId uint64,
	vaultCredential *protos.VaultCredential,
	opts utils.Option,
) (*internal_type.CallInfo, error) {
	info := &internal_type.CallInfo{Provider: asteriskProvider}

	if vaultCredential == nil {
		info.Status = "FAILED"
		info.ErrorMessage = "Missing vault credential for Asterisk ARI"
		return info, fmt.Errorf("missing vault credential for Asterisk ARI")
	}
	credMap := vaultCredential.GetValue().AsMap()
	ariURL, _ := credMap["ari_url"].(string)
	ariURL = fmt.Sprintf("%s/ari/channels", ariURL)
	params := url.Values{}
	params.Set("endpoint", fmt.Sprintf("%s", toPhone))

	if ctxVal, err := opts.GetString("context"); err == nil && ctxVal != "" {
		params.Set("context", ctxVal)
	}

	if extVal, err := opts.GetString("extension"); err == nil && extVal != "" {
		params.Set("extension", extVal)
	}
	callerId := fromPhone
	if callerIdVal, err := opts.GetString("caller_id"); err == nil && callerIdVal != "" {
		callerId = callerIdVal
	}

	params.Set("priority", "1")
	params.Set("callerId", callerId)
	params.Set("appArgs", fmt.Sprintf("incoming,assistant_id=%d,conversation_id=%d", assistantId, assistantConversationId))

	// Pass contextId as a channel variable — Asterisk dialplan uses this as the AudioSocket UUID
	// so the AudioSocket server can resolve the full call context from Redis.
	// All other call details (assistant, conversation, auth, org, project) are resolved
	// from Redis via the contextId — no need to pass them as separate channel variables.
	if contextID, err := opts.GetString("rapida.context_id"); err == nil && contextID != "" {
		params.Add("variables", fmt.Sprintf("RAPIDA_CONTEXT_ID=%s", contextID))
	}

	// Create HTTP request
	reqURL := fmt.Sprintf("%s?%s", ariURL, params.Encode())
	req, err := http.NewRequest("POST", reqURL, nil)
	if err != nil {
		info.Status = "FAILED"
		info.ErrorMessage = fmt.Sprintf("request creation error: %s", err.Error())
		return info, err
	}

	user, _ := credMap["ari_user"].(string)
	password, _ := credMap["ari_password"].(string)
	// Set authentication
	req.SetBasicAuth(user, password)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		info.Status = "FAILED"
		info.ErrorMessage = fmt.Sprintf("API error: %s", err.Error())
		return info, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		info.Status = "FAILED"
		info.ErrorMessage = fmt.Sprintf("API returned status: %d", resp.StatusCode)
		return info, fmt.Errorf("ARI API returned status: %d", resp.StatusCode)
	}

	// Parse response
	var ariResp map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&ariResp); err != nil {
		apt.logger.Warn("Failed to decode ARI response", "error", err)
	}

	if id, ok := ariResp["id"]; ok {
		info.ChannelUUID = fmt.Sprintf("%v", id)
	}

	info.Status = "SUCCESS"
	info.StatusInfo = internal_type.StatusInfo{Event: "channel_created", Payload: ariResp}
	return info, nil
}

// InboundCall handles inbound call setup for Asterisk.
// Returns the contextId as plain text — Asterisk dialplan uses this as the AudioSocket UUID
// so the AudioSocket server can resolve the full call context from Redis.
//
// For AudioSocket: same = n,AudioSocket(${CURL(https://api.rapida.ai/v1/talk/asterisk/call/${ASSISTANT_ID})},host:port)
// For chan_websocket: the contextId is used in the WS URL path: wss://host/v1/talk/asterisk/ctx/${contextId}
func (apt *asteriskTelephony) InboundCall(
	c *gin.Context,
	auth types.SimplePrinciple,
	assistantId uint64,
	clientNumber string,
	assistantConversationId uint64,
) error {
	// contextId was set by CallReciever after saving the call context to Redis.
	// Return it as plain text — Asterisk dialplan uses this as the AudioSocket UUID
	// or as part of the WebSocket URL path for chan_websocket.
	contextID, exists := c.Get("contextId")
	if !exists || contextID == "" {
		return fmt.Errorf("missing contextId — CallReciever must save call context before InboundCall")
	}
	c.String(http.StatusOK, fmt.Sprintf("%v", contextID))
	return nil
}
