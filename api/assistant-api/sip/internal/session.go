// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package internal_sip

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/rapidaai/pkg/commons"
)

// Session channel buffer sizes
const (
	audioInBufferSize  = 100
	audioOutBufferSize = 100
	eventBufferSize    = 50
	errorBufferSize    = 10
)

// SessionConfig holds configuration for creating a session
type SessionConfig struct {
	Config    *Config
	Direction CallDirection
	CallID    string // Optional: if empty, a new UUID will be generated
	Codec     *Codec
	Logger    commons.Logger
}

// Session manages a single SIP call session
type Session struct {
	mu     sync.RWMutex
	logger commons.Logger

	info   SessionInfo
	config *Config
	ended  atomic.Bool

	ctx          context.Context
	cancel       context.CancelFunc
	audioInChan  chan []byte
	audioOutChan chan []byte
	eventChan    chan Event
	errorChan    chan error

	// RTP handling
	rtpHandler    *RTPHandler
	rtpLocalPort  int
	rtpRemoteAddr string
	rtpRemotePort int

	// Codec negotiation result
	negotiatedCodec *Codec
}

// NewSession creates a new SIP session
func NewSession(ctx context.Context, cfg *SessionConfig) (*Session, error) {
	if cfg.Config == nil {
		return nil, fmt.Errorf("%w: config is required", ErrInvalidConfig)
	}
	if err := cfg.Config.Validate(); err != nil {
		return nil, err
	}

	sessionCtx, cancel := context.WithCancel(ctx)

	callID := cfg.CallID
	if callID == "" {
		callID = uuid.New().String()
	}

	codec := cfg.Codec
	if codec == nil {
		codec = &CodecPCMU
	}

	session := &Session{
		logger: cfg.Logger,
		info: SessionInfo{
			CallID:     callID,
			LocalTag:   uuid.New().String()[:8],
			State:      CallStateInitializing,
			Direction:  cfg.Direction,
			StartTime:  time.Now(),
			Codec:      codec.Name,
			SampleRate: int(codec.ClockRate),
		},
		config:          cfg.Config,
		ctx:             sessionCtx,
		cancel:          cancel,
		audioInChan:     make(chan []byte, audioInBufferSize),
		audioOutChan:    make(chan []byte, audioOutBufferSize),
		eventChan:       make(chan Event, eventBufferSize),
		errorChan:       make(chan error, errorBufferSize),
		negotiatedCodec: codec,
	}

	return session, nil
}

// GetInfo returns the current session information
func (s *Session) GetInfo() SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	info := s.info
	info.Duration = info.GetDuration()
	return info
}

// GetCallID returns the call ID
func (s *Session) GetCallID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info.CallID
}

// SetState updates the session state with proper state machine transitions
func (s *Session) SetState(state CallState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	previousState := s.info.State

	// Validate state transitions
	if !s.isValidTransition(previousState, state) {
		if s.logger != nil {
			s.logger.Warn("Invalid state transition",
				"call_id", s.info.CallID,
				"from", previousState,
				"to", state)
		}
		return
	}

	s.info.State = state

	switch state {
	case CallStateConnected:
		now := time.Now()
		s.info.ConnectedTime = &now
		s.emitEvent(EventTypeConnected, nil)
	case CallStateEnded:
		now := time.Now()
		s.info.EndTime = &now
		s.emitEvent(EventTypeBye, nil)
	case CallStateFailed:
		now := time.Now()
		s.info.EndTime = &now
		s.emitEvent(EventTypeError, nil)
	case CallStateRinging:
		s.emitEvent(EventTypeRinging, nil)
	}

	if s.logger != nil {
		s.logger.Debug("Session state changed",
			"call_id", s.info.CallID,
			"from", previousState,
			"to", state)
	}
}

// isValidTransition checks if a state transition is valid
func (s *Session) isValidTransition(from, to CallState) bool {
	// Allow any transition to ended/failed
	if to == CallStateEnded || to == CallStateFailed {
		return true
	}

	// Prevent transitions from terminal states
	if from.IsTerminal() {
		return false
	}

	// Define valid transitions
	validTransitions := map[CallState][]CallState{
		CallStateInitializing: {CallStateRinging, CallStateConnected},
		CallStateRinging:      {CallStateConnected, CallStateEnding},
		CallStateConnected:    {CallStateOnHold, CallStateEnding},
		CallStateOnHold:       {CallStateConnected, CallStateEnding},
		CallStateEnding:       {CallStateEnded},
	}

	allowed, exists := validTransitions[from]
	if !exists {
		return false
	}

	for _, validTo := range allowed {
		if validTo == to {
			return true
		}
	}
	return false
}

// emitEvent sends an event to the event channel (non-blocking)
func (s *Session) emitEvent(eventType EventType, data map[string]interface{}) {
	event := NewEvent(eventType, s.info.CallID, data)
	select {
	case s.eventChan <- event:
	default:
		// Channel full, drop event
	}
}

// SetRemoteRTP sets the remote RTP address after SDP negotiation
func (s *Session) SetRemoteRTP(addr string, port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rtpRemoteAddr = addr
	s.rtpRemotePort = port
	s.info.RemoteRTPAddress = fmt.Sprintf("%s:%d", addr, port)
}

// SetLocalRTP sets the local RTP address
func (s *Session) SetLocalRTP(addr string, port int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rtpLocalPort = port
	s.info.LocalRTPAddress = fmt.Sprintf("%s:%d", addr, port)
}

// SetNegotiatedCodec sets the negotiated codec
func (s *Session) SetNegotiatedCodec(codecName string, sampleRate int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	codec := GetCodecByName(codecName)
	if codec == nil {
		codec = &CodecPCMU
	}
	s.negotiatedCodec = codec
	s.info.Codec = codec.Name
	s.info.SampleRate = sampleRate
}

// GetNegotiatedCodec returns the negotiated codec
func (s *Session) GetNegotiatedCodec() *Codec {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.negotiatedCodec
}

// SetRTPHandler sets the RTP handler for this session
func (s *Session) SetRTPHandler(handler *RTPHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rtpHandler = handler
}

// GetRTPHandler returns the RTP handler for this session
func (s *Session) GetRTPHandler() *RTPHandler {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rtpHandler
}

// AudioIn returns the channel for receiving audio from remote
func (s *Session) AudioIn() <-chan []byte {
	return s.audioInChan
}

// AudioOut returns the channel for sending audio to remote
func (s *Session) AudioOut() chan<- []byte {
	return s.audioOutChan
}

// Events returns the event channel
func (s *Session) Events() <-chan Event {
	return s.eventChan
}

// Errors returns the error channel
func (s *Session) Errors() <-chan error {
	return s.errorChan
}

// Context returns the session context
func (s *Session) Context() context.Context {
	return s.ctx
}

// SendAudio sends audio data to the remote endpoint via RTP
func (s *Session) SendAudio(data []byte) error {
	if s.ended.Load() {
		return ErrSessionClosed
	}

	select {
	case s.audioOutChan <- data:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
		return ErrBufferFull
	}
}

// ReceiveAudio receives audio data from the remote endpoint
func (s *Session) ReceiveAudio() ([]byte, error) {
	if s.ended.Load() {
		return nil, ErrSessionClosed
	}

	select {
	case data, ok := <-s.audioInChan:
		if !ok {
			return nil, ErrSessionClosed
		}
		return data, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

// SendEvent sends an event notification (non-blocking)
func (s *Session) SendEvent(event Event) {
	if s.ended.Load() {
		return
	}
	select {
	case s.eventChan <- event:
	default:
		// Event dropped if channel is full
	}
}

// SendError sends an error to the error channel (non-blocking)
func (s *Session) SendError(err error) {
	if s.ended.Load() {
		return
	}
	select {
	case s.errorChan <- err:
	default:
		// Error dropped if channel is full
	}
}

// End terminates the SIP session gracefully
func (s *Session) End() {
	// Use atomic to ensure End is only called once
	if !s.ended.CompareAndSwap(false, true) {
		return // Already ended
	}

	s.SetState(CallStateEnding)

	// Stop RTP handler if present
	s.mu.Lock()
	rtpHandler := s.rtpHandler
	s.rtpHandler = nil
	s.mu.Unlock()

	if rtpHandler != nil {
		if err := rtpHandler.Stop(); err != nil && s.logger != nil {
			s.logger.Warn("Error stopping RTP handler", "error", err, "call_id", s.info.CallID)
		}
	}

	// Cancel context
	s.cancel()

	// Close channels safely
	s.closeChannels()

	s.SetState(CallStateEnded)

	if s.logger != nil {
		s.logger.Info("Session ended",
			"call_id", s.info.CallID,
			"duration", s.info.GetDuration())
	}
}

// closeChannels safely closes all session channels
func (s *Session) closeChannels() {
	defer func() {
		// Recover from panic if channel is already closed
		recover()
	}()

	close(s.audioInChan)
	close(s.audioOutChan)
	close(s.eventChan)
	close(s.errorChan)
}

// IsActive returns whether the session is still active
func (s *Session) IsActive() bool {
	if s.ended.Load() {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info.State.IsActive()
}

// IsEnded returns whether the session has ended
func (s *Session) IsEnded() bool {
	return s.ended.Load()
}

// GetState returns the current session state
func (s *Session) GetState() CallState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info.State
}

// GetRTPStats returns RTP statistics if available
func (s *Session) GetRTPStats() *RTPStats {
	s.mu.RLock()
	rtpHandler := s.rtpHandler
	s.mu.RUnlock()

	if rtpHandler == nil {
		return nil
	}

	sent, received := rtpHandler.GetStats()
	return &RTPStats{
		PacketsSent:     sent,
		PacketsReceived: received,
	}
}
