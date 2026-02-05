// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package sip

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Session manages a single SIP call session
type Session struct {
	mu sync.RWMutex

	info         SessionInfo
	config       *Config
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
	negotiatedCodec string
	sampleRate      int
}

// NewSession creates a new SIP session
func NewSession(ctx context.Context, config *Config, direction string) (*Session, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid SIP config: %w", err)
	}

	sessionCtx, cancel := context.WithCancel(ctx)

	session := &Session{
		info: SessionInfo{
			CallID:    uuid.New().String(),
			LocalTag:  uuid.New().String()[:8],
			State:     CallStateInitializing,
			Direction: direction,
			StartTime: time.Now(),
		},
		config:       config,
		ctx:          sessionCtx,
		cancel:       cancel,
		audioInChan:  make(chan []byte, 100),
		audioOutChan: make(chan []byte, 100),
		eventChan:    make(chan Event, 50),
		errorChan:    make(chan error, 10),
	}

	return session, nil
}

// GetInfo returns the current session information
func (s *Session) GetInfo() SessionInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info
}

// SetState updates the session state
func (s *Session) SetState(state CallState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.info.State = state

	switch state {
	case CallStateConnected:
		now := time.Now()
		s.info.ConnectedTime = &now
	case CallStateEnded, CallStateFailed:
		now := time.Now()
		s.info.EndTime = &now
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
func (s *Session) SetNegotiatedCodec(codec string, sampleRate int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.negotiatedCodec = codec
	s.sampleRate = sampleRate
	s.info.Codec = codec
}

// GetNegotiatedCodec returns the negotiated codec info
func (s *Session) GetNegotiatedCodec() (string, int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.negotiatedCodec, s.sampleRate
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
	select {
	case s.audioOutChan <- data:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
		return fmt.Errorf("audio output buffer full")
	}
}

// ReceiveAudio receives audio data from the remote endpoint
func (s *Session) ReceiveAudio() ([]byte, error) {
	select {
	case data := <-s.audioInChan:
		return data, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

// SendEvent sends an event notification
func (s *Session) SendEvent(event Event) {
	select {
	case s.eventChan <- event:
	default:
	}
}

// End terminates the SIP session
func (s *Session) End() {
	s.SetState(CallStateEnding)
	s.cancel()
	close(s.audioInChan)
	close(s.audioOutChan)
	close(s.eventChan)
	close(s.errorChan)
	s.SetState(CallStateEnded)
}

// IsActive returns whether the session is still active
func (s *Session) IsActive() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.info.State != CallStateEnded && s.info.State != CallStateFailed
}
