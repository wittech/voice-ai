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
	"time"

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
	configSent   bool
}

// StreamerConfig holds configuration for creating a SIP streamer
type StreamerConfig struct {
	Config       *Config
	Logger       commons.Logger
	TenantID     string
	Assistant    *internal_assistant_entity.Assistant
	Conversation *internal_conversation_entity.AssistantConversation
}

// InboundStreamerConfig holds configuration for inbound SIP calls
type InboundStreamerConfig struct {
	Config       *Config
	Logger       commons.Logger
	Session      *Session
	Assistant    *internal_assistant_entity.Assistant
	Conversation *internal_conversation_entity.AssistantConversation
}

// NewInboundStreamer creates a streamer for an inbound SIP call using an existing session
// This does NOT create a new SIP server - it uses the session's RTP handler from the global server
func NewInboundStreamer(ctx context.Context, cfg *InboundStreamerConfig) (internal_type.Streamer, error) {
	if cfg.Session == nil {
		return nil, fmt.Errorf("session is required for inbound streamer")
	}

	// Get the RTP handler from the session (created by server.handleInvite)
	rtpHandler := cfg.Session.GetRTPHandler()
	if rtpHandler == nil {
		return nil, fmt.Errorf("session has no RTP handler")
	}

	streamerCtx, cancel := context.WithCancel(ctx)

	s := &Streamer{
		logger:       cfg.Logger,
		config:       cfg.Config,
		session:      cfg.Session,
		rtpHandler:   rtpHandler,
		assistant:    cfg.Assistant,
		conversation: cfg.Conversation,
		codec:        "PCMU",
		sampleRate:   8000,
		ctx:          streamerCtx,
		cancel:       cancel,
	}

	// Start audio forwarding from RTP handler
	go s.forwardIncomingAudio()

	localIP, localPort := rtpHandler.LocalAddr()
	cfg.Logger.Info("Inbound SIP streamer created",
		"call_id", cfg.Session.GetInfo().CallID,
		"codec", s.codec,
		"rtp_port", localPort,
		"local_ip", localIP)

	return s, nil
}

// NewStreamer creates a new native SIP streamer for outbound calls
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
		s.logger.Error("forwardIncomingAudio: RTP handler is nil")
		return
	}

	s.logger.Info("forwardIncomingAudio: Started listening for RTP audio")
	packetCount := 0

	for {
		select {
		case <-s.ctx.Done():
			s.logger.Info("forwardIncomingAudio: Context cancelled", "total_packets", packetCount)
			return
		case audioData, ok := <-rtpHandler.AudioIn():
			if !ok {
				s.logger.Info("forwardIncomingAudio: Audio channel closed", "total_packets", packetCount)
				return
			}
			packetCount++
			s.mu.Lock()
			s.inputBuffer = append(s.inputBuffer, audioData...)
			bufLen := len(s.inputBuffer)
			s.mu.Unlock()

			// Log every 50 packets (1 second)
			if packetCount%50 == 1 {
				s.logger.Debug("forwardIncomingAudio: Buffered audio", "packet_count", packetCount, "buffer_size", bufLen, "chunk_size", len(audioData))
			}
		}
	}
}

func (s *Streamer) Context() context.Context {
	return s.ctx
}

func (s *Streamer) Recv() (*protos.AssistantTalkInput, error) {
	// Send connection/config request on first call
	s.mu.Lock()
	if !s.configSent {
		s.configSent = true
		s.mu.Unlock()
		s.logger.Info("SIP streamer sending connection request",
			"assistant_id", s.assistant.Id,
			"conversation_id", s.conversation.Id)
		return s.createConnectionRequest()
	}
	s.mu.Unlock()

	// Buffer threshold: 60ms of audio at 8kHz = 480 samples
	bufferSizeThreshold := s.sampleRate * 60 / 1000

	// Block until we have enough audio data or context is cancelled
	for {
		select {
		case <-s.ctx.Done():
			return nil, io.EOF
		default:
		}

		s.mu.Lock()
		// Check if session is active
		if s.session == nil || !s.session.IsActive() {
			s.mu.Unlock()
			return nil, io.EOF
		}

		if len(s.inputBuffer) >= bufferSizeThreshold {
			audioData := make([]byte, bufferSizeThreshold)
			copy(audioData, s.inputBuffer[:bufferSizeThreshold])
			s.inputBuffer = s.inputBuffer[bufferSizeThreshold:]
			s.mu.Unlock()

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
		s.mu.Unlock()

		// Wait a bit before checking again (20ms = typical RTP packet interval)
		select {
		case <-s.ctx.Done():
			return nil, io.EOF
		case <-time.After(20 * time.Millisecond):
			// Continue polling
		}
	}
}

// createConnectionRequest creates the initial connection request for the talker
func (s *Streamer) createConnectionRequest() (*protos.AssistantTalkInput, error) {
	inputConfig, outputConfig := s.GetAudioConfig()
	return &protos.AssistantTalkInput{
		Request: &protos.AssistantTalkInput_Configuration{
			Configuration: &protos.ConversationConfiguration{
				AssistantConversationId: s.conversation.Id,
				Assistant: &protos.AssistantDefinition{
					AssistantId: s.assistant.Id,
					Version:     "latest",
				},
				InputConfig:  &protos.StreamConfig{Audio: inputConfig},
				OutputConfig: &protos.StreamConfig{Audio: outputConfig},
			},
		},
	}, nil
}

func (s *Streamer) Send(response *protos.AssistantTalkOutput) error {
	switch data := response.GetData().(type) {
	case *protos.AssistantTalkOutput_Assistant:
		switch content := data.Assistant.Message.(type) {
		case *protos.ConversationAssistantMessage_Audio:
			s.logger.Debug("Send: Received audio output from assistant", "audio_size", len(content.Audio))
			return s.sendAudio(content.Audio)
		}
	case *protos.AssistantTalkOutput_Interruption:
		s.logger.Debug("Send: Received interruption")
		if data.Interruption.Type == protos.ConversationInterruption_INTERRUPTION_TYPE_WORD {
			return s.handleInterruption()
		}
	case *protos.AssistantTalkOutput_Directive:
		s.logger.Debug("Send: Received directive", "type", data.Directive.GetType())
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
		s.logger.Error("sendAudio: RTP handler is nil")
		return nil
	}

	select {
	case rtpHandler.AudioOut() <- audioData:
		s.logger.Debug("sendAudio: Queued audio for RTP", "size", len(audioData))
		return nil
	default:
		s.logger.Warn("sendAudio: RTP output channel full, dropping audio")
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
