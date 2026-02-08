// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package webrtc_internal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/pion/rtp"
	pionwebrtc "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	internal_audio "github.com/rapidaai/api/assistant-api/internal/audio"
	internal_audio_resampler "github.com/rapidaai/api/assistant-api/internal/audio/resampler"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/protos"
)

// AudioChunk represents a processed audio chunk ready for streaming
type AudioChunk struct {
	Data     []byte
	Duration time.Duration
}

// AudioProcessorConfig holds audio processor configuration
type AudioProcessorConfig struct {
	// Input buffering threshold in bytes (default: 1920 = 60ms at 16kHz)
	InputBufferThreshold int
	// Output chunk size in bytes (default: OpusFrameBytes = 1920)
	OutputChunkSize int
	// Output chunk duration (default: 20ms)
	OutputChunkDuration time.Duration
}

// DefaultAudioProcessorConfig returns default audio processor configuration
func DefaultAudioProcessorConfig() *AudioProcessorConfig {
	return &AudioProcessorConfig{
		InputBufferThreshold: InputBufferThreshold,
		OutputChunkSize:      OpusFrameBytes,
		OutputChunkDuration:  OpusFrameDuration * time.Millisecond,
	}
}

// AudioProcessor handles audio resampling, encoding, and chunking for WebRTC
type AudioProcessor struct {
	logger commons.Logger
	config *AudioProcessorConfig

	// Resampler for sample rate conversion
	resampler internal_type.AudioResampler

	// Audio configs
	opusConfig          *protos.AudioConfig // 48kHz mono for Opus/WebRTC
	internalAudioConfig *protos.AudioConfig // 16kHz mono for STT/TTS

	// Opus codec for encoding/decoding
	opusCodec *OpusCodec

	// Input buffer for accumulating incoming audio
	inputBuffer   *bytes.Buffer
	inputBufferMu sync.Mutex

	// Output buffer for audio to be sent
	outputBuffer   *bytes.Buffer
	outputBufferMu sync.Mutex

	// Callback for processed input audio
	onInputAudio func(audio []byte)
}

// NewAudioProcessor creates a new audio processor
func NewAudioProcessor(logger commons.Logger, config *AudioProcessorConfig) (*AudioProcessor, error) {
	if config == nil {
		config = DefaultAudioProcessorConfig()
	}

	resampler, err := internal_audio_resampler.GetResampler(logger)
	if err != nil {
		return nil, fmt.Errorf("failed to create resampler: %w", err)
	}

	opusCodec, err := NewOpusCodec()
	if err != nil {
		return nil, fmt.Errorf("failed to create Opus codec: %w", err)
	}

	return &AudioProcessor{
		logger:              logger,
		config:              config,
		resampler:           resampler,
		opusConfig:          internal_audio.NewLinear48khzMonoAudioConfig(),
		internalAudioConfig: internal_audio.NewLinear16khzMonoAudioConfig(),
		opusCodec:           opusCodec,
		inputBuffer:         new(bytes.Buffer),
		outputBuffer:        new(bytes.Buffer),
	}, nil
}

// SetInputAudioCallback sets the callback for processed input audio
func (p *AudioProcessor) SetInputAudioCallback(callback func(audio []byte)) {
	p.onInputAudio = callback
}

// ============================================================================
// Input Audio Processing (from WebRTC client -> server)
// ============================================================================

// ProcessRemoteTrack reads from a WebRTC remote track, decodes, resamples,
// and sends to the input callback. Runs until context is cancelled.
func (p *AudioProcessor) ProcessRemoteTrack(ctx context.Context, track *pionwebrtc.TrackRemote) {
	buf := make([]byte, RTPBufferSize)
	mimeType := track.Codec().MimeType

	// Only Opus is supported
	if mimeType != pionwebrtc.MimeTypeOpus {
		p.logger.Error("Unsupported codec, only Opus is supported", "codec", mimeType)
		return
	}

	opusDecoder, err := NewOpusCodec()
	if err != nil {
		p.logger.Error("Failed to create Opus decoder", "error", err)
		return
	}
	consecutiveErrors := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, _, err := track.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			consecutiveErrors++
			if consecutiveErrors >= MaxConsecutiveErrors {
				p.logger.Error("Too many consecutive read errors, stopping audio reader", "lastError", err)
				return
			}
			continue
		}
		consecutiveErrors = 0

		pkt := &rtp.Packet{}
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			p.logger.Debug("Failed to unmarshal RTP packet", "error", err)
			continue
		}
		if len(pkt.Payload) == 0 {
			continue
		}

		// Decode Opus to PCM (48kHz)
		pcm, err := opusDecoder.Decode(pkt.Payload)
		if err != nil {
			p.logger.Debug("Opus decode failed", "error", err)
			continue
		}

		// Resample from 48kHz to 16kHz for STT
		resampled, err := p.ResampleInput(pcm)
		if err != nil {
			p.logger.Debug("Audio resample failed", "error", err)
			continue
		}

		// Buffer and send when threshold reached
		p.bufferAndSendInput(resampled)
	}
}

// ResampleInput resamples audio from 48kHz (Opus/WebRTC) to 16kHz (STT)
func (p *AudioProcessor) ResampleInput(audio []byte) ([]byte, error) {
	return p.resampler.Resample(audio, p.opusConfig, p.internalAudioConfig)
}

// bufferAndSendInput buffers input audio and sends when threshold is reached
func (p *AudioProcessor) bufferAndSendInput(audio []byte) {
	p.inputBufferMu.Lock()
	p.inputBuffer.Write(audio)

	if p.inputBuffer.Len() < p.config.InputBufferThreshold {
		p.inputBufferMu.Unlock()
		return
	}

	audioData := make([]byte, p.inputBuffer.Len())
	p.inputBuffer.Read(audioData)
	p.inputBufferMu.Unlock()

	if p.onInputAudio != nil {
		p.onInputAudio(audioData)
	}
}

// ClearInputBuffer clears the input audio buffer
func (p *AudioProcessor) ClearInputBuffer() {
	p.inputBufferMu.Lock()
	p.inputBuffer.Reset()
	p.inputBufferMu.Unlock()
}

// ============================================================================
// Output Audio Processing (from server -> WebRTC client)
// ============================================================================

// ProcessOutputAudio resamples audio from 16kHz (TTS) to 48kHz (Opus/WebRTC)
// and buffers it for chunked streaming
func (p *AudioProcessor) ProcessOutputAudio(audio []byte) error {
	if len(audio) == 0 {
		return nil
	}

	// Resample from 16kHz to 48kHz for Opus
	audio48kHz, err := p.ResampleOutput(audio)
	if err != nil {
		p.logger.Error("Resample to 48kHz failed", "error", err)
		return err
	}

	p.outputBufferMu.Lock()
	defer p.outputBufferMu.Unlock()

	// Buffer grows dynamically - audio will be drained at 20ms intervals
	// by RunOutputSender. No dropping to ensure complete audio playback.
	p.outputBuffer.Write(audio48kHz)
	return nil
}

// ResampleOutput resamples audio from 16kHz (TTS) to 48kHz (Opus/WebRTC)
func (p *AudioProcessor) ResampleOutput(audio []byte) ([]byte, error) {
	return p.resampler.Resample(audio, p.internalAudioConfig, p.opusConfig)
}

// GetNextChunk retrieves the next audio chunk from the output buffer,
// encodes it to Opus, and returns it ready for streaming.
// Returns nil if no audio is available.
func (p *AudioProcessor) GetNextChunk() *AudioChunk {
	chunk := make([]byte, p.config.OutputChunkSize)

	p.outputBufferMu.Lock()
	n, _ := p.outputBuffer.Read(chunk)
	p.outputBufferMu.Unlock()

	if n == 0 {
		return nil
	}

	// Pad with silence if chunk is not full
	if n < p.config.OutputChunkSize {
		for i := n; i < p.config.OutputChunkSize; i++ {
			chunk[i] = 0
		}
	}

	// Encode to Opus
	encoded, err := p.opusCodec.Encode(chunk)
	if err != nil {
		p.logger.Debug("Opus encode failed", "error", err)
		return nil
	}

	return &AudioChunk{
		Data:     encoded,
		Duration: p.config.OutputChunkDuration,
	}
}

// ChunkAndEncode takes raw 48kHz PCM audio and returns a slice of encoded
// Opus audio chunks ready for streaming. This is useful for pre-processing
// large audio buffers before streaming.
func (p *AudioProcessor) ChunkAndEncode(audio48kHz []byte) ([]*AudioChunk, error) {
	if len(audio48kHz) == 0 {
		return nil, nil
	}

	chunkSize := p.config.OutputChunkSize
	numChunks := (len(audio48kHz) + chunkSize - 1) / chunkSize
	chunks := make([]*AudioChunk, 0, numChunks)

	for offset := 0; offset < len(audio48kHz); offset += chunkSize {
		end := offset + chunkSize
		if end > len(audio48kHz) {
			end = len(audio48kHz)
		}

		chunk := make([]byte, chunkSize)
		copy(chunk, audio48kHz[offset:end])

		// Pad with silence if chunk is not full
		if end-offset < chunkSize {
			for i := end - offset; i < chunkSize; i++ {
				chunk[i] = 0
			}
		}

		// Encode to Opus
		encoded, err := p.opusCodec.Encode(chunk)
		if err != nil {
			return chunks, fmt.Errorf("opus encode failed at offset %d: %w", offset, err)
		}

		chunks = append(chunks, &AudioChunk{
			Data:     encoded,
			Duration: p.config.OutputChunkDuration,
		})
	}

	return chunks, nil
}

// RunOutputSender continuously sends audio chunks from the output buffer
// to the provided WebRTC track at consistent 20ms intervals.
// Runs until context is cancelled.
// IMPORTANT: WebRTC requires consistent timing - we must send packets at
// regular intervals even if there's no audio (send silence in that case).
func (p *AudioProcessor) RunOutputSender(ctx context.Context, track *pionwebrtc.TrackLocalStaticSample) {
	if track == nil {
		p.logger.Error("RunOutputSender called with nil track")
		return
	}

	chunkDuration := p.config.OutputChunkDuration
	nextSendTime := time.Now().Add(chunkDuration)

	// Pre-encode a silence chunk for when buffer is empty
	silenceChunk := p.createSilenceChunk()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Wait until next send time with precision
		now := time.Now()
		if sleepDuration := nextSendTime.Sub(now); sleepDuration > 0 {
			time.Sleep(sleepDuration)
		}

		// Schedule next send immediately to minimize drift
		nextSendTime = nextSendTime.Add(chunkDuration)

		// If we've fallen behind, reset timing (don't try to catch up)
		if time.Now().After(nextSendTime) {
			nextSendTime = time.Now().Add(chunkDuration)
		}

		// Get audio chunk or use silence
		chunk := p.GetNextChunk()
		if chunk == nil {
			chunk = silenceChunk
		}

		// Write sample to track - this should never be skipped
		if err := track.WriteSample(media.Sample{
			Data:     chunk.Data,
			Duration: chunk.Duration,
		}); err != nil {
			p.logger.Debug("Failed to write sample to track", "error", err)
		}
	}
}

// createSilenceChunk creates an encoded silence chunk for padding
func (p *AudioProcessor) createSilenceChunk() *AudioChunk {
	// Create silent PCM data (all zeros)
	silence := make([]byte, p.config.OutputChunkSize)

	// Encode silence to Opus
	encoded, err := p.opusCodec.Encode(silence)
	if err != nil {
		p.logger.Error("Failed to encode silence chunk", "error", err)
		// Return raw silence as fallback (shouldn't happen)
		return &AudioChunk{
			Data:     silence,
			Duration: p.config.OutputChunkDuration,
		}
	}

	return &AudioChunk{
		Data:     encoded,
		Duration: p.config.OutputChunkDuration,
	}
}

// ClearOutputBuffer clears the output audio buffer
func (p *AudioProcessor) ClearOutputBuffer() {
	p.outputBufferMu.Lock()
	p.outputBuffer.Reset()
	p.outputBufferMu.Unlock()
}
