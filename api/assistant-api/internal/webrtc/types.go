// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package webrtc

import (
	"time"

	"github.com/zaf/g711"
)

// ============================================================================
// Audio Constants
// ============================================================================

const (
	// OpusSampleRate is the sample rate for Opus codec (WebRTC standard).
	OpusSampleRate = 48000
	// STTTTSSampleRate is the sample rate for STT/TTS processing.
	STTTTSSampleRate = 16000
	// OpusFrameDuration is the frame duration in milliseconds.
	OpusFrameDuration = 20
	// OpusFrameSamples is the number of samples per frame (20ms at 48kHz).
	OpusFrameSamples = 960
	// OpusFrameBytes is the number of PCM bytes per frame (960 samples * 2 bytes).
	OpusFrameBytes = 1920
	// MaxOutputBufferBytes is the warning threshold (~10 seconds of audio).
	// Buffer can grow beyond this - it's just a logging threshold to detect issues.
	// TTS sends in bursts; output sender drains at real-time rate and catches up.
	MaxOutputBufferBytes = OpusFrameBytes * 500
)

// ============================================================================
// Configuration
// ============================================================================

// Config holds WebRTC configuration.
type Config struct {
	ICEServers         []ICEServer `json:"ice_servers"`
	ICETransportPolicy string      `json:"ice_transport_policy,omitempty"` // "all" or "relay"
	BundlePolicy       string      `json:"bundle_policy,omitempty"`        // "balanced", "max-compat", "max-bundle"
	AudioCodec         string      `json:"audio_codec,omitempty"`          // "opus", "pcmu", "pcma"
	SampleRate         int         `json:"sample_rate,omitempty"`          // 48000 for Opus
}

// ICEServer represents a STUN/TURN server configuration.
type ICEServer struct {
	URLs       []string `json:"urls"`
	Username   string   `json:"username,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// DefaultConfig returns default WebRTC configuration.
// Uses Opus codec at 48kHz for best voice quality.
func DefaultConfig() *Config {
	return &Config{
		ICEServers: []ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
		},
		ICETransportPolicy: "all",
		BundlePolicy:       "max-bundle",
		AudioCodec:         "opus",
		SampleRate:         OpusSampleRate,
	}
}

// ============================================================================
// Signaling Types
// ============================================================================

// SignalingMessage represents a WebRTC signaling message
type SignalingMessage struct {
	Type      string                 `json:"type"` // "offer", "answer", "ice_candidate", "connect", "disconnect", "config", "content", "clear", "error"
	SessionID string                 `json:"session_id,omitempty"`
	SDP       string                 `json:"sdp,omitempty"`
	Candidate *ICECandidate          `json:"candidate,omitempty"`
	Error     string                 `json:"error,omitempty"`
	Metadata  map[string]interface{} `json:"metadata,omitempty"`
}

// ICECandidate represents an ICE candidate
type ICECandidate struct {
	Candidate        string `json:"candidate"`
	SDPMid           string `json:"sdpMid"`
	SDPMLineIndex    int    `json:"sdpMLineIndex"`
	UsernameFragment string `json:"usernameFragment,omitempty"`
}

// ============================================================================
// Session State
// ============================================================================

// SessionState represents the state of a WebRTC session
type SessionState string

const (
	SessionStateNew          SessionState = "new"
	SessionStateConnecting   SessionState = "connecting"
	SessionStateConnected    SessionState = "connected"
	SessionStateDisconnected SessionState = "disconnected"
	SessionStateFailed       SessionState = "failed"
	SessionStateClosed       SessionState = "closed"
)

// SessionInfo contains information about a WebRTC session
type SessionInfo struct {
	SessionID      string       `json:"session_id"`
	State          SessionState `json:"state"`
	CreatedAt      time.Time    `json:"created_at"`
	ConnectedAt    *time.Time   `json:"connected_at,omitempty"`
	DisconnectedAt *time.Time   `json:"disconnected_at,omitempty"`
}

// ============================================================================
// Audio Codec - G.711 encoding/decoding
// ============================================================================

// Codec handles G.711 audio encoding/decoding for WebRTC
type Codec struct {
	codecType string
}

// NewCodec creates a new G.711 codec
func NewCodec(codecType string) *Codec {
	if codecType != "pcmu" && codecType != "pcma" {
		codecType = "pcmu" // default
	}
	return &Codec{codecType: codecType}
}

// Encode encodes PCM16 to G.711
func (c *Codec) Encode(pcm []byte) []byte {
	if len(pcm) == 0 {
		return pcm
	}
	if c.codecType == "pcma" {
		return g711.EncodeAlaw(pcm)
	}
	return g711.EncodeUlaw(pcm)
}

// Decode decodes G.711 to PCM16
func (c *Codec) Decode(encoded []byte) []byte {
	if len(encoded) == 0 {
		return encoded
	}
	if c.codecType == "pcma" {
		return g711.DecodeAlaw(encoded)
	}
	return g711.DecodeUlaw(encoded)
}

// Type returns the codec type
func (c *Codec) Type() string {
	return c.codecType
}
