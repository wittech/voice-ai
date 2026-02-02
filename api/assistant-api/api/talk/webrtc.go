// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package assistant_talk_api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"golang.org/x/sync/errgroup"

	internal_adapter "github.com/rapidaai/api/assistant-api/internal/adapters"
	internal_assistant_entity "github.com/rapidaai/api/assistant-api/internal/entity/assistants"
	internal_conversation_entity "github.com/rapidaai/api/assistant-api/internal/entity/conversations"
	internal_services "github.com/rapidaai/api/assistant-api/internal/services"
	internal_webrtc "github.com/rapidaai/api/assistant-api/internal/webrtc"
	"github.com/rapidaai/pkg/types"
	type_enums "github.com/rapidaai/pkg/types/enums"
	"github.com/rapidaai/pkg/utils"
)

var webrtcUpgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// WebRTCConnect handles WebRTC connections for voice conversations
// The WebSocket is used ONLY for signaling (SDP/ICE exchange)
// Audio flows through native WebRTC media tracks (SRTP), NOT WebSocket
//
// @Router /v1/talk/webrtc/:assistantId [get]
// @Summary Connect to assistant via WebRTC
// @Description Establishes a WebRTC connection for real-time voice conversation
// @Param assistantId path uint64 true "Assistant ID"
// @Produce json
// @Success 101 "Switching Protocols"
// @Failure 400 {object} gin.H
// @Failure 401 {object} gin.H
func (cApi *ConversationApi) WebRTCConnect(c *gin.Context) {
	// Upgrade to WebSocket for signaling only
	signalingConn, err := webrtcUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		cApi.logger.Errorf("WebSocket upgrade failed: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unable to upgrade to WebSocket"})
		return
	}

	// Helper to send error over WebSocket and close
	sendErrorAndClose := func(errMsg string) {
		errData, _ := json.Marshal(map[string]interface{}{
			"type":  "error",
			"error": errMsg,
		})
		signalingConn.WriteMessage(websocket.TextMessage, errData)
		signalingConn.Close()
	}

	auth, isAuthenticated := types.GetAuthPrinciple(c)
	if !isAuthenticated {
		cApi.logger.Error("WebRTC: Unauthenticated request")
		sendErrorAndClose("Unauthenticated request - missing or invalid authorization")
		return
	}

	cApi.logger.Infof("WebRTC: Authenticated user %d", auth.GetUserId())

	assistantIdStr := c.Param("assistantId")
	assistantId, err := strconv.ParseUint(assistantIdStr, 10, 64)
	if err != nil {
		cApi.logger.Errorf("WebRTC: Invalid assistantId: %v", err)
		sendErrorAndClose("Invalid assistantId")
		return
	}

	cApi.logger.Infof("WebRTC: Loading assistant %d", assistantId)

	// Load assistant first to get provider model ID
	assistant, err := cApi.assistantService.Get(c, auth, assistantId, nil, internal_services.NewDefaultGetAssistantOption())
	if err != nil {
		cApi.logger.Errorf("WebRTC: Failed to load assistant: %v", err)
		sendErrorAndClose(fmt.Sprintf("Failed to load assistant: %v", err))
		return
	}

	cApi.logger.Infof("WebRTC: Assistant loaded, creating conversation")

	// Create identifier for the conversation
	identifier := internal_adapter.Identifier(utils.WebRTC, c, auth, "")

	// Create new conversation for WebRTC session
	conversation, err := cApi.assistantConversationService.CreateConversation(
		c, auth, identifier, assistantId, assistant.AssistantProviderId,
		type_enums.DIRECTION_INBOUND, utils.WebRTC,
	)
	if err != nil {
		cApi.logger.Errorf("WebRTC: Failed to create conversation: %v", err)
		sendErrorAndClose(fmt.Sprintf("Failed to create conversation: %v", err))
		return
	}

	cApi.logger.Infof("WebRTC: Conversation created %d, setting up streamer", conversation.Id)

	// Create WebRTC streamer
	// Audio flows through WebRTC peer connection, WebSocket only for signaling
	streamer, err := internal_webrtc.NewStreamer(c.Request.Context(), &internal_webrtc.StreamerConfig{
		Config:        internal_webrtc.DefaultConfig(),
		Logger:        cApi.logger,
		SignalingConn: signalingConn,
		Assistant:     assistant,
		Conversation:  conversation,
	})
	if err != nil {
		cApi.assistantConversationService.ApplyConversationMetrics(c, auth, assistantId, conversation.Id,
			[]*types.Metric{{Name: type_enums.STATUS.String(), Value: type_enums.RECORD_FAILED.String(), Description: "WebRTC setup failed"}})
		cApi.logger.Errorf("WebRTC: Failed to create streamer: %v", err)
		sendErrorAndClose(fmt.Sprintf("Failed to create WebRTC connection: %v", err))
		return
	}

	cApi.logger.Infof("WebRTC: Streamer created, setting up talker")

	// Create talker with WebRTC source
	talker, err := internal_adapter.GetTalker(
		utils.WebRTC,
		c,
		cApi.cfg,
		cApi.logger,
		cApi.postgres,
		cApi.opensearch,
		cApi.redis,
		cApi.storage,
		streamer,
	)
	if err != nil {
		if closeable, ok := streamer.(io.Closer); ok {
			closeable.Close()
		}
		cApi.assistantConversationService.ApplyConversationMetrics(c, auth, assistantId, conversation.Id,
			[]*types.Metric{{Name: type_enums.STATUS.String(), Value: type_enums.RECORD_FAILED.String(), Description: "Talker creation failed"}})
		cApi.logger.Errorf("WebRTC: Failed to create talker: %v", err)
		sendErrorAndClose(fmt.Sprintf("Failed to setup conversation: %v", err))
		return
	}

	cApi.logger.Infof("WebRTC: Talker created, starting conversation")

	cApi.logger.Infof("WebRTC session started: assistant=%d, conversation=%d, identifier=%s", assistantId, conversation.Id, identifier)

	// Start the conversation - this blocks until conversation ends
	// Use the same identifier that was used to create the conversation
	if err := talker.Talk(c, auth, identifier); err != nil {
		cApi.logger.Errorf("WebRTC conversation error: %v", err)
	}

	cApi.logger.Infof("WebRTC session ended: assistant=%d, conversation=%d", assistantId, conversation.Id)
}

// WebRTCConnectWithConversation handles WebRTC connections with an existing conversation
//
// @Router /v1/talk/webrtc/:assistantId/:conversationId [get]
// @Summary Connect to assistant via WebRTC with existing conversation
func (cApi *ConversationApi) WebRTCConnectWithConversation(c *gin.Context) {
	signalingConn, err := webrtcUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		cApi.logger.Errorf("WebSocket upgrade failed: %v", err)
		c.JSON(http.StatusBadRequest, gin.H{"error": "Unable to upgrade to WebSocket"})
		return
	}

	// Helper to send error over WebSocket and close
	sendErrorAndClose := func(errMsg string) {
		errData, _ := json.Marshal(map[string]interface{}{
			"type":  "error",
			"error": errMsg,
		})
		signalingConn.WriteMessage(websocket.TextMessage, errData)
		signalingConn.Close()
	}

	auth, isAuthenticated := types.GetAuthPrinciple(c)
	if !isAuthenticated {
		cApi.logger.Error("WebRTC: Unauthenticated request")
		sendErrorAndClose("Unauthenticated request - missing or invalid authorization")
		return
	}

	assistantId, err := strconv.ParseUint(c.Param("assistantId"), 10, 64)
	if err != nil {
		sendErrorAndClose("Invalid assistantId")
		return
	}

	conversationId, err := strconv.ParseUint(c.Param("conversationId"), 10, 64)
	if err != nil {
		sendErrorAndClose("Invalid conversationId")
		return
	}

	var (
		wg           errgroup.Group
		assistant    *internal_assistant_entity.Assistant
		conversation *internal_conversation_entity.AssistantConversation
	)

	wg.Go(func() error {
		var aErr error
		assistant, aErr = cApi.assistantService.Get(c, auth, assistantId, nil, internal_services.NewDefaultGetAssistantOption())
		return aErr
	})

	wg.Go(func() error {
		var cErr error
		conversation, cErr = cApi.assistantConversationService.Get(c, auth, assistantId, conversationId, internal_services.NewDefaultGetConversationOption())
		return cErr
	})

	if err := wg.Wait(); err != nil {
		cApi.logger.Errorf("WebRTC: Failed to setup session: %v", err)
		sendErrorAndClose(fmt.Sprintf("Failed to load conversation: %v", err))
		return
	}

	streamer, err := internal_webrtc.NewStreamer(c.Request.Context(), &internal_webrtc.StreamerConfig{
		Config:        internal_webrtc.DefaultConfig(),
		Logger:        cApi.logger,
		SignalingConn: signalingConn,
		Assistant:     assistant,
		Conversation:  conversation,
	})
	if err != nil {
		cApi.logger.Errorf("WebRTC: Failed to create streamer: %v", err)
		sendErrorAndClose(fmt.Sprintf("Failed to create WebRTC connection: %v", err))
		return
	}

	talker, err := internal_adapter.GetTalker(
		utils.WebRTC,
		c,
		cApi.cfg,
		cApi.logger,
		cApi.postgres,
		cApi.opensearch,
		cApi.redis,
		cApi.storage,
		streamer,
	)
	if err != nil {
		if closeable, ok := streamer.(io.Closer); ok {
			closeable.Close()
		}
		cApi.logger.Errorf("WebRTC: Failed to create talker: %v", err)
		sendErrorAndClose(fmt.Sprintf("Failed to setup conversation: %v", err))
		return
	}

	cApi.logger.Infof("WebRTC session started: assistant=%d, conversation=%d, identifier=%s", assistantId, conversationId, conversation.Identifier)

	// Use the conversation's stored identifier to ensure consistent lookup
	if err := talker.Talk(c, auth, conversation.Identifier); err != nil {
		cApi.logger.Errorf("WebRTC conversation error: %v", err)
	}

	cApi.logger.Infof("WebRTC session ended: assistant=%d, conversation=%d", assistantId, conversationId)
}
