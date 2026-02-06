// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package channel_webrtc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/google/uuid"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	pionwebrtc "github.com/pion/webrtc/v4"
	internal_audio "github.com/rapidaai/api/assistant-api/internal/audio"
	webrtc_internal "github.com/rapidaai/api/assistant-api/internal/channel/webrtc/internal"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/protos"
	"google.golang.org/grpc"
)

// ============================================================================
// GrpcStreamer - WebRTC with gRPC signaling
// ============================================================================

// GrpcStreamer implements the Streamer interface using Pion WebRTC
// with gRPC bidirectional stream for signaling instead of WebSocket.
// Audio flows through WebRTC media tracks; gRPC is used for signaling.
type GrpcStreamer struct {
	mu sync.Mutex

	// Core components
	logger     commons.Logger
	config     *webrtc_internal.Config
	grpcStream grpc.BidiStreamingServer[protos.WebTalkInput, protos.WebTalkOutput]

	// Lifecycle
	ctx    context.Context
	cancel context.CancelFunc

	// Session state
	sessionID string

	// Pion WebRTC
	pc         *pionwebrtc.PeerConnection
	localTrack *pionwebrtc.TrackLocalStaticSample

	// Audio processor for resampling, encoding, and chunking
	audioProcessor *webrtc_internal.AudioProcessor

	// Single channel for all inputs to downstream
	inputCh chan *protos.WebTalkInput
	errCh   chan error

	// Output sender state
	outputStarted bool

	// Audio processing context - cancelled on audio disconnect/reconnect
	audioCtx    context.Context
	audioCancel context.CancelFunc
	audioWg     sync.WaitGroup // Tracks audio goroutines for clean shutdown

	// Track if first configuration has been received
	// First config = initial connect (PC already created)
	// Subsequent configs = reconnect (need to recreate PC)
	firstConfigReceived bool
}

// NewGrpcStreamer creates a new WebRTC streamer with gRPC signaling
func NewWebRTCStreamer(
	ctx context.Context,
	logger commons.Logger,
	grpcStream grpc.BidiStreamingServer[protos.WebTalkInput, protos.WebTalkOutput],
) (internal_type.WebRTCStreamer, error) {
	streamerCtx, cancel := context.WithCancel(ctx)
	audioProcessor, err := webrtc_internal.NewAudioProcessor(logger, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create audio processor: %w", err)
	}
	s := &GrpcStreamer{
		logger:         logger,
		config:         webrtc_internal.DefaultConfig(),
		grpcStream:     grpcStream,
		ctx:            streamerCtx,
		cancel:         cancel,
		sessionID:      uuid.New().String(),
		audioProcessor: audioProcessor,
		inputCh:        make(chan *protos.WebTalkInput, webrtc_internal.InputChannelSize),
		errCh:          make(chan error, webrtc_internal.ErrorChannelSize),
	}

	// Set callback for processed input audio
	s.audioProcessor.SetInputAudioCallback(s.sendProcessedInputAudio)

	// Create peer connection
	if err := s.createPeerConnection(); err != nil {
		cancel()
		return nil, err
	}
	// Initiate WebRTC handshake
	if err := s.initiateWebRTCHandshake(); err != nil {
		cancel()
		s.pc.Close()
		return nil, fmt.Errorf("failed to initiate WebRTC handshake: %w", err)
	}
	// Start gRPC message reader
	go s.readGrpcMessages()
	return s, nil
}

// sendProcessedInputAudio is the callback for the audio processor
func (s *GrpcStreamer) sendProcessedInputAudio(audio []byte) {
	s.sendInput(&protos.WebTalkInput{
		Request: &protos.WebTalkInput_Message{
			Message: &protos.ConversationUserMessage{
				Message: &protos.ConversationUserMessage_Audio{Audio: audio},
			},
		},
	})
}

// ============================================================================
// Peer Connection Setup (same as WebSocket version)
// ============================================================================

// stopAudioProcessing cancels audio goroutines (runOutputSender, readRemoteAudio)
func (s *GrpcStreamer) stopAudioProcessing() {
	s.mu.Lock()
	if s.audioCancel != nil {
		s.audioCancel()
		s.audioCancel = nil
	}
	s.audioCtx = nil
	s.mu.Unlock()

	// Wait for audio goroutines to finish
	s.audioWg.Wait()
}

func (s *GrpcStreamer) createPeerConnection() error {
	// Create new audio context for this connection
	s.mu.Lock()
	s.audioCtx, s.audioCancel = context.WithCancel(s.ctx)
	s.mu.Unlock()

	mediaEngine := &pionwebrtc.MediaEngine{}

	// Opus - primary codec
	if err := mediaEngine.RegisterCodec(pionwebrtc.RTPCodecParameters{
		RTPCodecCapability: pionwebrtc.RTPCodecCapability{
			MimeType:    pionwebrtc.MimeTypeOpus,
			ClockRate:   webrtc_internal.OpusSampleRate,
			Channels:    webrtc_internal.OpusChannels,
			SDPFmtpLine: webrtc_internal.OpusSDPFmtpLine,
		},
		PayloadType: webrtc_internal.OpusPayloadType,
	}, pionwebrtc.RTPCodecTypeAudio); err != nil {
		return fmt.Errorf("failed to register Opus: %w", err)
	}

	// Note: Only Opus codec registered. PCMU/PCMA removed for simplicity.

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

	s.mu.Lock()
	s.pc = pc
	s.mu.Unlock()

	s.setupPeerEventHandlers()
	return s.createLocalTrack()
}

func (s *GrpcStreamer) setupPeerEventHandlers() {
	// ICE candidates - send via gRPC using clean proto types
	s.pc.OnICECandidate(func(c *pionwebrtc.ICECandidate) {
		if c == nil {
			return
		}
		cJSON := c.ToJSON()
		ice := &webrtc_internal.ICECandidate{Candidate: cJSON.Candidate}
		if cJSON.SDPMid != nil {
			ice.SDPMid = *cJSON.SDPMid
		}
		if cJSON.SDPMLineIndex != nil {
			ice.SDPMLineIndex = int(*cJSON.SDPMLineIndex)
		}
		if cJSON.UsernameFragment != nil {
			ice.UsernameFragment = *cJSON.UsernameFragment
		}
		s.sendICECandidate(ice)
	})

	// Connection state
	s.pc.OnConnectionStateChange(func(state pionwebrtc.PeerConnectionState) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if state == pionwebrtc.PeerConnectionStateConnected && !s.outputStarted {
			s.outputStarted = true
			go s.runOutputSender()
		}
	})

	// Remote track (incoming audio)
	s.pc.OnTrack(func(track *pionwebrtc.TrackRemote, _ *pionwebrtc.RTPReceiver) {
		if track.Kind() != pionwebrtc.RTPCodecTypeAudio {
			return
		}
		s.logger.Info("Remote audio track received", "codec", track.Codec().MimeType)
		go s.readRemoteAudio(track)
	})
}

func (s *GrpcStreamer) createLocalTrack() error {
	track, err := pionwebrtc.NewTrackLocalStaticSample(
		pionwebrtc.RTPCodecCapability{
			MimeType:  pionwebrtc.MimeTypeOpus,
			ClockRate: webrtc_internal.OpusSampleRate,
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

	s.mu.Lock()
	s.localTrack = track
	s.mu.Unlock()
	return nil
}

// ============================================================================
// Audio Processing (same as WebSocket version)
// ============================================================================

func (s *GrpcStreamer) readRemoteAudio(track *pionwebrtc.TrackRemote) {
	s.audioWg.Add(1)
	defer s.audioWg.Done()

	// Capture audioCtx at start - if it's nil, exit immediately
	s.mu.Lock()
	audioCtx := s.audioCtx
	s.mu.Unlock()

	if audioCtx == nil {
		return
	}

	// Delegate to audio processor for decoding, resampling, and buffering
	s.audioProcessor.ProcessRemoteTrack(audioCtx, track)
}

// ============================================================================
// gRPC Signaling - Using clean proto types
// ============================================================================

// sendConfig sends WebRTC configuration (ICE servers, codec info) to client
func (s *GrpcStreamer) sendConfig() error {
	iceServers := make([]*protos.ICEServer, len(s.config.ICEServers))
	for i, srv := range s.config.ICEServers {
		iceServers[i] = &protos.ICEServer{
			Urls:       srv.URLs,
			Username:   srv.Username,
			Credential: srv.Credential,
		}
	}

	return s.grpcStream.Send(&protos.WebTalkOutput{
		Code:    200,
		Success: true,
		Data: &protos.WebTalkOutput_Signaling{
			Signaling: &protos.ServerSignaling{
				SessionId: s.sessionID,
				Message: &protos.ServerSignaling_Config{
					Config: &protos.WebRTCConfig{
						IceServers: iceServers,
						AudioCodec: "opus",
						SampleRate: int32(webrtc_internal.OpusSampleRate),
					},
				},
			},
		},
	})
}

// sendOffer sends SDP offer to client
func (s *GrpcStreamer) sendOffer(sdp string) error {
	return s.grpcStream.Send(&protos.WebTalkOutput{
		Code:    200,
		Success: true,
		Data: &protos.WebTalkOutput_Signaling{
			Signaling: &protos.ServerSignaling{
				SessionId: s.sessionID,
				Message: &protos.ServerSignaling_Sdp{
					Sdp: &protos.WebRTCSDP{
						Type: protos.WebRTCSDP_OFFER,
						Sdp:  sdp,
					},
				},
			},
		},
	})
}

// sendICECandidate sends ICE candidate to client
func (s *GrpcStreamer) sendICECandidate(ice *webrtc_internal.ICECandidate) error {
	return s.grpcStream.Send(&protos.WebTalkOutput{
		Code:    200,
		Success: true,
		Data: &protos.WebTalkOutput_Signaling{
			Signaling: &protos.ServerSignaling{
				SessionId: s.sessionID,
				Message: &protos.ServerSignaling_IceCandidate{
					IceCandidate: &protos.ICECandidate{
						Candidate:        ice.Candidate,
						SdpMid:           ice.SDPMid,
						SdpMLineIndex:    int32(ice.SDPMLineIndex),
						UsernameFragment: ice.UsernameFragment,
					},
				},
			},
		},
	})
}

// sendClear sends clear/interrupt signal to client
func (s *GrpcStreamer) sendClear() error {
	return s.grpcStream.Send(&protos.WebTalkOutput{
		Code:    200,
		Success: true,
		Data: &protos.WebTalkOutput_Signaling{
			Signaling: &protos.ServerSignaling{
				SessionId: s.sessionID,
				Message:   &protos.ServerSignaling_Clear{Clear: true},
			},
		},
	})
}

// ============================================================================
// Streamer Interface Implementation
// ============================================================================

func (s *GrpcStreamer) Context() context.Context {
	return s.ctx
}

// readGrpcMessages reads from gRPC stream and routes messages
func (s *GrpcStreamer) readGrpcMessages() {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}
		msg, err := s.grpcStream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				s.sendError(io.EOF)
				return
			}
			s.sendError(err)
			return
		}

		s.handleGrpcMessage(msg)
	}
}

// sendError sends error to errCh
func (s *GrpcStreamer) sendError(err error) {
	select {
	case s.errCh <- err:
	default:
		s.logger.Debug("Error channel full, dropping error", "error", err)
	}
}

// sendInput sends input to inputCh
func (s *GrpcStreamer) sendInput(input *protos.WebTalkInput) {
	select {
	case s.inputCh <- input:
	case <-s.ctx.Done():
	}
}

// sendConfigUpstream sends configuration to upstream immediately
func (s *GrpcStreamer) sendConfigUpstream(config *protos.ConversationConfiguration) {
	audioConfig := internal_audio.NewLinear16khzMonoAudioConfig()
	config.InputConfig = &protos.StreamConfig{Audio: audioConfig}
	config.OutputConfig = &protos.StreamConfig{Audio: audioConfig}
	s.sendInput(&protos.WebTalkInput{
		Request: &protos.WebTalkInput_Configuration{
			Configuration: config,
		},
	})
}

// bufferAndSendAudio is now handled by AudioProcessor via callback

// handleGrpcMessage processes incoming gRPC message
func (s *GrpcStreamer) handleGrpcMessage(msg *protos.WebTalkInput) {
	switch msg.GetRequest().(type) {
	case *protos.WebTalkInput_Message:
		s.sendInput(msg)
		return
	case *protos.WebTalkInput_Signaling:
		s.handleClientSignaling(msg.GetSignaling())
		return
	case *protos.WebTalkInput_Configuration:
		s.handleGrpcConnect(msg.GetConfiguration())
	default:
		s.logger.Warn("Unknown gRPC message type received")
	}
}

// Recv receives the next input - simple channel read
func (s *GrpcStreamer) Recv() (*protos.WebTalkInput, error) {
	select {
	case <-s.ctx.Done():
		return nil, io.EOF
	case err := <-s.errCh:
		return nil, err
	case input := <-s.inputCh:
		return input, nil
	}
}

// handleClientSignaling processes client WebRTC signaling messages
func (s *GrpcStreamer) handleClientSignaling(signaling *protos.ClientSignaling) {
	s.mu.Lock()
	pc := s.pc
	s.mu.Unlock()

	switch msg := signaling.GetMessage().(type) {
	case *protos.ClientSignaling_Sdp:
		if msg.Sdp.GetType() == protos.WebRTCSDP_ANSWER {
			if pc == nil {
				s.logger.Warn("Received SDP answer but peer connection is nil, ignoring")
				return
			}
			if err := pc.SetRemoteDescription(pionwebrtc.SessionDescription{
				Type: pionwebrtc.SDPTypeAnswer,
				SDP:  msg.Sdp.GetSdp(),
			}); err != nil {
				s.logger.Error("Failed to set remote description", "error", err)
			}
		}

	case *protos.ClientSignaling_IceCandidate:
		if pc == nil {
			s.logger.Warn("Received ICE candidate but peer connection is nil, ignoring")
			return
		}
		ice := msg.IceCandidate
		idx := uint16(ice.GetSdpMLineIndex())
		sdpMid := ice.GetSdpMid()
		usernameFragment := ice.GetUsernameFragment()
		if err := pc.AddICECandidate(pionwebrtc.ICECandidateInit{
			Candidate:        ice.GetCandidate(),
			SDPMid:           &sdpMid,
			SDPMLineIndex:    &idx,
			UsernameFragment: &usernameFragment,
		}); err != nil {
			s.logger.Error("Failed to add ICE candidate", "error", err)
		}

	case *protos.ClientSignaling_Disconnect:
		if msg.Disconnect {
			s.sendError(io.EOF)
			s.Close()
		}
	}
}

func (s *GrpcStreamer) handleGrpcConnect(config *protos.ConversationConfiguration) {
	// Send config to upstream (talker)
	s.sendConfigUpstream(config)

	// Check if audio is requested based on InputConfig.Audio
	wantsAudio := config.GetInputConfig() != nil && config.GetInputConfig().GetAudio() != nil

	s.mu.Lock()
	isFirstConfig := !s.firstConfigReceived
	s.firstConfigReceived = true
	isAudioConnected := s.pc != nil && s.pc.ConnectionState() == pionwebrtc.PeerConnectionStateConnected
	s.mu.Unlock()

	// First configuration: PC already created and handshake initiated in NewGrpcStreamer
	if isFirstConfig {
		if wantsAudio {
			// Audio requested and handshake already in progress - nothing to do
			s.logger.Info("First configuration received, WebRTC handshake already in progress",
				"session", s.sessionID)
			return
		}
		// First config but no audio requested - clean up unnecessary PC
		s.logger.Info("First configuration received without audio, cleaning up WebRTC",
			"session", s.sessionID)
		s.stopAudioProcessing()
		s.mu.Lock()
		if s.pc != nil {
			s.pc.Close()
			s.pc = nil
		}
		s.localTrack = nil
		s.outputStarted = false
		s.mu.Unlock()
		return
	}

	s.logger.Info("Mode switch requested",
		"session", s.sessionID,
		"wantsAudio", wantsAudio,
		"isAudioConnected", isAudioConnected)

	if wantsAudio {
		if isAudioConnected {
			return
		}
		s.stopAudioProcessing()
		s.mu.Lock()
		if s.pc != nil {
			s.pc.Close()
			s.pc = nil
		}
		s.localTrack = nil
		s.outputStarted = false
		s.mu.Unlock()

		// Create new peer connection
		if err := s.createPeerConnection(); err != nil {
			s.logger.Error("Failed to create peer connection", "error", err)
			return
		}

		// Initiate WebRTC handshake (send config and SDP offer)
		if err := s.initiateWebRTCHandshake(); err != nil {
			s.logger.Error("Failed to initiate WebRTC handshake", "error", err)
		}
	} else {
		// Client wants text mode - disconnect audio if connected
		if isAudioConnected {
			s.logger.Info("Disconnecting audio for text mode", "session", s.sessionID)

			// Stop audio processing goroutines
			s.stopAudioProcessing()

			s.mu.Lock()
			if s.pc != nil {
				s.pc.Close()
				s.pc = nil
			}
			s.localTrack = nil
			s.outputStarted = false
			s.mu.Unlock()
		} else {
			s.logger.Info("Audio not connected, no action needed for text mode", "session", s.sessionID)
		}
	}
}

// initiateWebRTCHandshake sends config and creates/sends SDP offer.
func (s *GrpcStreamer) initiateWebRTCHandshake() error {
	if err := s.sendConfig(); err != nil {
		return fmt.Errorf("failed to send config: %w", err)
	}

	offer, err := s.createAndSetLocalOffer()
	if err != nil {
		return err
	}

	if err := s.sendOffer(offer.SDP); err != nil {
		return fmt.Errorf("failed to send offer: %w", err)
	}
	return nil
}

// createAndSetLocalOffer creates SDP offer and sets it as local description.
func (s *GrpcStreamer) createAndSetLocalOffer() (*pionwebrtc.SessionDescription, error) {
	offer, err := s.pc.CreateOffer(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create offer: %w", err)
	}

	if err := s.pc.SetLocalDescription(offer); err != nil {
		return nil, fmt.Errorf("failed to set local description: %w", err)
	}

	return &offer, nil
}

// Send sends output to the client

func (s *GrpcStreamer) Send(response *protos.WebTalkOutput) error {
	switch data := response.GetData().(type) {
	case *protos.WebTalkOutput_Assistant:
		switch content := data.Assistant.Message.(type) {
		case *protos.ConversationAssistantMessage_Audio:
			return s.sendAudio(content.Audio)
		case *protos.ConversationAssistantMessage_Text:
			// Send text via gRPC
			return s.grpcStream.Send(&protos.WebTalkOutput{
				Code:    200,
				Success: true,
				Data:    response.GetData(),
			})
		}

	case *protos.WebTalkOutput_Configuration:
		return s.grpcStream.Send(response)

	case *protos.WebTalkOutput_User:
		return s.grpcStream.Send(response)

	case *protos.WebTalkOutput_Interruption:
		if data.Interruption.Type == protos.ConversationInterruption_INTERRUPTION_TYPE_WORD {
			s.audioProcessor.ClearInputBuffer()
			s.audioProcessor.ClearOutputBuffer()
			// Send clear signal via WebRTC signaling
			return s.sendClear()
		}

	case *protos.WebTalkOutput_Directive:
		s.grpcStream.Send(response)
		if data.Directive.GetType() == protos.ConversationDirective_END_CONVERSATION {
			return s.Close()
		}
		return nil
	case *protos.WebTalkOutput_Error:
		return s.grpcStream.Send(response)
	}
	return nil
}

func (s *GrpcStreamer) sendAudio(audio []byte) error {
	// Delegate to audio processor for resampling and buffering
	return s.audioProcessor.ProcessOutputAudio(audio)
}

func (s *GrpcStreamer) runOutputSender() {
	s.audioWg.Add(1)
	defer s.audioWg.Done()

	// Capture audioCtx at start - if it's nil, exit immediately
	s.mu.Lock()
	audioCtx := s.audioCtx
	localTrack := s.localTrack
	s.mu.Unlock()

	if audioCtx == nil {
		return
	}

	// Delegate to audio processor for chunking and sending
	s.audioProcessor.RunOutputSender(audioCtx, localTrack)
}

// clearOutputBuffer is now handled by AudioProcessor

// Close closes the WebRTC connection
func (s *GrpcStreamer) Close() error {
	// Stop audio processing goroutines first
	s.stopAudioProcessing()

	// Cancel main context
	s.cancel()

	s.mu.Lock()
	if s.pc != nil {
		s.pc.Close()
		s.pc = nil
	}
	s.localTrack = nil
	s.mu.Unlock()

	return nil
}
