// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.
package internal_recorder

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
	"time"

	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
)

// Audio configuration constants
const (
	AudioSampleRate     = 16000 // 16kHz sample rate
	AudioChannels       = 1     // Mono
	AudioBytesPerSample = 2     // 16-bit (2 bytes)
	AudioBitsPerSample  = 16    // 16-bit PCM
	AudioPCMFormat      = 1     // PCM format code
)

// AudioChunk represents a timestamped audio chunk
type AudioChunk struct {
	Data      []byte
	Timestamp time.Time
	IsSystem  bool  // true for system audio, false for user audio
	ID        int64 // Add unique identifier for each chunk
}

type audioRecorder struct {
	logger            commons.Logger
	mu                sync.Mutex   // Ensure thread-safe access
	userAudioChunks   []AudioChunk // User audio chunks with timestamps
	systemAudioChunks []AudioChunk // System audio chunks with timestamps
	chunkIDCounter    int64        // Counter for unique chunk IDs
}

func NewDefaultAudioRecorder(logger commons.Logger) (internal_type.Recorder, error) {
	return &audioRecorder{
		logger:            logger,
		userAudioChunks:   []AudioChunk{},
		systemAudioChunks: []AudioChunk{},
		mu:                sync.Mutex{},
	}, nil
}

func (r *audioRecorder) Record(ctx context.Context, p internal_type.Packet) error {
	switch vl := p.(type) {
	case internal_type.UserAudioPacket:
		return r.user(vl.Audio)
	case internal_type.InterruptionPacket:
		return r.interrupt()
	case internal_type.TextToSpeechAudioPacket:
		return r.system(vl.AudioChunk)
	default:
	}
	return nil
}

func (r *audioRecorder) user(in []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.chunkIDCounter++
	chunk := AudioChunk{
		Data:      make([]byte, len(in)),
		Timestamp: time.Now(),
		IsSystem:  false,
		ID:        r.chunkIDCounter,
	}
	copy(chunk.Data, in)
	r.userAudioChunks = append(r.userAudioChunks, chunk)
	return nil
}

func (r *audioRecorder) interrupt() error {
	// No-op: interruptions are not handled in independent audio mode
	return nil
}

func (r *audioRecorder) system(out []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.chunkIDCounter++
	chunk := AudioChunk{
		Data:      make([]byte, len(out)),
		Timestamp: time.Now(),
		IsSystem:  true,
		ID:        r.chunkIDCounter,
	}
	copy(chunk.Data, out)
	r.systemAudioChunks = append(r.systemAudioChunks, chunk)
	return nil
}

// Helper function to merge user audio chunks into a single byte buffer (LINEAR16)
func (r *audioRecorder) mergeUserAudio() ([]byte, error) {
	if len(r.userAudioChunks) == 0 {
		return nil, fmt.Errorf("no user audio chunks to persist")
	}

	// Sort chunks by timestamp to maintain chronological order
	sort.Slice(r.userAudioChunks, func(i, j int) bool {
		return r.userAudioChunks[i].Timestamp.Before(r.userAudioChunks[j].Timestamp)
	})

	var buffer bytes.Buffer
	for _, chunk := range r.userAudioChunks {
		buffer.Write(chunk.Data)
	}

	return buffer.Bytes(), nil
}

// Helper function to merge system audio chunks into a single byte buffer (LINEAR16)
func (r *audioRecorder) mergeSystemAudio() ([]byte, error) {
	if len(r.systemAudioChunks) == 0 {
		return nil, fmt.Errorf("no system audio chunks to persist")
	}

	// Sort chunks by timestamp to maintain chronological order
	sort.Slice(r.systemAudioChunks, func(i, j int) bool {
		return r.systemAudioChunks[i].Timestamp.Before(r.systemAudioChunks[j].Timestamp)
	})

	var buffer bytes.Buffer
	for _, chunk := range r.systemAudioChunks {
		buffer.Write(chunk.Data)
	}

	return buffer.Bytes(), nil
}

// Persist returns both user and system audio as WAV files encoded in JSON
// The returned byte array contains a JSON object with both audio tracks
func (r *audioRecorder) Persist() ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.userAudioChunks) == 0 && len(r.systemAudioChunks) == 0 {
		return nil, nil, fmt.Errorf("no audio chunks to persist")
	}

	var err error
	var (
		userAudio   []byte
		systemAudio []byte
	)
	if len(r.userAudioChunks) > 0 {
		// Merge user audio chunks
		sort.Slice(r.userAudioChunks, func(i, j int) bool {
			return r.userAudioChunks[i].Timestamp.Before(r.userAudioChunks[j].Timestamp)
		})

		var userBuffer bytes.Buffer
		for _, chunk := range r.userAudioChunks {
			userBuffer.Write(chunk.Data)
		}

		userAudio, err = r.createWAVFile(userBuffer.Bytes(), AudioSampleRate, AudioChannels, AudioBytesPerSample)
		if err != nil {
			r.logger.Error("Failed to create user WAV file", err)
		}
	}

	if len(r.systemAudioChunks) > 0 {
		// Merge system audio chunks
		sort.Slice(r.systemAudioChunks, func(i, j int) bool {
			return r.systemAudioChunks[i].Timestamp.Before(r.systemAudioChunks[j].Timestamp)
		})

		var systemBuffer bytes.Buffer
		for _, chunk := range r.systemAudioChunks {
			systemBuffer.Write(chunk.Data)
		}

		systemAudio, err = r.createWAVFile(systemBuffer.Bytes(), AudioSampleRate, AudioChannels, AudioBytesPerSample)
		if err != nil {
			r.logger.Error("Failed to create system WAV file", err)
		}
	}
	return userAudio, systemAudio, nil
}

// createWAVFile creates a WAV file from PCM audio data (LINEAR16 format)
func (r *audioRecorder) createWAVFile(pcmData []byte, sampleRate int, channels int, bytesPerSample int) ([]byte, error) {
	var buf bytes.Buffer

	// WAV header
	// RIFF header
	buf.Write([]byte("RIFF"))
	binary.Write(&buf, binary.LittleEndian, uint32(36+len(pcmData))) // File size - 8
	buf.Write([]byte("WAVE"))

	// fmt chunk
	buf.Write([]byte("fmt "))
	binary.Write(&buf, binary.LittleEndian, uint32(16))                                 // fmt chunk size
	binary.Write(&buf, binary.LittleEndian, uint16(AudioPCMFormat))                     // PCM format code
	binary.Write(&buf, binary.LittleEndian, uint16(channels))                           // Number of channels
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))                         // Sample rate (Hz)
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*channels*bytesPerSample)) // Byte rate
	binary.Write(&buf, binary.LittleEndian, uint16(channels*bytesPerSample))            // Block align
	binary.Write(&buf, binary.LittleEndian, uint16(AudioBitsPerSample))                 // Bits per sample

	// data chunk
	buf.Write([]byte("data"))
	binary.Write(&buf, binary.LittleEndian, uint32(len(pcmData)))
	buf.Write(pcmData)

	return buf.Bytes(), nil
}
