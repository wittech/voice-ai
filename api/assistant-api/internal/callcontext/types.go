// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package internal_callcontext

import (
	"fmt"
	"strconv"

	"github.com/rapidaai/pkg/types"
	"github.com/rapidaai/pkg/utils"
)

const redisKeyPrefix = "call:ctx:"

// CallContext holds all the information needed to resolve a call session.
// It bridges the gap between the HTTP call-setup request (inbound webhook or outbound gRPC)
// and the AudioSocket/WebSocket connection that follows.
type CallContext struct {
	ContextID      string `mapstructure:"context_id"`
	AssistantID    uint64 `mapstructure:"assistant_id"`
	ConversationID uint64 `mapstructure:"conversation_id"`
	ProjectID      uint64 `mapstructure:"project_id"`
	OrganizationID uint64 `mapstructure:"organization_id"`
	AuthToken      string `mapstructure:"auth_token"`
	AuthType       string `mapstructure:"auth_type"`
	Provider       string `mapstructure:"provider"`
	Direction      string `mapstructure:"direction"`
	CallerNumber   string `mapstructure:"caller_number"`
	CalleeNumber   string `mapstructure:"callee_number"`
	FromNumber     string `mapstructure:"from_number"`
	Status         string `mapstructure:"status"`

	// AssistantProviderId is the version identifier for the assistant provider.
	// Stored so that streamer can build AssistantDefinition without re-fetching from DB.
	AssistantProviderId uint64 `mapstructure:"assistant_provider_id"`

	// ChannelUUID is the provider-specific call identifier (Twilio CallSid, Vonage UUID,
	// Asterisk channel ID, SIP Call-ID, etc.). Stored so that any telephony operation
	// (transfer, disconnect, hold) can reference the live call on the provider.
	ChannelUUID string `mapstructure:"channel_uuid"`
}

// RedisKey returns the Redis key for this call context instance.
func (cc *CallContext) RedisKey() string {
	return redisKeyPrefix + cc.ContextID
}

// RedisKey returns the Redis key for a given contextId (package-level helper).
func RedisKey(contextID string) string {
	return redisKeyPrefix + contextID
}

// ToHashFields converts the CallContext to a map suitable for Redis HSET.
func (cc *CallContext) ToHashFields() map[string]string {
	fields := map[string]string{
		"context_id":            cc.ContextID,
		"assistant_id":          strconv.FormatUint(cc.AssistantID, 10),
		"conversation_id":       strconv.FormatUint(cc.ConversationID, 10),
		"project_id":            strconv.FormatUint(cc.ProjectID, 10),
		"organization_id":       strconv.FormatUint(cc.OrganizationID, 10),
		"auth_token":            cc.AuthToken,
		"auth_type":             cc.AuthType,
		"provider":              cc.Provider,
		"direction":             cc.Direction,
		"caller_number":         cc.CallerNumber,
		"callee_number":         cc.CalleeNumber,
		"from_number":           cc.FromNumber,
		"status":                cc.Status,
		"assistant_provider_id": strconv.FormatUint(cc.AssistantProviderId, 10),
		"channel_uuid":          cc.ChannelUUID,
	}
	return fields
}

// fromHashFields reconstructs a CallContext from a Redis HGETALL map[string]string result.
// This is the inverse of ToHashFields and avoids the fragile generic Cmd→ResultStruct pipeline.
func fromHashFields(fields map[string]string) (*CallContext, error) {
	cc := &CallContext{
		ContextID:    fields["context_id"],
		AuthToken:    fields["auth_token"],
		AuthType:     fields["auth_type"],
		Provider:     fields["provider"],
		Direction:    fields["direction"],
		CallerNumber: fields["caller_number"],
		CalleeNumber: fields["callee_number"],
		FromNumber:   fields["from_number"],
		Status:       fields["status"],
		ChannelUUID:  fields["channel_uuid"],
	}

	if cc.ContextID == "" {
		return nil, fmt.Errorf("call context has empty context_id")
	}

	if v := fields["assistant_id"]; v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid assistant_id %q: %w", v, err)
		}
		cc.AssistantID = n
	}
	if v := fields["conversation_id"]; v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid conversation_id %q: %w", v, err)
		}
		cc.ConversationID = n
	}
	if v := fields["project_id"]; v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid project_id %q: %w", v, err)
		}
		cc.ProjectID = n
	}
	if v := fields["organization_id"]; v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid organization_id %q: %w", v, err)
		}
		cc.OrganizationID = n
	}
	if v := fields["assistant_provider_id"]; v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid assistant_provider_id %q: %w", v, err)
		}
		cc.AssistantProviderId = n
	}

	return cc, nil
}

// ToAuth converts the CallContext into a SimplePrinciple for use in service calls.
func (cc *CallContext) ToAuth() types.SimplePrinciple {
	auth := &types.ServiceScope{
		CurrentToken: cc.AuthToken,
	}
	if cc.ProjectID != 0 {
		auth.ProjectId = utils.Ptr(cc.ProjectID)
	}
	if cc.OrganizationID != 0 {
		auth.OrganizationId = utils.Ptr(cc.OrganizationID)
	}
	return auth
}

// ExtractChannelUUID extracts the provider call UUID from telemetry metadata.
// All providers use the key "telephony.uuid" for the provider-specific call identifier
// (Twilio CallSid, Vonage UUID, Asterisk channel ID, SIP Call-ID, etc.).
func ExtractChannelUUID(metadatas []*types.Metadata) string {
	for _, m := range metadatas {
		if m.Key == "telephony.uuid" {
			return m.Value
		}
	}
	return ""
}
