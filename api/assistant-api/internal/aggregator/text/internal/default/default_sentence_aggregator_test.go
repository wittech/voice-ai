// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package internal_default_aggregator

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
)

// collectResults drains the result channel until closed, context cancelled, or timeout.
func collectResults(ctx context.Context, resultChan <-chan internal_type.Packet) []internal_type.Packet {
	var results []internal_type.Packet
	for {
		select {
		case result, ok := <-resultChan:
			if !ok {
				return results
			}
			results = append(results, result)
		case <-ctx.Done():
			return results
		case <-time.After(100 * time.Millisecond):
			return results
		}
	}
}

func TestNewDefaultLLMTextAggregator(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()

	aggregator, err := NewDefaultLLMTextAggregator(t.Context(), logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if aggregator == nil {
		t.Fatal("aggregator is nil")
	}
	defer aggregator.Close()

	st := aggregator.(*textAggregator)
	if st.boundaryRegex == nil {
		t.Error("expected boundaryRegex to be set")
	}
}

func TestSingleText(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()
	err := aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
		ContextID: "speaker1",
		Text:      "Hello world.",
	})
	if err != nil {
		t.Fatalf("Aggregate failed: %v", err)
	}

	results := collectResults(ctx, aggregator.Result())
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	ts, ok := results[0].(internal_type.LLMResponseDeltaPacket)
	if !ok {
		t.Fatalf("unexpected result type: %T", results[0])
	}
	if ts.Text != "Hello world." {
		t.Errorf("expected 'Hello world.', got %q", ts.Text)
	}
	if ts.ContextID != "speaker1" {
		t.Errorf("expected context 'speaker1', got %q", ts.ContextID)
	}
}

func TestMultipleTexts(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()
	sentences := []string{
		"First sentence.",
		" Second sentence.",
		" Third sentence.",
	}

	go func() {
		for _, s := range sentences {
			aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
				ContextID: "speaker1",
				Text:      s,
			})
		}
	}()

	results := collectResults(ctx, aggregator.Result())
	if len(results) != 3 {
		t.Fatalf("expected 3 results, got %d", len(results))
	}

	expected := []string{"First sentence.", "Second sentence.", "Third sentence."}
	for i, result := range results {
		if ts, ok := result.(internal_type.LLMResponseDeltaPacket); ok {
			if ts.Text != expected[i] {
				t.Errorf("result %d: expected %q, got %q", i, expected[i], ts.Text)
			}
		}
	}
}

func TestMultipleBoundaries(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	ctx := context.Background()

	// When all text is sent in a single Aggregate call, the aggregator emits
	// all complete text up to the last boundary as one coalesced result.
	testCases := []struct {
		input        string
		expected     int
		expectedText string
	}{
		{"What a day!", 1, "What a day!"},
		{"Is this real?", 1, "Is this real?"},
		{"Sure; let's go.", 1, "Sure; let's go."},
		{"One. Two? Three!", 1, "One. Two? Three!"},
	}

	for _, tc := range testCases {
		aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)

		err := aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "speaker1",
			Text:      tc.input,
		})
		if err != nil {
			t.Fatalf("Aggregate failed: %v", err)
		}

		results := collectResults(ctx, aggregator.Result())

		if len(results) != tc.expected {
			t.Errorf("input %q: got %d results (expected %d)", tc.input, len(results), tc.expected)
		}

		if len(results) > 0 {
			if ts, ok := results[0].(internal_type.LLMResponseDeltaPacket); ok {
				if ts.Text != tc.expectedText {
					t.Errorf("input %q: expected text %q, got %q", tc.input, tc.expectedText, ts.Text)
				}
			}
		}

		aggregator.Close()
	}
}

func TestUnicodeBoundaries(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	ctx := context.Background()

	// When all text is sent in a single Aggregate call, the aggregator coalesces
	// all complete text up to the last boundary into one result.
	testCases := []struct {
		name         string
		input        string
		expected     int
		expectedText string
	}{
		{"Japanese period", "こんにちは。元気ですか。", 1, "こんにちは。元気ですか。"},
		{"Devanagari danda", "नमस्ते। कैसे हैं।", 1, "नमस्ते। कैसे हैं।"},
		{"Ellipsis", "Wait… Really…", 1, "Wait… Really…"},
		{"Fullwidth period", "テスト．完了．", 1, "テスト．完了．"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)

			err := aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
				ContextID: "speaker1",
				Text:      tc.input,
			})
			if err != nil {
				t.Fatalf("Aggregate failed: %v", err)
			}

			results := collectResults(ctx, aggregator.Result())

			if len(results) != tc.expected {
				t.Errorf("input %q: got %d results (expected %d)", tc.input, len(results), tc.expected)
			}

			if len(results) > 0 {
				if ts, ok := results[0].(internal_type.LLMResponseDeltaPacket); ok {
					if ts.Text != tc.expectedText {
						t.Errorf("input %q: expected text %q, got %q", tc.input, tc.expectedText, ts.Text)
					}
				}
			}

			aggregator.Close()
		})
	}
}

func TestContextSwitching(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()

	go func() {
		aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "speaker1",
			Text:      "Hello there.",
		})
		aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "speaker2",
			Text:      "Goodbye.",
		})
	}()

	results := collectResults(ctx, aggregator.Result())
	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	foundSpeaker1, foundSpeaker2 := false, false
	for _, result := range results {
		if ts, ok := result.(internal_type.LLMResponseDeltaPacket); ok {
			switch ts.ContextID {
			case "speaker1":
				foundSpeaker1 = true
				if ts.Text != "Hello there." {
					t.Errorf("speaker1 expected 'Hello there.', got %q", ts.Text)
				}
			case "speaker2":
				foundSpeaker2 = true
				if ts.Text != "Goodbye." {
					t.Errorf("speaker2 expected 'Goodbye.', got %q", ts.Text)
				}
			}
		}
	}
	if !foundSpeaker1 {
		t.Error("expected to find speaker1 result")
	}
	if !foundSpeaker2 {
		t.Error("expected to find speaker2 result")
	}
}

func TestDonePacketFlush(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()

	go func() {
		aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "speaker1",
			Text:      "This is incomplete",
		})
		aggregator.Aggregate(ctx, internal_type.LLMResponseDonePacket{
			ContextID: "speaker1",
		})
	}()

	results := collectResults(ctx, aggregator.Result())
	if len(results) != 2 {
		t.Fatalf("expected 2 results (flushed text + done), got %d", len(results))
	}

	if ts, ok := results[0].(internal_type.LLMResponseDeltaPacket); !ok {
		t.Errorf("expected first result to be LLMResponseDeltaPacket, got %T", results[0])
	} else if ts.Text != "This is incomplete" {
		t.Errorf("expected flushed text 'This is incomplete', got %q", ts.Text)
	}

	if _, ok := results[1].(internal_type.LLMResponseDonePacket); !ok {
		t.Errorf("expected second result to be LLMResponseDonePacket, got %T", results[1])
	}
}

func TestEmptyInput(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()
	err := aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
		ContextID: "speaker1",
		Text:      "",
	})
	if err != nil {
		t.Fatalf("Aggregate should not error on empty input: %v", err)
	}

	results := collectResults(ctx, aggregator.Result())
	if len(results) != 0 {
		t.Errorf("expected 0 results for empty input, got %d", len(results))
	}
}

func TestContextCancellation(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
		ContextID: "speaker1",
		Text:      "Hello.",
	})

	// When context is already cancelled, Aggregate may or may not return an error
	// depending on whether the channel send or ctx.Done() wins the select race.
	if err != nil && err != context.Canceled {
		t.Errorf("expected nil or context.Canceled, got %v", err)
	}
}

func TestConcurrentContexts(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()
	var wg sync.WaitGroup
	resultCount := atomic.Int32{}

	for speaker := 0; speaker < 3; speaker++ {
		wg.Add(1)
		go func(speakerID int) {
			defer wg.Done()
			contextID := fmt.Sprintf("speaker%d", speakerID)
			for i := 0; i < 3; i++ {
				aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
					ContextID: contextID,
					Text:      fmt.Sprintf("Text %d.", i),
				})
			}
		}(speaker)
	}

	go func() {
		wg.Wait()
		aggregator.Close()
	}()

	for range aggregator.Result() {
		resultCount.Add(1)
	}

	if resultCount.Load() == 0 {
		t.Error("expected to receive some results from concurrent aggregation")
	}
}

func TestBufferStateMaintenance(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()

	go func() {
		aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "speaker1",
			Text:      "Hello",
		})
		aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "speaker1",
			Text:      " world",
		})
		aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "speaker1",
			Text:      ".",
		})
	}()

	results := collectResults(ctx, aggregator.Result())
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	if ts, ok := results[0].(internal_type.LLMResponseDeltaPacket); ok && ts.Text != "Hello world." {
		t.Errorf("expected 'Hello world.', got %q", ts.Text)
	}
}

func TestWhitespaceHandling(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()

	go func() {
		aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "speaker1",
			Text:      "Hello.   \n  ",
		})
		aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "speaker1",
			Text:      "World.",
		})
	}()

	results := collectResults(ctx, aggregator.Result())
	if len(results) < 1 {
		t.Fatalf("expected at least 1 result, got %d", len(results))
	}

	if ts, ok := results[0].(internal_type.LLMResponseDeltaPacket); ok && ts.Text != "Hello." {
		t.Errorf("expected 'Hello.', got %q", ts.Text)
	}
}

func TestMultipleClose(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)

	err1 := aggregator.Close()
	err2 := aggregator.Close()

	if err1 != nil || err2 != nil {
		t.Errorf("Close should not error on multiple calls: err1=%v, err2=%v", err1, err2)
	}
}

func TestResultChannelClosure(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)

	resultChan := aggregator.Result()
	aggregator.Close()

	_, ok := <-resultChan
	if ok {
		t.Error("expected channel to be closed")
	}
}

func TestSpecialCharacterBoundaries(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()

	go func() {
		aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "speaker1",
			Text:      "Really?",
		})
	}()

	results := collectResults(ctx, aggregator.Result())
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	if ts, ok := results[0].(internal_type.LLMResponseDeltaPacket); ok && ts.Text != "Really?" {
		t.Errorf("special character boundary failed: got %q", ts.Text)
	}
}

func TestLargeBatch(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()
	const batchSize = 100

	go func() {
		for i := 0; i < batchSize; i++ {
			aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
				ContextID: "speaker1",
				Text:      fmt.Sprintf("Text %d.", i),
			})
		}
	}()

	results := collectResults(ctx, aggregator.Result())
	if len(results) != batchSize {
		t.Errorf("expected %d results, got %d", batchSize, len(results))
	}
}

func TestLLMStreamingInput(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()

	llmChunks := []string{
		"Hello",
		" world",
		", this",
		" is",
		" an",
		" LLM",
		" streamed",
		" sentence",
		".",
		" Another",
		" one",
		"!",
	}

	done := make(chan bool)
	var results []internal_type.Packet

	go func() {
		resultChan := aggregator.Result()
		for {
			select {
			case result, ok := <-resultChan:
				if !ok {
					done <- true
					return
				}
				results = append(results, result)
			case <-time.After(500 * time.Millisecond):
				done <- true
				return
			}
		}
	}()

	go func() {
		for _, chunk := range llmChunks {
			if err := aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
				ContextID: "llm",
				Text:      chunk,
			}); err != nil {
				t.Errorf("Aggregate failed: %v", err)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		aggregator.Close()
	}()

	<-done

	if len(results) != 2 {
		t.Fatalf("expected 2 sentences from LLM stream, got %d", len(results))
	}

	expected := []string{
		"Hello world, this is an LLM streamed sentence.",
		"Another one!",
	}

	for i, r := range results {
		ts, ok := r.(internal_type.LLMResponseDeltaPacket)
		if !ok {
			t.Errorf("result %d: unexpected type %T", i, r)
			continue
		}
		if ts.ContextID != "llm" {
			t.Errorf("result %d: expected context 'llm', got %q", i, ts.ContextID)
		}
		if ts.Text != expected[i] {
			t.Errorf("result %d: expected %q, got %q", i, expected[i], ts.Text)
		}
	}
}

func TestLLMStreamingWithPauses(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()
	chunks := []string{"This", " sentence", " arrives", " slowly", "."}

	var results []internal_type.Packet
	go func() {
		for _, chunk := range chunks {
			time.Sleep(50 * time.Millisecond)
			_ = aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
				ContextID: "llm",
				Text:      chunk,
			})
		}
		aggregator.Close()
	}()

	for r := range aggregator.Result() {
		results = append(results, r)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 sentence, got %d", len(results))
	}

	if ts, ok := results[0].(internal_type.LLMResponseDeltaPacket); ok && ts.Text != "This sentence arrives slowly." {
		t.Errorf("unexpected sentence: %q", ts.Text)
	}
}

func TestLLMStreamingWithContextSwitch(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()
	var results []internal_type.Packet

	go func() {
		_ = aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "llm-A",
			Text:      "LLM A is speaking.",
		})
		_ = aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "llm-B",
			Text:      "Hello from B.",
		})
		aggregator.Close()
	}()

	for r := range aggregator.Result() {
		results = append(results, r)
	}

	if len(results) < 2 {
		t.Fatalf("expected at least 2 results, got %d", len(results))
	}

	if ts, ok := results[0].(internal_type.LLMResponseDeltaPacket); ok {
		if ts.ContextID != "llm-A" {
			t.Errorf("expected first result from llm-A, got %s", ts.ContextID)
		}
	}

	foundB := false
	for _, r := range results {
		if ts, ok := r.(internal_type.LLMResponseDeltaPacket); ok && ts.ContextID == "llm-B" {
			foundB = true
		}
	}
	if !foundB {
		t.Error("expected output from llm-B after context switch")
	}
}

func TestLLMStreamingForcedCompletion(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()
	var results []internal_type.Packet

	go func() {
		_ = aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
			ContextID: "llm",
			Text:      "This sentence never ends",
		})
		_ = aggregator.Aggregate(ctx, internal_type.LLMResponseDonePacket{
			ContextID: "llm",
		})
		aggregator.Close()
	}()

	for r := range aggregator.Result() {
		results = append(results, r)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results (flushed text + done), got %d", len(results))
	}

	if ts, ok := results[0].(internal_type.LLMResponseDeltaPacket); !ok {
		t.Errorf("expected first result to be LLMResponseDeltaPacket, got %T", results[0])
	} else if ts.Text != "This sentence never ends" {
		t.Errorf("expected flushed text 'This sentence never ends', got %q", ts.Text)
	}

	if _, ok := results[1].(internal_type.LLMResponseDonePacket); !ok {
		t.Errorf("expected second result to be LLMResponseDonePacket, got %T", results[1])
	}
}

func TestLLMStreamingUnformattedButComplete(t *testing.T) {
	logger, _ := commons.NewApplicationLogger()
	aggregator, _ := NewDefaultLLMTextAggregator(t.Context(), logger)
	defer aggregator.Close()

	ctx := context.Background()
	var results []internal_type.Packet

	go func() {
		chunks := []string{"this", " is", " a", " raw", " llm", " response"}
		for _, chunk := range chunks {
			_ = aggregator.Aggregate(ctx, internal_type.LLMResponseDeltaPacket{
				ContextID: "llm",
				Text:      chunk,
			})
		}
		_ = aggregator.Aggregate(ctx, internal_type.LLMResponseDonePacket{
			ContextID: "llm",
		})
		aggregator.Close()
	}()

	for r := range aggregator.Result() {
		results = append(results, r)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results (flushed text + done), got %d", len(results))
	}

	if ts, ok := results[0].(internal_type.LLMResponseDeltaPacket); !ok {
		t.Errorf("expected first result to be LLMResponseDeltaPacket, got %T", results[0])
	} else if ts.Text != "this is a raw llm response" {
		t.Errorf("expected flushed text 'this is a raw llm response', got %q", ts.Text)
	}

	if _, ok := results[1].(internal_type.LLMResponseDonePacket); !ok {
		t.Errorf("expected second result to be LLMResponseDonePacket, got %T", results[1])
	}
}
