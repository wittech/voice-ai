// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.
package internal_grpc

import (
	"context"

	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/protos"
	"google.golang.org/grpc"
)

type unidirectionalStreamer struct {
	server grpc.BidiStreamingServer[protos.AssistantTalkInput, protos.AssistantTalkOutput]
}

func NewGrpcStreamer(
	ctx context.Context,
	logger commons.Logger,
	server protos.TalkService_AssistantTalkServer,
) (internal_type.GrpcStreamer, error) {
	return &unidirectionalStreamer{
		server: server,
	}, nil
}

func (uds *unidirectionalStreamer) Context() context.Context {
	return uds.server.Context()
}

func (uds *unidirectionalStreamer) Recv() (*protos.AssistantTalkInput, error) {
	return uds.server.Recv()
}

// Send sends an output value to the stream.
// It returns an error if the send operation fails.
func (uds *unidirectionalStreamer) Send(out *protos.AssistantTalkOutput) error {
	return uds.server.Send(out)
}
