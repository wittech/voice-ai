// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package webrtc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/rtp"
	pionwebrtc "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	internal_audio "github.com/rapidaai/api/assistant-api/internal/audio"
	internal_audio_resampler "github.com/rapidaai/api/assistant-api/internal/audio/resampler"
	internal_assistant_entity "github.com/rapidaai/api/assistant-api/internal/entity/assistants"
	internal_conversation_entity "github.com/rapidaai/api/assistant-api/internal/entity/conversations"
	internal_streamers "github.com/rapidaai/api/assistant-api/internal/streamers"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/protos"
)

// ============================================================================
// Streamer - Main WebRTC implementation
// ============================================================================

// Streamer implements the Streamer interface using native Pion WebRTC
// Audio flows through WebRTC media tracks; WebSocket is only for signaling
// Uses Opus codec at 48kHz for best quality and browser compatibility
type Streamer struct {
	mu sync.Mutex

	// Core components
	logger        commons.Logger
	config        *Config
	signalingConn *websocket.Conn
	assistant     *internal_assistant_entity.Assistant
	conversation  *internal_conversation_entity.AssistantConversation

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc

	// Session state (embedded, no separate struct)
	sessionID string
	state     SessionState
	createdAt time.Time

	// Pion WebRTC
	pc             *pionwebrtc.PeerConnection
	localTrack     *pionwebrtc.TrackLocalStaticSample
	opusCodec      *OpusCodec // Opus encoder for outgoing audio
	mediaConnected bool
	configSent     bool // Track if Configuration has been sent to downstream

	// Separate channels - signaling (low volume) and audio (high volume)
	// All decisions made in Recv() for synchronous processing
	signalingCh chan SignalingMessage // raw signaling messages
	audioCh     chan []byte           // decoded+resampled audio
	errCh       chan error

	// Buffer for incoming audio accumulation in Recv()
	inputBuffer *bytes.Buffer

	// Write mutex for RTP - ensures sequential WriteSample calls
	writeMu     sync.Mutex
	packetCount uint64 // Track packets sent for debugging

	// Resampler for sample rate conversion
	resampler    internal_type.AudioResampler
	opusConfig   *protos.AudioConfig // 48kHz for Opus/WebRTC
	sttTtsConfig *protos.AudioConfig // 16kHz for STT/TTS

	// Output audio queue for consistent pacing
	// TTS sends variable-sized chunks; we buffer and drain at consistent 20ms rate
	outputBuffer   *bytes.Buffer
	outputBufferMu sync.Mutex
	outputCh       chan struct{} // Signal that new audio is available
	outputStarted  bool          // Track if output sender goroutine is started
}

// StreamerConfig holds configuration for creating a WebRTC streamer
type StreamerConfig struct {
	Config        *Config
	Logger        commons.Logger
	SignalingConn *websocket.Conn
	Assistant     *internal_assistant_entity.Assistant
	Conversation  *internal_conversation_entity.AssistantConversation
}

// NewStreamer creates a new WebRTC streamer
func NewStreamer(ctx context.Context, cfg *StreamerConfig) (internal_streamers.Streamer, error) {
	if cfg.Config == nil {
		cfg.Config = DefaultConfig()
	}

	streamerCtx, cancel := context.WithCancel(ctx)

	resampler, err := internal_audio_resampler.GetResampler(cfg.Logger)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create resampler: %w", err)
	}

	// Create Opus codec for encoding outgoing audio
	opusCodec, err := NewOpusCodec()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create Opus codec: %w", err)
	}

	s := &Streamer{
		logger:        cfg.Logger,
		config:        cfg.Config,
		signalingConn: cfg.SignalingConn,
		assistant:     cfg.Assistant,
		conversation:  cfg.Conversation,
		ctx:           streamerCtx,
		cancel:        cancel,

		// Session
		sessionID: uuid.New().String(),
		state:     SessionStateNew,
		createdAt: time.Now(),

		// Separate channels - decisions made in Recv()
		signalingCh: make(chan SignalingMessage, 10), // signaling: low volume, high priority
		audioCh:     make(chan []byte, 100),          // audio: high volume
		errCh:       make(chan error, 1),

		// Input buffer for Recv()
		inputBuffer: new(bytes.Buffer),

		// Audio - Opus at 48kHz, STT/TTS at 16kHz
		resampler:    resampler,
		opusConfig:   internal_audio.NewLinear48khzMonoAudioConfig(),
		sttTtsConfig: internal_audio.NewLinear16khzMonoAudioConfig(),
		opusCodec:    opusCodec,

		// Output buffer for consistent pacing
		outputBuffer: new(bytes.Buffer),
		outputCh:     make(chan struct{}, 1),
	}

	// Create peer connection
	if err := s.createPeerConnection(); err != nil {
		cancel()
		return nil, err
	}

	// Start signaling reader goroutine
	go s.readSignaling()

	return s, nil
}

// ============================================================================
// Peer Connection Setup
// ============================================================================

func (s *Streamer) createPeerConnection() error {
	// Setup media engine with Opus as primary codec
	mediaEngine := &pionwebrtc.MediaEngine{}

	// Opus - primary codec (best quality, native WebRTC)
	// stereo=0 for mono, useinbandfec=1 for forward error correction
	if err := mediaEngine.RegisterCodec(pionwebrtc.RTPCodecParameters{
		RTPCodecCapability: pionwebrtc.RTPCodecCapability{
			MimeType:    pionwebrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2, // Opus always uses 2 channels in RTP even for mono
			SDPFmtpLine: "minptime=10;useinbandfec=1;stereo=0;sprop-stereo=0",
		},
		PayloadType: 111,
	}, pionwebrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("failed to register Opus: %w", err)
	}

	// PCMU (μ-law) - fallback
	if err := mediaEngine.RegisterCodec(pionwebrtc.RTPCodecParameters{
		RTPCodecCapability: pionwebrtc.RTPCodecCapability{
			MimeType:  pionwebrtc.MimeTypePCMU,
			ClockRate: 8000,
			Channels:  1,
		},
		PayloadType: 0,
	}, pionwebrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("failed to register PCMU: %w", err)
	}

	// PCMA (A-law) - fallback
	if err := mediaEngine.RegisterCodec(pionwebrtc.RTPCodecParameters{
		RTPCodecCapability: pionwebrtc.RTPCodecCapability{
			MimeType:  pionwebrtc.MimeTypePCMA,
			ClockRate: 8000,
			Channels:  1,
		},
		PayloadType: 8,
	}, pionwebrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("failed to register PCMA: %w", err)
	}

	// Interceptors
	registry := &interceptor.Registry{}
	if err := pionwebrtc.RegisterDefaultInterceptors(mediaEngine, registry); err != nil {
		return fmt.Errorf("failed to register interceptors: %w", err)
	}
	pli, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		return fmt.Errorf("failed to create PLI interceptor: %w", err)
	}
	registry.Add(pli)

	// Create API and peer connection
	api := pionwebrtc.NewAPI(
		pionwebrtc.WithMediaEngine(mediaEngine),
		pionwebrtc.WithInterceptorRegistry(registry),
	)

	iceServers := make([]pionwebrtc.ICEServer, len(s.config.ICEServers))
	for i, srv := range s.config.ICEServers {
		iceServers[i] = pionwebrtc.ICEServer{
			URLs:       srv.URLs,
			Username:   srv.Username,
			Credential: srv.Credential,
		}
	}

	pcConfig := pionwebrtc.Configuration{ICEServers: iceServers}
	if s.config.ICETransportPolicy == "relay" {
		pcConfig.ICETransportPolicy = pionwebrtc.ICETransportPolicyRelay
	}

	pc, err := api.NewPeerConnection(pcConfig)
	if err != nil {
		return fmt.Errorf("failed to create peer connection: %w", err)
	}
	s.pc = pc

	// Setup event handlers
	s.setupPeerEventHandlers()

	// Create local audio track
	return s.createLocalTrack()
}

func (s *Streamer) setupPeerEventHandlers() {
	// ICE candidates - send directly (no goroutine needed)
	s.pc.OnICECandidate(func(c *pionwebrtc.ICECandidate) {
		if c == nil {
			return
		}
		cJSON := c.ToJSON()
		ice := &ICECandidate{Candidate: cJSON.Candidate}
		if cJSON.SDPMid != nil {
			ice.SDPMid = *cJSON.SDPMid
		}
		if cJSON.SDPMLineIndex != nil {
			ice.SDPMLineIndex = int(*cJSON.SDPMLineIndex)
		}
		if cJSON.UsernameFragment != nil {
			ice.UsernameFragment = *cJSON.UsernameFragment
		}
		s.sendSignaling(SignalingMessage{
			Type:      "ice_candidate",
			SessionID: s.sessionID,
			Candidate: ice,
		})
	})

	// Connection state
	s.pc.OnConnectionStateChange(func(state pionwebrtc.PeerConnectionState) {
		s.logger.Info("WebRTC connection state", "state", state.String(), "session", s.sessionID)

		s.mu.Lock()
		switch state {
		case pionwebrtc.PeerConnectionStateConnected:
			s.state = SessionStateConnected
			s.mediaConnected = true
			// Start output sender goroutine for consistent audio pacing
			if !s.outputStarted {
				s.outputStarted = true
				go s.runOutputSender()
			}
			// Configuration will be sent on first audio receive (like telephony pattern)
		case pionwebrtc.PeerConnectionStateDisconnected:
			s.state = SessionStateDisconnected
			s.mediaConnected = false
		case pionwebrtc.PeerConnectionStateFailed:
			s.state = SessionStateFailed
			s.mediaConnected = false
		case pionwebrtc.PeerConnectionStateClosed:
			s.state = SessionStateClosed
			s.mediaConnected = false
		}
		s.mu.Unlock()
	})

	// Remote track (incoming audio from client)
	s.pc.OnTrack(func(track *pionwebrtc.TrackRemote, _ *pionwebrtc.RTPReceiver) {
		if track.Kind() != pionwebrtc.RTPCodecTypeAudio {
			return
		}

		s.logger.Info("Remote audio track received", "codec", track.Codec().MimeType)
		go s.readRemoteAudio(track)
	})
}

func (s *Streamer) createLocalTrack() error {
	// Opus RTP uses Channels=2 in header but actual audio is mono (Opus convention)
	track, err := pionwebrtc.NewTrackLocalStaticSample(
		pionwebrtc.RTPCodecCapability{
			MimeType:  pionwebrtc.MimeTypeOpus,
			ClockRate: OpusSampleRate,
			Channels:  2,
		},
		"audio",
		"rapida-voice-ai",
	)
	if err != nil {
		return fmt.Errorf("failed to create Opus track: %w", err)
	}

	if _, err := s.pc.AddTrack(track); err != nil {
		return fmt.Errorf("failed to add track: %w", err)
	}

	s.localTrack = track
	return nil
}

// ============================================================================
// Audio Processing
// ============================================================================

// readRemoteAudio reads from WebRTC track, decodes, resamples, and pushes to audioCh.
// Handles both Opus (48kHz) and G.711 (8kHz) incoming audio.
func (s *Streamer) readRemoteAudio(track *pionwebrtc.TrackRemote) {
	buf := make([]byte, 1500)
	mimeType := track.Codec().MimeType
	isOpus := mimeType == pionwebrtc.MimeTypeOpus

	var g711Decoder *Codec
	var opusDecoder *OpusCodec
	var sourceConfig *protos.AudioConfig

	if isOpus {
		var err error
		opusDecoder, err = NewOpusCodec()
		if err != nil {
			s.logger.Error("Failed to create Opus decoder", "error", err)
			return
		}
		sourceConfig = s.opusConfig
	} else {
		codecType := "pcmu"
		if mimeType == pionwebrtc.MimeTypePCMA {
			codecType = "pcma"
		}
		g711Decoder = NewCodec(codecType)
		sourceConfig = internal_audio.NewLinear8khzMonoAudioConfig()
	}

	s.logger.Info("Remote audio decoder initialized", "codec", mimeType)

	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		n, _, err := track.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			continue
		}

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		if len(pkt.Payload) == 0 {
			continue
		}

		var pcm []byte
		if isOpus {
			pcm, err = opusDecoder.Decode(pkt.Payload)
			if err != nil {
				continue
			}
		} else {
			pcm = g711Decoder.Decode(pkt.Payload)
		}

		resampled, err := s.resampler.Resample(pcm, sourceConfig, s.sttTtsConfig)
		if err != nil {
			continue
		}

		select {
		case s.audioCh <- resampled:
		case <-s.ctx.Done():
			return
		}
	}
}

// ============================================================================
// Signaling
// ============================================================================

func (s *Streamer) sendSignaling(msg SignalingMessage) error {
	if s.signalingConn == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return s.signalingConn.WriteMessage(websocket.TextMessage, data)
}

// ============================================================================
// Streamer Interface Implementation
// ============================================================================

func (s *Streamer) Context() context.Context {
	return s.ctx
}

// readSignaling reads from websocket and pushes raw messages to signalingCh
func (s *Streamer) readSignaling() {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		if s.signalingConn == nil {
			select {
			case s.errCh <- io.EOF:
			default:
			}
			return
		}

		_, msg, err := s.signalingConn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				s.logger.Error("Signaling connection closed", "error", err)
			}
			select {
			case s.errCh <- io.EOF:
			default:
			}
			return
		}

		var sigMsg SignalingMessage
		if json.Unmarshal(msg, &sigMsg) != nil {
			continue
		}

		// Push raw message - Recv() handles all decisions
		select {
		case s.signalingCh <- sigMsg:
		case <-s.ctx.Done():
			return
		}
	}
}

// Recv receives the next input - ALL decisions made here for sync processing
// Priority: errors > signaling > audio (signaling is low volume, high priority)
func (s *Streamer) Recv() (*protos.AssistantTalkInput, error) {
	const bufferThreshold = 32 * 60 // 60ms at 16kHz

	for {
		// Priority select: check errors and signaling first
		select {
		case <-s.ctx.Done():
			return nil, io.EOF
		case err := <-s.errCh:
			return nil, err
		case sigMsg := <-s.signalingCh:
			// Handle signaling synchronously
			input, err := s.handleSignaling(sigMsg)
			if err != nil {
				return nil, err
			}
			if input != nil {
				return input, nil
			}
			continue
		default:
			// No signaling, check audio
		}

		// Check audio channel
		select {
		case <-s.ctx.Done():
			return nil, io.EOF
		case err := <-s.errCh:
			return nil, err
		case sigMsg := <-s.signalingCh:
			input, err := s.handleSignaling(sigMsg)
			if err != nil {
				return nil, err
			}
			if input != nil {
				return input, nil
			}
			continue
		case audio := <-s.audioCh:
			// First audio = media connected, send Configuration
			s.mu.Lock()
			if !s.configSent && s.mediaConnected {
				s.configSent = true
				s.inputBuffer.Write(audio) // Buffer this audio for next Recv()
				s.mu.Unlock()

				s.logger.Info("WebRTC media ready, sending configuration", "session", s.sessionID)
				audioConfig := internal_audio.NewLinear16khzMonoAudioConfig()
				return &protos.AssistantTalkInput{
					Request: &protos.AssistantTalkInput_Configuration{
						Configuration: &protos.ConversationConfiguration{
							AssistantConversationId: s.conversation.Id,
							Assistant:               &protos.AssistantDefinition{AssistantId: s.assistant.Id},
							InputConfig:             &protos.StreamConfig{Audio: audioConfig},
							OutputConfig:            &protos.StreamConfig{Audio: audioConfig},
						},
					},
				}, nil
			}

			// Accumulate audio in buffer
			s.inputBuffer.Write(audio)

			if s.inputBuffer.Len() >= bufferThreshold {
				audioData := make([]byte, s.inputBuffer.Len())
				s.inputBuffer.Read(audioData)
				s.mu.Unlock()

				return &protos.AssistantTalkInput{
					Request: &protos.AssistantTalkInput_Message{
						Message: &protos.ConversationUserMessage{
							Message: &protos.ConversationUserMessage_Audio{Audio: audioData},
						},
					},
				}, nil
			}
			s.mu.Unlock()
			// Not enough audio yet, continue loop
		}
	}
}

func (s *Streamer) handleSignaling(msg SignalingMessage) (*protos.AssistantTalkInput, error) {
	switch msg.Type {
	case "connect":
		return s.handleConnect()
	case "offer":
		return s.handleOffer(msg.SDP)
	case "answer":
		return s.handleAnswer(msg.SDP)
	case "ice_candidate":
		return s.handleICE(msg.Candidate)
	case "content":
		return s.handleContent(msg.Metadata)
	case "disconnect":
		s.Close()
		return nil, io.EOF
	}
	return nil, nil
}

func (s *Streamer) handleConnect() (*protos.AssistantTalkInput, error) {
	s.logger.Info("WebRTC connect - starting handshake", "session", s.sessionID)
	s.mu.Lock()
	s.state = SessionStateConnecting
	s.mu.Unlock()

	// Send config to client
	s.sendSignaling(SignalingMessage{
		Type:      "config",
		SessionID: s.sessionID,
		Metadata: map[string]interface{}{
			"ice_servers":     s.config.ICEServers,
			"audio_codec":     s.config.AudioCodec,
			"sample_rate":     s.config.SampleRate,
			"conversation_id": s.conversation.Id,
		},
	})

	// Create and send offer
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create offer: %w", err)
	}
	if err := s.pc.SetLocalDescription(offer); err != nil {
		return nil, fmt.Errorf("failed to set local description: %w", err)
	}

	s.sendSignaling(SignalingMessage{
		Type:      "offer",
		SessionID: s.sessionID,
		SDP:       offer.SDP,
	})

	// Don't return Configuration here - wait for media connection
	// Configuration will be returned via Recv() when connectCh signals
	return nil, nil
}

func (s *Streamer) handleOffer(sdp string) (*protos.AssistantTalkInput, error) {
	if sdp == "" {
		return nil, nil
	}

	if err := s.pc.SetRemoteDescription(pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeOffer,
		SDP:  sdp,
	}); err != nil {
		return nil, fmt.Errorf("failed to set remote offer: %w", err)
	}

	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create answer: %w", err)
	}
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return nil, fmt.Errorf("failed to set local description: %w", err)
	}

	return nil, s.sendSignaling(SignalingMessage{
		Type:      "answer",
		SessionID: s.sessionID,
		SDP:       answer.SDP,
	})
}

func (s *Streamer) handleAnswer(sdp string) (*protos.AssistantTalkInput, error) {
	if sdp == "" {
		return nil, nil
	}
	return nil, s.pc.SetRemoteDescription(pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeAnswer,
		SDP:  sdp,
	})
}

func (s *Streamer) handleICE(candidate *ICECandidate) (*protos.AssistantTalkInput, error) {
	if candidate == nil {
		return nil, nil
	}

	idx := uint16(candidate.SDPMLineIndex)
	return nil, s.pc.AddICECandidate(pionwebrtc.ICECandidateInit{
		Candidate:        candidate.Candidate,
		SDPMid:           &candidate.SDPMid,
		SDPMLineIndex:    &idx,
		UsernameFragment: &candidate.UsernameFragment,
	})
}

func (s *Streamer) handleContent(metadata map[string]interface{}) (*protos.AssistantTalkInput, error) {
	if metadata == nil {
		return nil, nil
	}

	contentType, _ := metadata["content_type"].(string)
	text, _ := metadata["text"].(string)
	if contentType != "user_text" || text == "" {
		return nil, nil
	}

	msgID, _ := metadata["message_id"].(string)
	if msgID == "" {
		msgID = fmt.Sprintf("msg_%d", time.Now().UnixMilli())
	}

	s.logger.Info("Received text", "type", contentType, "text", text)

	return &protos.AssistantTalkInput{
		Request: &protos.AssistantTalkInput_Message{
			Message: &protos.ConversationUserMessage{
				Id:      msgID,
				Message: &protos.ConversationUserMessage_Text{Text: text},
			},
		},
	}, nil
}

// Send sends output to the client
func (s *Streamer) Send(response *protos.AssistantTalkOutput) error {
	switch data := response.GetData().(type) {
	case *protos.AssistantTalkOutput_Assistant:
		switch content := data.Assistant.Message.(type) {
		case *protos.ConversationAssistantMessage_Audio:
			return s.sendAudio(content.Audio, data.Assistant.GetCompleted())
		case *protos.ConversationAssistantMessage_Text:
			return s.sendContent("text", content.Text, data.Assistant.GetId(), data.Assistant.GetCompleted())
		}

	case *protos.AssistantTalkOutput_User:
		if content, ok := data.User.Message.(*protos.ConversationUserMessage_Text); ok {
			return s.sendContent("user_text", content.Text, data.User.GetId(), data.User.GetCompleted())
		}

	case *protos.AssistantTalkOutput_Interruption:
		if data.Interruption.Type == protos.ConversationInterruption_INTERRUPTION_TYPE_WORD {
			s.mu.Lock()
			s.inputBuffer.Reset()
			s.mu.Unlock()

			// Clear pending output audio on interruption
			s.clearOutputBuffer()

			s.logger.Info("Interruption received")
			return s.sendSignaling(SignalingMessage{Type: "clear", SessionID: s.sessionID})
		}

	case *protos.AssistantTalkOutput_Directive:
		if data.Directive.GetType() == protos.ConversationDirective_END_CONVERSATION {
			return s.Close()
		}

	case *protos.AssistantTalkOutput_Error:
		return s.sendSignaling(SignalingMessage{
			Type:      "error",
			SessionID: s.sessionID,
			Error:     data.Error.GetErrorMessage(),
		})
	}
	return nil
}

// sendAudio queues 16kHz TTS audio for consistent-rate transmission.
// Flow: TTS audio (16kHz) → resample to 48kHz → queue to outputBuffer.
// The runOutputSender goroutine drains the buffer at a consistent 20ms rate.
//
// TTS sends audio in bursts but total duration matches real-time, so buffer
// will accumulate during bursts and drain steadily - NO audio should be dropped.
func (s *Streamer) sendAudio(audio []byte, _ bool) error {
	if len(audio) == 0 {
		return nil
	}

	// Resample from 16kHz (TTS) to 48kHz (Opus)
	audio48kHz, err := s.resampler.Resample(audio, s.sttTtsConfig, s.opusConfig)
	if err != nil {
		s.logger.Error("Resample to 48kHz failed", "error", err)
		return err
	}

	// Queue to output buffer - let it grow, output sender drains at real-time rate
	s.outputBufferMu.Lock()
	s.outputBuffer.Write(audio48kHz)
	bufferLen := s.outputBuffer.Len()
	s.outputBufferMu.Unlock()

	// Warn if buffer gets unusually large (>10 seconds) - indicates potential issue
	if bufferLen > MaxOutputBufferBytes {
		s.logger.Warn("Output buffer large", "bufferBytes", bufferLen, "bufferMs", (bufferLen/2)*1000/OpusSampleRate)
	}

	// Signal that new audio is available (non-blocking)
	select {
	case s.outputCh <- struct{}{}:
	default:
	}

	return nil
}

// runOutputSender drains the output buffer at a consistent 20ms rate.
// This ensures packets are sent at real-time rate regardless of how TTS delivers audio.
// Prevents jitter buffer overflow by maintaining consistent packet timing.
func (s *Streamer) runOutputSender() {
	const pktDuration = OpusFrameDuration * time.Millisecond

	ticker := time.NewTicker(pktDuration)
	defer ticker.Stop()

	chunk := make([]byte, OpusFrameBytes)

	s.logger.Info("Output sender started", "session", s.sessionID)

	for {
		select {
		case <-s.ctx.Done():
			s.logger.Info("Output sender stopped", "session", s.sessionID, "totalPackets", s.packetCount)
			return
		case <-ticker.C:
			s.outputBufferMu.Lock()
			n, _ := s.outputBuffer.Read(chunk)
			s.outputBufferMu.Unlock()

			if n == 0 {
				continue
			}

			// Pad with silence if partial chunk
			if n < OpusFrameBytes {
				for i := n; i < OpusFrameBytes; i++ {
					chunk[i] = 0
				}
			}

			encoded, err := s.opusCodec.Encode(chunk)
			if err != nil {
				s.logger.Error("Opus encode failed", "error", err)
				continue
			}

			if len(encoded) == 0 {
				continue
			}

			s.writeMu.Lock()
			s.packetCount++
			err = s.localTrack.WriteSample(media.Sample{
				Data:     encoded,
				Duration: pktDuration,
			})
			s.writeMu.Unlock()

			if err != nil {
				s.logger.Error("WriteSample failed", "error", err)
			}
		}
	}
}

// clearOutputBuffer clears any pending audio in the output buffer (for interruptions).
func (s *Streamer) clearOutputBuffer() {
	s.outputBufferMu.Lock()
	cleared := s.outputBuffer.Len()
	s.outputBuffer.Reset()
	s.outputBufferMu.Unlock()

	if cleared > 0 {
		s.logger.Debug("Cleared output buffer on interruption", "clearedMs", (cleared/2)*1000/OpusSampleRate)
	}
}

func (s *Streamer) sendContent(contentType, text, msgID string, completed bool) error {
	return s.sendSignaling(SignalingMessage{
		Type:      "content",
		SessionID: s.sessionID,
		Metadata: map[string]interface{}{
			"content_type": contentType,
			"text":         text,
			"message_id":   msgID,
			"completed":    completed,
		},
	})
}

// GetAudioConfig returns audio config for STT/TTS (16kHz).
func (s *Streamer) GetAudioConfig() (*protos.AudioConfig, *protos.AudioConfig) {
	cfg := internal_audio.NewLinear16khzMonoAudioConfig()
	return cfg, cfg
}

// Close closes the streamer and releases all resources.
func (s *Streamer) Close() error {
	s.cancel()

	s.mu.Lock()
	s.state = SessionStateClosed
	s.mediaConnected = false
	s.inputBuffer.Reset()
	s.mu.Unlock()

	s.sendSignaling(SignalingMessage{Type: "disconnect", SessionID: s.sessionID})

	if s.pc != nil {
		s.pc.Close()
	}

	return nil
}

// GetSessionID returns the session ID
func (s *Streamer) GetSessionID() string {
	return s.sessionID
}

// GetState returns the current session state
func (s *Streamer) GetState() SessionState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}
