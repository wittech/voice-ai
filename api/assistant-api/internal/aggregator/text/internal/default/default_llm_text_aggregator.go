// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

// Package internal_default_aggregator provides the default sentence-boundary
// text aggregator for streaming LLM responses.
//
// The aggregator accumulates incoming text deltas, splits them at sentence
// boundaries, and emits complete sentences through a buffered channel.
// It supports multilingual punctuation (Latin, CJK, Devanagari, Arabic)
// and handles context switching between concurrent speakers/contexts.
//
// # Usage
//
//	agg, err := NewDefaultLLMTextAggregator(ctx, logger)
//	if err != nil { ... }
//	defer agg.Close()
//
//	go func() {
//	    for pkt := range agg.Result() {
//	        process(pkt)
//	    }
//	}()
//
//	agg.Aggregate(ctx, deltaPacket1, deltaPacket2, donePacket)
package internal_default_aggregator

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"

	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
)

// ============================================================================
// Constants
// ============================================================================

// sentenceBoundaries defines punctuation marks that delimit sentence endings
// across multiple writing systems: Latin, CJK, Devanagari, and Arabic.
var sentenceBoundaries = []string{
	".", "!", "?", "|", ";", ":", "…", // Latin / general
	"。", "．", // CJK full stop / fullwidth full stop
	"।", // Devanagari danda
	"۔", // Arabic full stop
}

const (
	// resultChannelSize is the buffered capacity for the output sentence channel.
	resultChannelSize = 32

	// emitBufferPrealloc is the initial capacity for the per-call emit buffer,
	// sized to avoid reallocation in the common case of a few sentences.
	emitBufferPrealloc = 8
)

// ============================================================================
// textAggregator — sentence-level LLM text aggregator
// ============================================================================

// textAggregator implements internal_type.LLMTextAggregator using regex-based
// sentence boundary detection. It accumulates streamed text deltas, extracts
// complete sentences at punctuation boundaries, and forwards them through a
// buffered channel.
//
// Thread safety: all mutable state is guarded by mu. Channel sends are
// performed outside the lock to prevent deadlocks with slow consumers.
type textAggregator struct {
	logger commons.Logger

	// result delivers aggregated sentence packets to downstream consumers.
	result chan internal_type.Packet
	closed bool

	// mu guards buffer, currentContext, closed, and toEmitBuffer.
	mu sync.Mutex

	// Buffering state: accumulates partial text until a sentence boundary is found.
	buffer         strings.Builder
	currentContext string

	// boundaryRegex is the pre-compiled pattern matching any sentence boundary
	// followed by optional trailing whitespace.
	boundaryRegex *regexp.Regexp

	// toEmitBuffer is a reusable slice that collects packets to emit during
	// a single Aggregate call, reducing per-call heap allocations.
	toEmitBuffer []internal_type.Packet
}

// NewDefaultLLMTextAggregator creates a sentence-boundary text aggregator.
//
// Sentence boundaries are statically defined to support multiple languages and
// punctuation styles (Latin, CJK, Devanagari, Arabic). The boundary regex is
// compiled once during construction.
//
// Returns an error if the boundary regex compilation fails.
func NewDefaultLLMTextAggregator(_ context.Context, logger commons.Logger) (internal_type.LLMTextAggregator, error) {
	regex, err := compileBoundaryRegex()
	if err != nil {
		return nil, err
	}

	return &textAggregator{
		logger:        logger,
		result:        make(chan internal_type.Packet, resultChannelSize),
		toEmitBuffer:  make([]internal_type.Packet, 0, emitBufferPrealloc),
		boundaryRegex: regex,
	}, nil
}

// compileBoundaryRegex builds a regex that matches any sentence boundary
// character followed by optional whitespace.
func compileBoundaryRegex() (*regexp.Regexp, error) {
	parts := make([]string, len(sentenceBoundaries))
	for i, b := range sentenceBoundaries {
		parts[i] = regexp.QuoteMeta(b)
	}

	pattern := fmt.Sprintf(`(%s)\s*`, strings.Join(parts, "|"))
	regex, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to compile sentence boundary regex: %w", err)
	}
	return regex, nil
}

// ============================================================================
// LLMTextAggregator interface implementation
// ============================================================================

// Aggregate processes one or more LLM packets and emits complete sentences
// through the Result channel.
//
// Behaviour per packet type:
//   - LLMResponseDeltaPacket: text is appended to the buffer. If a context
//     switch is detected (different ContextID), the buffer is reset first.
//     Complete sentences are extracted at boundary positions and emitted.
//   - LLMResponseDonePacket: any remaining buffered text for the active
//     context is flushed, then the done packet itself is forwarded.
//
// The method respects context cancellation: if ctx is cancelled while sending
// to the result channel, the remaining packets are dropped and ctx.Err() is
// returned.
//
// Returns an error if the aggregator has been closed.
func (st *textAggregator) Aggregate(ctx context.Context, pkts ...internal_type.LLMPacket) error {
	toEmit, resultChan, err := st.processPackets(pkts)
	if err != nil {
		return err
	}

	// Emit outside the lock to prevent deadlocks with slow consumers.
	for _, pkt := range toEmit {
		select {
		case resultChan <- pkt:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Result returns a read-only channel that receives complete sentence packets.
//
// The channel is closed when Close is called. Consumers should range over it:
//
//	for pkt := range aggregator.Result() {
//	    fmt.Println(pkt)
//	}
func (st *textAggregator) Result() <-chan internal_type.Packet {
	return st.result
}

// Close gracefully shuts down the aggregator and closes the result channel.
//
// After Close is called, subsequent Aggregate calls return an error.
// It is safe to call Close multiple times; subsequent calls are no-ops.
func (st *textAggregator) Close() error {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.closed {
		return nil
	}

	st.buffer.Reset()
	st.currentContext = ""
	close(st.result)
	st.closed = true

	return nil
}

// ============================================================================
// Internal: packet processing (lock-guarded)
// ============================================================================

// processPackets processes all packets under a single lock acquisition.
// Returns a snapshot of packets to emit and the result channel reference,
// allowing the caller to perform channel sends without holding the lock.
func (st *textAggregator) processPackets(pkts []internal_type.LLMPacket) ([]internal_type.Packet, chan internal_type.Packet, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	if st.closed {
		return nil, nil, errors.New("text aggregator is closed")
	}

	// Reset the reusable emit buffer for this call.
	st.toEmitBuffer = st.toEmitBuffer[:0]

	for _, pkt := range pkts {
		st.dispatchPacketLocked(pkt)
	}

	// Snapshot the emit buffer so the caller can send outside the lock.
	snapshot := make([]internal_type.Packet, len(st.toEmitBuffer))
	copy(snapshot, st.toEmitBuffer)

	return snapshot, st.result, nil
}

// dispatchPacketLocked routes a single LLM packet to the appropriate handler.
// MUST be called with mu held.
func (st *textAggregator) dispatchPacketLocked(pkt internal_type.LLMPacket) {
	switch input := pkt.(type) {
	case internal_type.LLMResponseDeltaPacket:
		st.handleDeltaLocked(input)
	case internal_type.LLMResponseDonePacket:
		st.handleDoneLocked(input)
	default:
		st.logger.Warnf("unsupported LLM packet type: %T", pkt)
	}
}

// handleDeltaLocked appends delta text to the buffer and extracts any
// complete sentences at boundary positions.
// MUST be called with mu held.
func (st *textAggregator) handleDeltaLocked(delta internal_type.LLMResponseDeltaPacket) {
	// Context switch: discard the previous context's partial buffer.
	if delta.ContextID != st.currentContext && st.currentContext != "" {
		st.buffer.Reset()
	}
	st.currentContext = delta.ContextID

	st.buffer.WriteString(delta.Text)
	st.extractSentencesAtBoundaryLocked(delta.ContextID)
}

// handleDoneLocked flushes any remaining buffered text for the active context,
// then forwards the done packet.
// MUST be called with mu held.
func (st *textAggregator) handleDoneLocked(done internal_type.LLMResponseDonePacket) {
	if done.ContextID == st.currentContext {
		st.flushBufferLocked(done.ContextID)
		st.currentContext = ""
	}
	st.toEmitBuffer = append(st.toEmitBuffer, done)
}

// ============================================================================
// Internal: sentence extraction and buffer management
// ============================================================================

// extractSentencesAtBoundaryLocked scans the buffer for sentence boundaries,
// emits all complete text up to the last boundary as a single delta packet,
// and retains any trailing partial sentence in the buffer.
// MUST be called with mu held.
func (st *textAggregator) extractSentencesAtBoundaryLocked(contextID string) {
	text := st.buffer.String()

	matches := st.boundaryRegex.FindAllStringIndex(text, -1)
	if len(matches) == 0 {
		return
	}

	// The last match end position is the split point between complete and
	// incomplete text.
	lastBoundaryEnd := matches[len(matches)-1][1]
	if lastBoundaryEnd == 0 {
		return
	}

	if complete := strings.TrimSpace(text[:lastBoundaryEnd]); complete != "" {
		st.toEmitBuffer = append(st.toEmitBuffer, internal_type.LLMResponseDeltaPacket{
			ContextID: contextID,
			Text:      complete,
		})
	}

	// Retain any trailing partial sentence after the last boundary.
	st.buffer.Reset()
	if lastBoundaryEnd < len(text) {
		st.buffer.WriteString(text[lastBoundaryEnd:])
	}
}

// flushBufferLocked emits any non-empty buffered text as a final delta packet
// and resets the buffer.
// MUST be called with mu held.
func (st *textAggregator) flushBufferLocked(contextID string) {
	if remaining := strings.TrimSpace(st.buffer.String()); remaining != "" {
		st.toEmitBuffer = append(st.toEmitBuffer, internal_type.LLMResponseDeltaPacket{
			ContextID: contextID,
			Text:      remaining,
		})
	}
	st.buffer.Reset()
}
