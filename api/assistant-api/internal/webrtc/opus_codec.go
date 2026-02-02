// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package webrtc

import (
	"encoding/binary"
	"fmt"

	"gopkg.in/hraban/opus.v2"
)

// OpusCodec handles Opus audio encoding/decoding for WebRTC.
// Opus at 48kHz is the native WebRTC codec with best quality.
type OpusCodec struct {
	encoder    *opus.Encoder
	decoder    *opus.Decoder
	sampleRate int
	channels   int
	frameSize  int
}

// NewOpusCodec creates a new Opus codec for WebRTC.
// Uses 48kHz mono which is optimal for voice.
func NewOpusCodec() (*OpusCodec, error) {
	const (
		sampleRate = OpusSampleRate
		channels   = 1
		frameSize  = OpusFrameSamples
	)

	// Create encoder with VoIP application (optimized for speech)
	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("failed to create Opus encoder: %w", err)
	}

	// Set encoder options for low latency voice
	enc.SetBitrate(24000)     // 24 kbps - good quality for voice
	enc.SetComplexity(5)      // Balance between quality and CPU
	enc.SetInBandFEC(true)    // Forward error correction
	enc.SetPacketLossPerc(10) // Expect up to 10% packet loss
	enc.SetDTX(false)         // Disable DTX for continuous audio
	enc.SetMaxBandwidth(opus.Fullband)

	// Create decoder
	dec, err := opus.NewDecoder(sampleRate, channels)
	if err != nil {
		return nil, fmt.Errorf("failed to create Opus decoder: %w", err)
	}

	return &OpusCodec{
		encoder:    enc,
		decoder:    dec,
		sampleRate: sampleRate,
		channels:   channels,
		frameSize:  frameSize,
	}, nil
}

// Encode encodes PCM16 samples (at 48kHz) to Opus
// Input: raw PCM16 bytes (little-endian, 48kHz, mono)
// Output: Opus encoded frame
func (c *OpusCodec) Encode(pcm []byte) ([]byte, error) {
	if len(pcm) == 0 {
		return nil, nil
	}

	// Convert bytes to int16 samples
	numSamples := len(pcm) / 2
	samples := make([]int16, numSamples)
	for i := 0; i < numSamples; i++ {
		samples[i] = int16(binary.LittleEndian.Uint16(pcm[i*2 : i*2+2]))
	}

	// Encode to Opus (max output size for 20ms frame)
	output := make([]byte, 1000)
	n, err := c.encoder.Encode(samples, output)
	if err != nil {
		return nil, fmt.Errorf("Opus encode failed: %w", err)
	}

	return output[:n], nil
}

// Decode decodes Opus to PCM16 samples (48kHz)
// Input: Opus encoded frame
// Output: raw PCM16 bytes (little-endian, 48kHz, mono)
func (c *OpusCodec) Decode(encoded []byte) ([]byte, error) {
	if len(encoded) == 0 {
		return nil, nil
	}

	// Decode to samples
	samples := make([]int16, c.frameSize)
	n, err := c.decoder.Decode(encoded, samples)
	if err != nil {
		return nil, fmt.Errorf("Opus decode failed: %w", err)
	}

	// Convert samples to bytes
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:i*2+2], uint16(samples[i]))
	}

	return pcm, nil
}

// FrameSize returns the number of samples per frame
func (c *OpusCodec) FrameSize() int {
	return c.frameSize
}

// SampleRate returns the sample rate (48000)
func (c *OpusCodec) SampleRate() int {
	return c.sampleRate
}

// FrameDuration returns frame duration in milliseconds (20ms)
func (c *OpusCodec) FrameDuration() int {
	return 20
}

// FrameBytes returns the number of PCM bytes per frame.
// 20ms at 48kHz mono = 960 samples * 2 bytes = 1920 bytes.
func (c *OpusCodec) FrameBytes() int {
	return c.frameSize * 2
}

// Close releases codec resources
func (c *OpusCodec) Close() {
	// opus-go handles cleanup automatically
}
