// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package qwen3asr

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/utils"
	"github.com/rapidaai/protos"
)

type qwen3AsrSTT struct {
	opt      *qwen3AsrOption
	logger   commons.Logger
	onPacket func(pkt ...internal_type.Packet) error

	mu            sync.Mutex
	conn          *websocket.Conn
	isStreaming   bool
	isInitialized bool
}

// Qwen3 protocol message types
type Qwen3MessageType string

const (
	Qwen3MsgTypeStart      Qwen3MessageType = "start"
	Qwen3MsgTypeResult     Qwen3MessageType = "result"
	Qwen3MsgTypeSegmentEnd Qwen3MessageType = "segment_end"
	Qwen3MsgTypeFinal      Qwen3MessageType = "final"
	Qwen3MsgTypeStarted    Qwen3MessageType = "started"
	Qwen3MsgTypeStop       Qwen3MessageType = "stop"
	Qwen3MsgTypeError      Qwen3MessageType = "error"
)

// Start request payload
type Qwen3StartPayload struct {
	Format          string  `json:"format"`
	SampleRate      int     `json:"sample_rate"`
	Language        string  `json:"language,omitempty"`
	Context         string  `json:"context,omitempty"`
	ChunkSizeSec    float64 `json:"chunk_size_sec"`
	UnfixedChunkNum int     `json:"unfixed_chunk_num"`
	UnfixedTokenNum int     `json:"unfixed_token_num"`
}

// Start request message
type Qwen3StartMessage struct {
	Type    string            `json:"type"`
	Payload Qwen3StartPayload `json:"payload"`
}

// Result message from server
type Qwen3ResultMessage struct {
	Type                   string            `json:"type"`
	TaskID                 string            `json:"task_id"`
	Results                []Qwen3ResultItem `json:"results"`
	SegmentIndex           int               `json:"segment_index"`
	ConfirmedSegmentsCount int               `json:"confirmed_segments_count"`
}

type Qwen3ResultItem struct {
	Text               string `json:"text"`
	CurrentSegmentText string `json:"current_segment_text"`
	Language           string `json:"language"`
	ChunkID            int    `json:"chunk_id"`
	IsPartial          bool   `json:"is_partial"`
	SegmentIndex       int    `json:"segment_index"`
}

// Segment end message
type Qwen3SegmentEndMessage struct {
	Type           string             `json:"type"`
	TaskID         string             `json:"task_id"`
	SegmentIndex   int                `json:"segment_index"`
	Reason         string             `json:"reason"`
	Result         Qwen3SegmentResult `json:"result"`
	ConfirmedTexts []string           `json:"confirmed_texts"`
}

type Qwen3SegmentResult struct {
	Text        string `json:"text"`
	SegmentText string `json:"segment_text"`
	Language    string `json:"language"`
}

// Final message
type Qwen3FinalMessage struct {
	Type   string           `json:"type"`
	TaskID string           `json:"task_id"`
	Result Qwen3FinalResult `json:"result"`
}

type Qwen3FinalResult struct {
	Text          string         `json:"text"`
	FullText      string         `json:"full_text"`
	Language      string         `json:"language"`
	TotalChunks   int            `json:"total_chunks"`
	TotalSegments int            `json:"total_segments"`
	Segments      []Qwen3Segment `json:"segments"`
}

type Qwen3Segment struct {
	Index    int    `json:"index"`
	Text     string `json:"text"`
	Language string `json:"language"`
	Reason   string `json:"reason"`
}

// Error message
type Qwen3ErrorMessage struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
	TaskID  string `json:"task_id"`
}

func NewQwen3AsrSTT(
	opt *qwen3AsrOption,
	logger commons.Logger,
	onPacket func(pkt ...internal_type.Packet) error,
	opts utils.Option,
) *qwen3AsrSTT {
	return &qwen3AsrSTT{
		opt:      opt,
		logger:   logger,
		onPacket: onPacket,
	}
}

func (q *qwen3AsrSTT) Name() string {
	return "qwen3-asr"
}

func (q *qwen3AsrSTT) Initialize() error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.isInitialized {
		return nil
	}

	// Connect to Qwen3 ASR WebSocket server
	conn, _, err := websocket.DefaultDialer.Dial(q.opt.GetServerURL(), nil)
	if err != nil {
		q.logger.Errorf("qwen3-asr: failed to connect to WebSocket: %v", err)
		return fmt.Errorf("qwen3-asr: connection failed: %w", err)
	}

	q.conn = conn

	// Start goroutine to handle incoming messages
	go q.handleMessages()

	// Send start message to initialize the stream
	startMsg := Qwen3StartMessage{
		Type: string(Qwen3MsgTypeStart),
		Payload: Qwen3StartPayload{
			Format:          "pcm",
			SampleRate:      16000,
			Language:        q.opt.GetLanguage(),
			Context:         q.opt.GetContext(),
			ChunkSizeSec:    q.opt.GetChunkSize(),
			UnfixedChunkNum: 2,
			UnfixedTokenNum: 5,
		},
	}

	if err := q.conn.WriteJSON(startMsg); err != nil {
		q.logger.Errorf("qwen3-asr: failed to send start message: %v", err)
		q.conn.Close()
		return fmt.Errorf("qwen3-asr: failed to send start: %w", err)
	}

	q.isStreaming = true
	q.isInitialized = true

	q.logger.Debugf("qwen3-asr: connection initialized")
	return nil
}

func (q *qwen3AsrSTT) handleMessages() {
	for {
		_, message, err := q.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				q.logger.Errorf("qwen3-asr: WebSocket error: %v", err)
			}
			break
		}

		// Parse message type
		var msgType struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(message, &msgType); err != nil {
			q.logger.Errorf("qwen3-asr: failed to parse message: %v", err)
			continue
		}

		switch Qwen3MessageType(msgType.Type) {
		case Qwen3MsgTypeStarted:
			q.logger.Debugf("qwen3-asr: streaming started")

		case Qwen3MsgTypeResult:
			var result Qwen3ResultMessage
			if err := json.Unmarshal(message, &result); err != nil {
				q.logger.Errorf("qwen3-asr: failed to parse result: %v", err)
				continue
			}
			// Send interim results
			for _, item := range result.Results {
				if item.IsPartial {
					q.onPacket(internal_type.SpeechToTextPacket{
						Script:   item.Text,
						Language: item.Language,
						Interim:  true,
					})
				}
			}

		case Qwen3MsgTypeSegmentEnd:
			var segEnd Qwen3SegmentEndMessage
			if err := json.Unmarshal(message, &segEnd); err != nil {
				q.logger.Errorf("qwen3-asr: failed to parse segment_end: %v", err)
				continue
			}
			// Send final segment
			q.onPacket(internal_type.SpeechToTextPacket{
				Script:   segEnd.Result.SegmentText,
				Language: segEnd.Result.Language,
				Interim:  false,
			})

		case Qwen3MsgTypeFinal:
			var final Qwen3FinalMessage
			if err := json.Unmarshal(message, &final); err != nil {
				q.logger.Errorf("qwen3-asr: failed to parse final: %v", err)
				continue
			}
			// Send final result
			for _, seg := range final.Result.Segments {
				q.onPacket(internal_type.SpeechToTextPacket{
					Script:   seg.Text,
					Language: seg.Language,
					Interim:  false,
				})
			}

		case Qwen3MsgTypeError:
			var errMsg Qwen3ErrorMessage
			if err := json.Unmarshal(message, &errMsg); err != nil {
				q.logger.Errorf("qwen3-asr: failed to parse error: %v", err)
				continue
			}
			q.logger.Errorf("qwen3-asr: server error: %s - %s", errMsg.Code, errMsg.Message)

		default:
			q.logger.Debugf("qwen3-asr: unknown message type: %s", msgType.Type)
		}
	}
}

// Transform sends audio data to the Qwen3 ASR WebSocket
func (q *qwen3AsrSTT) Transform(ctx context.Context, in internal_type.UserAudioPacket) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if !q.isInitialized || q.conn == nil {
		return fmt.Errorf("qwen3-asr: connection not initialized")
	}

	if !q.isStreaming {
		return nil
	}

	// Send audio bytes directly (WebSocket binary message)
	err := q.conn.WriteMessage(websocket.BinaryMessage, in.Audio)
	if err != nil {
		q.logger.Errorf("qwen3-asr: failed to send audio: %v", err)
		return fmt.Errorf("qwen3-asr: failed to send audio: %w", err)
	}

	return nil
}

// Close closes the WebSocket connection
func (q *qwen3AsrSTT) Close(ctx context.Context) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.conn != nil {
		// Send stop message
		stopMsg := map[string]string{
			"type": string(Qwen3MsgTypeStop),
		}
		q.conn.WriteJSON(stopMsg)

		// Give some time for final results to arrive
		time.Sleep(100 * time.Millisecond)

		// Close the connection
		q.conn.Close()
		q.conn = nil
	}

	q.isStreaming = false
	q.isInitialized = false

	q.logger.Debugf("qwen3-asr: connection closed")
	return nil
}

func NewQwen3AsrSpeechToText(
	ctx context.Context,
	logger commons.Logger,
	vaultCredential *protos.VaultCredential,
	onPacket func(pkt ...internal_type.Packet) error,
	opts utils.Option,
) (internal_type.SpeechToTextTransformer, error) {
	qwen3Opts, err := NewQwen3AsrOption(logger, vaultCredential, opts)
	if err != nil {
		logger.Errorf("qwen3-asr: failed to create option: %+v", err)
		return nil, err
	}

	return NewQwen3AsrSTT(qwen3Opts, logger, onPacket, opts), nil
}
