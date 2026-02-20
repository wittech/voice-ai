// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package internal_callcontext

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/connectors"
)

const (
	// DefaultTTL is the default time-to-live for call context entries.
	// Call contexts are ephemeral — they bridge the gap between the HTTP call-setup
	// request and the AudioSocket/WebSocket connection that follows within seconds.
	DefaultTTL = 5 * time.Minute
)

// Store provides operations to save and retrieve call contexts from Redis.
type Store interface {
	// Save stores a call context in Redis with a generated contextId (UUID).
	// Returns the generated contextId.
	Save(ctx context.Context, cc *CallContext) (string, error)

	// Get retrieves a call context by contextId.
	// Returns nil if not found or expired.
	Get(ctx context.Context, contextID string) (*CallContext, error)

	// GetAndDelete atomically retrieves and deletes a call context in a single
	// Redis operation (Lua script). This prevents race conditions where two
	// concurrent media connections could both claim the same call context.
	// Returns the CallContext or an error if not found/expired.
	GetAndDelete(ctx context.Context, contextID string) (*CallContext, error)

	// Delete removes a call context (called after successful resolution).
	Delete(ctx context.Context, contextID string) error

	// UpdateField sets a single field on an existing call context hash.
	// Used to patch the channel UUID after the telephony provider returns it.
	UpdateField(ctx context.Context, contextID, field, value string) error
}

type redisStore struct {
	redis  connectors.RedisConnector
	logger commons.Logger
	ttl    time.Duration
}

// NewStore creates a new call context store backed by Redis.
func NewStore(redis connectors.RedisConnector, logger commons.Logger) Store {
	return &redisStore{
		redis:  redis,
		logger: logger,
		ttl:    DefaultTTL,
	}
}

// Save stores a call context in Redis with a generated UUID as the contextId.
// The context is stored as a Redis hash and expires after DefaultTTL.
func (s *redisStore) Save(ctx context.Context, cc *CallContext) (string, error) {
	// Generate a UUID for the contextId if not already set
	if cc.ContextID == "" {
		cc.ContextID = uuid.New().String()
	}

	key := cc.RedisKey()
	client := s.redis.GetConnection()

	// Store as hash using HSET with all fields
	fields := cc.ToHashFields()
	args := make([]interface{}, 0, len(fields)*2)
	for k, v := range fields {
		args = append(args, k, v)
	}

	if err := client.HSet(ctx, key, args...).Err(); err != nil {
		return "", fmt.Errorf("failed to save call context %s: %w", cc.ContextID, err)
	}

	// Set TTL on the hash
	if err := client.Expire(ctx, key, s.ttl).Err(); err != nil {
		s.logger.Warnf("failed to set TTL on call context %s: %v", cc.ContextID, err)
	}

	s.logger.Infof("saved call context: contextId=%s, assistant=%d, conversation=%d, direction=%s",
		cc.ContextID, cc.AssistantID, cc.ConversationID, cc.Direction)

	return cc.ContextID, nil
}

// Get retrieves a call context by contextId.
func (s *redisStore) Get(ctx context.Context, contextID string) (*CallContext, error) {
	key := RedisKey(contextID)
	client := s.redis.GetConnection()

	result, err := client.HGetAll(ctx, key).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get call context %s: %w", contextID, err)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("call context not found (key does not exist): %s", contextID)
	}

	cc, err := fromHashFields(result)
	if err != nil {
		return nil, fmt.Errorf("failed to parse call context %s: %w", contextID, err)
	}

	s.logger.Debugf("resolved call context: contextId=%s, assistant=%d, conversation=%d",
		cc.ContextID, cc.AssistantID, cc.ConversationID)

	return cc, nil
}

// GetAndDelete atomically retrieves and deletes a call context using a Lua script.
// Only one caller can claim a given context — subsequent calls get an empty result.
func (s *redisStore) GetAndDelete(ctx context.Context, contextID string) (*CallContext, error) {
	key := RedisKey(contextID)
	client := s.redis.GetConnection()

	// Lua script: HGETALL + DEL in a single atomic operation.
	// Returns the flat array of field-value pairs from HGETALL.
	script := `local d=redis.call('HGETALL',KEYS[1]) if #d>0 then redis.call('DEL',KEYS[1]) end return d`
	raw, err := client.Eval(ctx, script, []string{key}).Result()
	if err != nil {
		return nil, fmt.Errorf("failed to get-and-delete call context %s: %w", contextID, err)
	}

	// Eval returns []interface{} for the flat HGETALL array
	pairs, ok := raw.([]interface{})
	if !ok || len(pairs) == 0 {
		return nil, fmt.Errorf("call context not found (key does not exist): %s", contextID)
	}

	// Convert flat pairs to map[string]string
	result := make(map[string]string, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		k, _ := pairs[i].(string)
		v, _ := pairs[i+1].(string)
		result[k] = v
	}

	cc, err := fromHashFields(result)
	if err != nil {
		return nil, fmt.Errorf("failed to parse call context %s: %w", contextID, err)
	}

	s.logger.Debugf("resolved and deleted call context: contextId=%s, assistant=%d, conversation=%d",
		cc.ContextID, cc.AssistantID, cc.ConversationID)

	return cc, nil
}

// Delete removes a call context from Redis.
// Should be called after the AudioSocket/WebSocket connection has successfully resolved the context.
func (s *redisStore) Delete(ctx context.Context, contextID string) error {
	key := RedisKey(contextID)
	client := s.redis.GetConnection()

	if err := client.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("failed to delete call context %s: %w", contextID, err)
	}

	s.logger.Debugf("deleted call context: contextId=%s", contextID)
	return nil
}

// UpdateField sets a single field on an existing call context hash in Redis.
func (s *redisStore) UpdateField(ctx context.Context, contextID, field, value string) error {
	key := RedisKey(contextID)
	client := s.redis.GetConnection()

	if err := client.HSet(ctx, key, field, value).Err(); err != nil {
		return fmt.Errorf("failed to update field %s on call context %s: %w", field, contextID, err)
	}

	s.logger.Debugf("updated call context field: contextId=%s, %s=%s", contextID, field, value)
	return nil
}
