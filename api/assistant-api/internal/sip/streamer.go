// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package sip

import (
	"context"
	"fmt"
	"io"
	"sync"

	internal_audio "github.com/rapidaai/api/assistant-api/internal/audio"
	internal_assistant_entity "github.com/rapidaai/api/assistant-api/internal/entity/assistants"
	internal_conversation_entity "github.com/rapidaai/api/assistant-api/internal/entity/conversations"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/protos"
)

// Streamer implements the Streamer interface using native SIP signaling and RTP
// No WebSocket needed - uses sipgo for signaling, RTP/UDP for audio
type Streamer struct {
	mu sync.RWMutex

	logger     commons.Logger
	config     *Config
	session    *Session
	server     *Server
	rtpHandler *RTPHandler

	assistant    *internal_assistant_entity.Assistant
	conversation *internal_conversation_entity.AssistantConversation

	codec      string
	sampleRate int

	ctx    context.Context
	cancel context.CancelFunc

	inputBuffer  []byte
	outputBuffer []byte
}

// StreamerConfig holds configuration for creating a SIP streamer
type StreamerConfig struct {
	Config       *Config
	Logger       commons.Logger
	TenantID     string
	Assistant    *internal_assistant_entity.Assistant
	Conversation *internal_conversation_entity.AssistantConversation
}

// NewStreamer creates a new native SIP streamer
// Uses sipgo for SIP signaling and RTP for audio transport - no WebSocket needed
func NewStreamer(ctx context.Context, cfg *StreamerConfig) (internal_type.Streamer, error) {
	if err := cfg.Config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid SIP config: %w", err)
	}

	streamerCtx, cancel := context.WithCancel(ctx)

	s := &Streamer{
		logger:       cfg.Logger,
		config:       cfg.Config,
		assistant:    cfg.Assistant,
		conversation: cfg.Conversation,
		codec:        "PCMU",
		sampleRate:   8000,
		ctx:          streamerCtx,
		cancel:       cancel,
	}

	// Initialize SIP server
	server, err := NewServer(streamerCtx, &ServerConfig{
		TenantID: cfg.TenantID,
		Config:   cfg.Config,
		Logger:   cfg.Logger,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create SIP server: %w", err)
	}
	s.server = server

	// Set up SIP event handlers
	server.SetOnInvite(s.handleInvite)
	server.SetOnBye(s.handleBye)

	// Start SIP server
	if err := server.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("failed to start SIP server: %w", err)
	}

	return s, nil
}

func (s *Streamer) handleInvite(session *Session, fromURI, toURI string) error {
	s.mu.Lock()
	s.session = session
	s.mu.Unlock()

	// Initialize RTP handler for audio
	var payloadType uint8 = 0 // PCMU
	if s.codec == "PCMA" {
		payloadType = 8
	}

	rtpHandler, err := NewRTPHandler(s.ctx, &RTPConfig{
		LocalIP:     s.config.Server,
		LocalPort:   s.config.RTPPortRangeStart,
		PayloadType: payloadType,
		ClockRate:   uint32(s.sampleRate),
		Logger:      s.logger,
	})
	if err != nil {
		return fmt.Errorf("failed to create RTP handler: %w", err)
	}

	s.mu.Lock()
	s.rtpHandler = rtpHandler
	s.mu.Unlock()

	// Update session with local RTP address
	localIP, localPort := rtpHandler.LocalAddr()
	session.SetLocalRTP(localIP, localPort)
	session.SetNegotiatedCodec(s.codec, s.sampleRate)

	// Start RTP processing
	rtpHandler.Start()

	// Start audio forwarding
	go s.forwardIncomingAudio()

	s.logger.Info("SIP call established",
		"from", fromURI,
		"to", toURI,
		"codec", s.codec)

	return nil
}

func (s *Streamer) handleBye(session *Session) error {
	s.Close()
	return nil
}

func (s *Streamer) forwardIncomingAudio() {
	s.mu.RLock()
	rtpHandler := s.rtpHandler
	s.mu.RUnlock()

	if rtpHandler == nil {
		return
	}

	for {
		select {
		case <-s.ctx.Done():
			return
		case audioData, ok := <-rtpHandler.AudioIn():
			if !ok {
				return
			}
			s.mu.Lock()
			s.inputBuffer = append(s.inputBuffer, audioData...)
			s.mu.Unlock()
		}
	}
}

func (s *Streamer) Context() context.Context {
	return s.ctx
}

func (s *Streamer) Recv() (*protos.AssistantTalkInput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if session is active
	if s.session == nil || !s.session.IsActive() {
		return nil, io.EOF
	}

	// Buffer threshold: 60ms of audio at 8kHz = 480 samples
	bufferSizeThreshold := s.sampleRate * 60 / 1000

	if len(s.inputBuffer) >= bufferSizeThreshold {
		audioData := s.inputBuffer[:bufferSizeThreshold]
		s.inputBuffer = s.inputBuffer[bufferSizeThreshold:]

		return &protos.AssistantTalkInput{
			Request: &protos.AssistantTalkInput_Message{
				Message: &protos.ConversationUserMessage{
					Message: &protos.ConversationUserMessage_Audio{
						Audio: audioData,
					},
				},
			},
		}, nil
	}

	return nil, nil
}

func (s *Streamer) Send(response *protos.AssistantTalkOutput) error {
	switch data := response.GetData().(type) {
	case *protos.AssistantTalkOutput_Assistant:
		switch content := data.Assistant.Message.(type) {
		case *protos.ConversationAssistantMessage_Audio:
			return s.sendAudio(content.Audio)
		}
	case *protos.AssistantTalkOutput_Interruption:
		if data.Interruption.Type == protos.ConversationInterruption_INTERRUPTION_TYPE_WORD {
			return s.handleInterruption()
		}
	case *protos.AssistantTalkOutput_Directive:
		if data.Directive.GetType() == protos.ConversationDirective_END_CONVERSATION {
			return s.Close()
		}
	}
	return nil
}

func (s *Streamer) sendAudio(audioData []byte) error {
	s.mu.RLock()
	rtpHandler := s.rtpHandler
	s.mu.RUnlock()

	if rtpHandler == nil {
		return nil
	}

	select {
	case rtpHandler.AudioOut() <- audioData:
		return nil
	default:
		return nil
	}
}

func (s *Streamer) handleInterruption() error {
	s.mu.Lock()
	s.outputBuffer = nil
	s.mu.Unlock()
	return nil
}

// GetAudioConfig returns the audio configuration for this streamer
func (s *Streamer) GetAudioConfig() (*protos.AudioConfig, *protos.AudioConfig) {
	inputConfig := internal_audio.NewMulaw8khzMonoAudioConfig()
	outputConfig := internal_audio.NewMulaw8khzMonoAudioConfig()
	return inputConfig, outputConfig
}

func (s *Streamer) Close() error {
	s.cancel()

	s.mu.Lock()
	if s.rtpHandler != nil {
		s.rtpHandler.Stop()
		s.rtpHandler = nil
	}
	if s.server != nil {
		s.server.Stop()
		s.server = nil
	}
	if s.session != nil {
		s.session.End()
		s.session = nil
	}
	s.mu.Unlock()

	return nil
}
