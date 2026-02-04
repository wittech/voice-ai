// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.
package internal_type

import (
	"context"

	"github.com/rapidaai/protos"
)

type Streamer interface {
	Context() context.Context
}

type GrpcStreamer interface {
	Streamer
	// Recv receives the next input value from the stream.
	// It returns the received value and any error encountered.
	// If the stream is closed, it should return (zero value, io.EOF).
	Recv() (*protos.AssistantTalkInput, error)

	// Send sends an output value to the stream.
	// It returns an error if the send operation fails.
	Send(*protos.AssistantTalkOutput) error
}

type WebRTCStreamer interface {
	Streamer
	// Recv receives the next input value from the stream.
	// It returns the received value and any error encountered.
	// If the stream is closed, it should return (zero value, io.EOF).
	Recv() (*protos.WebTalkInput, error)

	// Send sends an output value to the stream.
	// It returns an error if the send operation fails.
	Send(*protos.WebTalkOutput) error
}

type TelephonyStreamer interface {
	Streamer

	// Recv receives the next input value from the stream.
	// It returns the received value and any error encountered.
	// If the stream is closed, it should return (zero value, io.EOF).
	Recv() (*protos.AssistantTalkInput, error)

	// Send sends an output value to the stream.
	// It returns an error if the send operation fails.
	Send(*protos.AssistantTalkOutput) error
}
