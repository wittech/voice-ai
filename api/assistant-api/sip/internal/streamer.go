// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package internal_sip

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	internal_audio "github.com/rapidaai/api/assistant-api/internal/audio"
	internal_assistant_entity "github.com/rapidaai/api/assistant-api/internal/entity/assistants"
	internal_conversation_entity "github.com/rapidaai/api/assistant-api/internal/entity/conversations"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/protos"
)

// Streamer constants
const (
	// Audio buffer size threshold in milliseconds
	audioBufferThresholdMs = 60
	// RTP packet interval in milliseconds
	packetIntervalMs = 20
	// rtpLogInterval is the number of packets between periodic log entries
	rtpLogInterval = 50
)

// Streamer implements the TelephonyStreamer interface using native SIP signaling and RTP
// No WebSocket needed - uses sipgo for signaling, RTP/UDP for audio
type Streamer struct {
	mu     sync.RWMutex
	closed atomic.Bool

	logger     commons.Logger
	config     *Config
	session    *Session
	server     *Server
	rtpHandler *RTPHandler

	assistant    *internal_assistant_entity.Assistant
	conversation *internal_conversation_entity.AssistantConversation

	codec *Codec

	ctx    context.Context
	cancel context.CancelFunc

	inputBuffer []byte
	configSent  atomic.Bool
}

// StreamerConfig holds configuration for creating a SIP streamer
type StreamerConfig struct {
	Config       *Config
	Logger       commons.Logger
	TenantID     string
	Assistant    *internal_assistant_entity.Assistant
	Conversation *internal_conversation_entity.AssistantConversation
}

// Validate validates the streamer configuration
func (c *StreamerConfig) Validate() error {
	if c.Config == nil {
		return fmt.Errorf("SIP config is required")
	}
	if c.Logger == nil {
		return fmt.Errorf("logger is required")
	}
	if c.TenantID == "" {
		return fmt.Errorf("tenant_id is required")
	}
	if c.Assistant == nil {
		return fmt.Errorf("assistant is required")
	}
	if c.Conversation == nil {
		return fmt.Errorf("conversation is required")
	}
	return c.Config.Validate()
}

// InboundStreamerConfig holds configuration for inbound SIP calls
type InboundStreamerConfig struct {
	Config       *Config
	Logger       commons.Logger
	Session      *Session
	Assistant    *internal_assistant_entity.Assistant
	Conversation *internal_conversation_entity.AssistantConversation
}

// Validate validates the inbound streamer configuration
func (c *InboundStreamerConfig) Validate() error {
	if c.Session == nil {
		return fmt.Errorf("session is required for inbound streamer")
	}
	if c.Logger == nil {
		return fmt.Errorf("logger is required")
	}
	if c.Assistant == nil {
		return fmt.Errorf("assistant is required")
	}
	if c.Conversation == nil {
		return fmt.Errorf("conversation is required")
	}
	return nil
}

// NewInboundStreamer creates a streamer for an inbound SIP call using an existing session
// This does NOT create a new SIP server - it uses the session's RTP handler from the global server
func NewInboundStreamer(ctx context.Context, cfg *InboundStreamerConfig) (internal_type.TelephonyStreamer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, NewSIPError("NewInboundStreamer", "", "invalid configuration", err)
	}

	// Get the RTP handler from the session (created by server.handleInvite)
	rtpHandler := cfg.Session.GetRTPHandler()
	if rtpHandler == nil {
		return nil, NewSIPError("NewInboundStreamer", cfg.Session.GetCallID(), "session has no RTP handler", ErrRTPNotInitialized)
	}

	streamerCtx, cancel := context.WithCancel(ctx)

	// Get codec from session
	codec := cfg.Session.GetNegotiatedCodec()
	if codec == nil {
		codec = &CodecPCMU
	}

	s := &Streamer{
		logger:       cfg.Logger,
		config:       cfg.Config,
		session:      cfg.Session,
		rtpHandler:   rtpHandler,
		assistant:    cfg.Assistant,
		conversation: cfg.Conversation,
		codec:        codec,
		ctx:          streamerCtx,
		cancel:       cancel,
	}

	// Start audio forwarding from RTP handler
	go s.forwardIncomingAudio()

	localIP, localPort := rtpHandler.LocalAddr()
	cfg.Logger.Info("Inbound SIP streamer created",
		"call_id", cfg.Session.GetCallID(),
		"codec", codec.Name,
		"rtp_port", localPort,
		"local_ip", localIP)

	return s, nil
}

// NewStreamer creates a new native SIP streamer for outbound calls
// Uses sipgo for SIP signaling and RTP for audio transport - no WebSocket needed
func NewStreamer(ctx context.Context, cfg *StreamerConfig) (internal_type.TelephonyStreamer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, NewSIPError("NewStreamer", "", "invalid configuration", err)
	}
	streamerCtx, cancel := context.WithCancel(ctx)
	s := &Streamer{
		logger:       cfg.Logger,
		config:       cfg.Config,
		assistant:    cfg.Assistant,
		conversation: cfg.Conversation,
		codec:        &CodecPCMU,
		ctx:          streamerCtx,
		cancel:       cancel,
	}

	// Initialize SIP server for outbound calls
	// Creates ListenConfig from tenant config for local binding
	listenConfig := &ListenConfig{
		Address:   cfg.Config.Server,
		Port:      cfg.Config.Port,
		Transport: cfg.Config.Transport,
	}

	// Config resolver returns the tenant config for all calls on this server
	tenantConfig := cfg.Config
	configResolver := func(inviteCtx *InviteContext) (*InviteResult, error) {
		return &InviteResult{
			Config:      tenantConfig,
			ShouldAllow: true,
		}, nil
	}

	server, err := NewServer(streamerCtx, &ServerConfig{
		ListenConfig:   listenConfig,
		ConfigResolver: configResolver,
		Logger:         cfg.Logger,
	})
	if err != nil {
		cancel()
		return nil, err
	}
	s.server = server

	// Set up SIP event handlers
	server.SetOnInvite(s.handleInvite)
	server.SetOnBye(s.handleBye)
	server.SetOnError(s.handleError)

	// Start SIP server
	if err := server.Start(); err != nil {
		cancel()
		return nil, err
	}

	cfg.Logger.Info("Outbound SIP streamer created",
		"tenant_id", cfg.TenantID,
		"assistant_id", cfg.Assistant.Id,
		"conversation_id", cfg.Conversation.Id)

	return s, nil
}

func (s *Streamer) handleInvite(session *Session, fromURI, toURI string) error {
	s.mu.Lock()
	s.session = session
	codec := s.codec
	s.mu.Unlock()

	// Initialize RTP handler for audio
	rtpHandler, err := NewRTPHandler(s.ctx, &RTPConfig{
		LocalIP:     s.config.Server,
		LocalPort:   s.config.RTPPortRangeStart,
		PayloadType: codec.PayloadType,
		ClockRate:   codec.ClockRate,
		Logger:      s.logger,
	})
	if err != nil {
		return NewSIPError("handleInvite", session.GetCallID(), "failed to create RTP handler", err)
	}

	s.mu.Lock()
	s.rtpHandler = rtpHandler
	s.mu.Unlock()

	// Update session with local RTP address
	localIP, localPort := rtpHandler.LocalAddr()
	session.SetLocalRTP(localIP, localPort)
	session.SetNegotiatedCodec(codec.Name, int(codec.ClockRate))
	session.SetRTPHandler(rtpHandler)

	// Start RTP processing
	rtpHandler.Start()

	// Start audio forwarding
	go s.forwardIncomingAudio()

	s.logger.Info("SIP call established",
		"call_id", session.GetCallID(),
		"from", fromURI,
		"to", toURI,
		"codec", codec.Name)

	return nil
}

func (s *Streamer) handleBye(session *Session) error {
	s.logger.Info("BYE received, closing streamer", "call_id", session.GetCallID())
	return s.Close()
}

func (s *Streamer) handleError(session *Session, err error) {
	s.logger.Error("SIP error occurred",
		"call_id", session.GetCallID(),
		"error", err)
}

func (s *Streamer) forwardIncomingAudio() {
	s.mu.RLock()
	rtpHandler := s.rtpHandler
	s.mu.RUnlock()

	if rtpHandler == nil {
		s.logger.Error("forwardIncomingAudio: RTP handler is nil")
		return
	}

	s.logger.Debug("forwardIncomingAudio: Started listening for RTP audio")
	packetCount := 0

	for {
		select {
		case <-s.ctx.Done():
			s.logger.Debug("forwardIncomingAudio: Context cancelled", "total_packets", packetCount)
			return
		case audioData, ok := <-rtpHandler.AudioIn():
			if !ok {
				s.logger.Debug("forwardIncomingAudio: Audio channel closed", "total_packets", packetCount)
				return
			}
			packetCount++
			s.mu.Lock()
			s.inputBuffer = append(s.inputBuffer, audioData...)
			bufLen := len(s.inputBuffer)
			s.mu.Unlock()

			// Log periodically (every 50 packets = ~1 second)
			if packetCount%rtpLogInterval == 1 {
				s.logger.Debug("forwardIncomingAudio: Buffered audio",
					"packet_count", packetCount,
					"buffer_size", bufLen,
					"chunk_size", len(audioData))
			}
		}
	}
}

func (s *Streamer) Context() context.Context {
	return s.ctx
}

func (s *Streamer) Recv() (*protos.AssistantTalkInput, error) {
	if s.closed.Load() {
		return nil, io.EOF
	}

	// Send connection/config request on first call
	if s.configSent.CompareAndSwap(false, true) {
		s.logger.Info("SIP streamer sending connection request",
			"assistant_id", s.assistant.Id,
			"conversation_id", s.conversation.Id)
		return s.createConnectionRequest()
	}

	// Calculate buffer threshold based on codec sample rate
	sampleRate := int(s.codec.ClockRate)
	bufferSizeThreshold := sampleRate * audioBufferThresholdMs / 1000

	// Block until we have enough audio data or context is cancelled
	for {
		select {
		case <-s.ctx.Done():
			return nil, io.EOF
		default:
		}

		s.mu.RLock()
		session := s.session
		s.mu.RUnlock()

		// Check if session is active
		if session == nil || !session.IsActive() {
			return nil, io.EOF
		}

		s.mu.Lock()
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
		case <-time.After(packetIntervalMs * time.Millisecond):
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
	if s.closed.Load() {
		return ErrSessionClosed
	}

	switch data := response.GetData().(type) {
	case *protos.AssistantTalkOutput_Assistant:
		switch content := data.Assistant.Message.(type) {
		case *protos.ConversationAssistantMessage_Audio:
			s.logger.Debug("Send: Received audio output from assistant", "audio_size", len(content.Audio))
			return s.sendAudio(content.Audio)
		}
	case *protos.AssistantTalkOutput_Interruption:
		s.logger.Debug("Send: Received interruption", "type", data.Interruption.Type)
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
		return ErrRTPNotInitialized
	}

	select {
	case rtpHandler.AudioOut() <- audioData:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	default:
		s.logger.Warn("sendAudio: RTP output channel full, dropping audio", "size", len(audioData))
		return nil
	}
}

func (s *Streamer) handleInterruption() error {
	s.mu.Lock()
	s.inputBuffer = nil // Clear input buffer on interruption
	s.mu.Unlock()

	s.logger.Debug("Handled interruption, cleared audio buffers")
	return nil
}

// GetAudioConfig returns the audio configuration for this streamer
func (s *Streamer) GetAudioConfig() (*protos.AudioConfig, *protos.AudioConfig) {
	// Select audio config based on codec
	var inputConfig, outputConfig *protos.AudioConfig

	s.mu.RLock()
	codec := s.codec
	s.mu.RUnlock()

	if codec != nil && codec.Name == "PCMA" {
		// A-law codec (less common, mainly used in Europe)
		inputConfig = internal_audio.NewMulaw8khzMonoAudioConfig() // TODO: Add PCMA config
		outputConfig = internal_audio.NewMulaw8khzMonoAudioConfig()
	} else {
		// Default to μ-law (PCMU)
		inputConfig = internal_audio.NewMulaw8khzMonoAudioConfig()
		outputConfig = internal_audio.NewMulaw8khzMonoAudioConfig()
	}

	return inputConfig, outputConfig
}

// Close closes the streamer and releases all resources
func (s *Streamer) Close() error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil // Already closed
	}

	s.logger.Info("Closing SIP streamer",
		"assistant_id", s.assistant.Id,
		"conversation_id", s.conversation.Id)

	// Cancel context first
	s.cancel()

	s.mu.Lock()
	rtpHandler := s.rtpHandler
	server := s.server
	session := s.session
	s.rtpHandler = nil
	s.server = nil
	s.session = nil
	s.inputBuffer = nil
	s.mu.Unlock()

	// Stop RTP handler
	if rtpHandler != nil {
		if err := rtpHandler.Stop(); err != nil {
			s.logger.Warn("Error stopping RTP handler", "error", err)
		}
	}

	// Stop server
	if server != nil {
		server.Stop()
	}

	// End session
	if session != nil {
		session.End()
	}

	s.logger.Info("SIP streamer closed")
	return nil
}

// IsClosed returns whether the streamer has been closed
func (s *Streamer) IsClosed() bool {
	return s.closed.Load()
}

// GetSession returns the underlying SIP session
func (s *Streamer) GetSession() *Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.session
}
