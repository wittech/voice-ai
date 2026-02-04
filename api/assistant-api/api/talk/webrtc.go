// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package assistant_talk_api

import (
	"errors"

	internal_adapter "github.com/rapidaai/api/assistant-api/internal/adapters"
	internal_webrtc "github.com/rapidaai/api/assistant-api/internal/webrtc"
	"github.com/rapidaai/pkg/types"
	"github.com/rapidaai/pkg/utils"
	assistant_api "github.com/rapidaai/protos"
)

func (cApi *ConversationGrpcApi) WebTalk(stream assistant_api.WebRTC_WebTalkServer) error {
	auth, isAuthenticated := types.GetSimplePrincipleGRPC(stream.Context())
	if !isAuthenticated {
		cApi.logger.Errorf("unable to resolve the authentication object, please check the parameter for authentication")
		return errors.New("unauthenticated request for messaging")
	}

	source, ok := utils.GetClientSource(stream.Context())
	if !ok {
		cApi.logger.Errorf("unable to resolve the source from the context")
		return errors.New("illegal source")
	}
	streamer, err := internal_webrtc.NewWebRTCStreamer(stream.Context(), cApi.logger, stream)
	if err != nil {
		cApi.logger.Errorf("failed to create grpc streamer: %v", err)
		return err
	}
	talker, err := internal_adapter.GetTalker(
		source,
		stream.Context(),
		cApi.cfg,
		cApi.logger,
		cApi.postgres,
		cApi.opensearch,
		cApi.redis,
		cApi.storage,
		streamer,
	)
	if err != nil {
		cApi.logger.Errorf("failed to setup talker: %v", err)
		return err
	}

	return talker.Talk(stream.Context(), auth, internal_adapter.Identifier(source, stream.Context(), auth, ""))
}
