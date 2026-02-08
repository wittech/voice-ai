// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.
package adapter_internal

import (
	"context"
	"fmt"
	"time"

	internal_adapter_telemetry "github.com/rapidaai/api/assistant-api/internal/telemetry"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/types"
	"github.com/rapidaai/pkg/utils"
	"github.com/rapidaai/protos"
)

// tracerSpan is a type alias for the tracer interface used as span.
type tracerSpan = internal_adapter_telemetry.Tracer[utils.RapidaStage]

// =============================================================================
// Talk - Main Entry Point
// =============================================================================

// Talk handles the main conversation loop for different streamer types.
// It processes incoming messages and manages the connection lifecycle.
func (t *genericRequestor) Talk(ctx context.Context, auth types.SimplePrinciple) error {
	t.StartedAt = time.Now()
	var initialized bool
	for {
		select {
		case <-ctx.Done():
			if initialized {
				t.Disconnect()
			}
			return ctx.Err()
		default:
			req, err := t.streamer.Recv()
			if err != nil {
				continue
			}
			switch payload := req.(type) {
			case *protos.ConversationInitialization:
				if err := t.Connect(ctx, auth, payload); err != nil {
					t.logger.Errorf("unexpected error while connect assistant, might be problem in configuration %+v", err)
					return fmt.Errorf("talking.Connect error: %w", err)
				}
				initialized = true
			case *protos.ConversationConfiguration:
				// return in.Configuration, nil
			case *protos.ConversationUserMessage:
				if err := t.Input(payload); err != nil {
					t.logger.Errorf("error while accepting input %v", err)
				}
			case *protos.ConversationMetadata:
				// might be used for future enhancements
			case *protos.ConversationMerics:
				// Handle metrics if needed
			case *protos.ConversationDisconnection:
				// Handle disconnection if needed
			}
		}
	}
}

// // =============================================================================
// // Stream Processing
// // =============================================================================

// // processStream routes stream processing to the appropriate handler based on streamer type.
// func (t *genericRequestor) processStream(ctx context.Context, auth types.SimplePrinciple, identifier string, initialized bool) (bool, bool, error) {
// 	switch strm := t.streamer.(type) {
// 	case internal_type.TelephonyStreamer:
// 		// return t.processTelephonyStream(ctx, auth, identifier, strm, initialized)
// 		return t.processGrpcStream(ctx, auth, identifier, strm, initialized)
// 	case internal_type.GrpcStreamer:
// 		return t.processGrpcStream(ctx, auth, identifier, strm, initialized)
// 	case internal_type.WebRTCStreamer:
// 		return t.processWebRTCStream(ctx, auth, identifier, strm, initialized)
// 	default:
// 		return true, initialized, nil
// 	}
// }

// // processTelephonyStream handles telephony-based stream processing.
// func (t *genericRequestor) processTelephonyStream(ctx context.Context, auth types.SimplePrinciple, identifier string, strm internal_type.TelephonyStreamer, initialized bool) (bool, bool, error) {
// 	// TODO: Implement telephony stream handling
// 	return true, initialized, nil
// }

// // processGrpcStream handles gRPC-based stream processing.
// func (t *genericRequestor) processGrpcStream(ctx context.Context, auth types.SimplePrinciple, identifier string, strm internal_type.GrpcStreamer, initialized bool) (bool, bool, error) {
// 	return t.processStreamPayload(ctx, auth, identifier, initialized, func() (streamPayload, error) {
// 		return t.recvGrpcPayload(strm)
// 	})
// }

// // processWebRTCStream handles WebRTC-based stream processing.
// func (t *genericRequestor) processWebRTCStream(ctx context.Context, auth types.SimplePrinciple, identifier string, strm internal_type.WebRTCStreamer, initialized bool) (bool, bool, error) {
// 	return t.processStreamPayload(ctx, auth, identifier, initialized, func() (streamPayload, error) {
// 		return t.recvWebRTCPayload(strm)
// 	})
// }

// type streamPayload struct {
// 	config  *protos.ConversationConfiguration
// 	message *protos.ConversationUserMessage
// }

// func (t *genericRequestor) processStreamPayload(
// 	ctx context.Context,
// 	auth types.SimplePrinciple,
// 	identifier string,
// 	initialized bool,
// 	recvPayload func() (streamPayload, error),
// ) (bool, bool, error) {
// 	payload, err := recvPayload()
// 	if err != nil {
// 		shouldContinue, err := t.handleStreamError(err, initialized)
// 		return shouldContinue, initialized, err
// 	}

// 	if payload.config != nil {
// 		initialized = false
// if err := t.Connect(ctx, auth, identifier, payload.config); err != nil {
// 	t.logger.Errorf("unexpected error while connect assistant, might be problem in configuration %+v", err)
// 	return true, initialized, fmt.Errorf("talking.Connect error: %w", err)
// }
// initialized = true
// 		return true, initialized, nil
// 	}

// 	if payload.message != nil && initialized {
// if err := t.Input(payload.message); err != nil {
// 	t.logger.Errorf("error while accepting input %v", err)
// }
// 	}
// 	return true, initialized, nil
// }

// func (t *genericRequestor) recvGrpcPayload(strm internal_type.GrpcStreamer) (streamPayload, error) {
// 	req, err := strm.Recv()
// 	if err != nil {
// 		return streamPayload{}, err
// 	}

// 	switch payload := req.GetRequest().(type) {
// 	case *protos.AssistantTalkInput_Configuration:
// 		return streamPayload{config: payload.Configuration}, nil
// 	case *protos.AssistantTalkInput_Message:
// 		return streamPayload{message: req.GetMessage()}, nil
// 	default:
// 		return streamPayload{}, nil
// 	}
// }

// func (t *genericRequestor) recvWebRTCPayload(strm internal_type.WebRTCStreamer) (streamPayload, error) {

// }

// // =============================================================================
// // Stream Helpers
// // =============================================================================

// // handleStreamError processes stream errors and determines if processing should continue.
// func (t *genericRequestor) handleStreamError(err error, initialized bool) (bool, error) {
// 	if err == io.EOF || status.Code(err) == codes.Canceled {
// 		if initialized {
// 			t.Disconnect()
// 		}
// 		return false, nil
// 	}
// 	return false, fmt.Errorf("stream.Recv error: %w", err)
// }

// // =============================================================================
// // Message Sending
// // =============================================================================

// // sendMessage sends a response to the appropriate streamer based on its type.
// // For WebRTC streamers, it converts AssistantTalkOutput to WebTalkOutput.
// func (t *genericRequestor) sendMessage(ctx context.Context, response *protos.AssistantTalkOutput) error {
// 	switch strm := t.streamer.(type) {
// 	case internal_type.WebRTCStreamer:
// 		webResponse := t.convertToWebTalkOutput(response)
// 		return strm.Send(webResponse)
// 	case internal_type.TelephonyStreamer:
// 		return strm.Send(response)
// 	case internal_type.GrpcStreamer:
// 		return strm.Send(response)
// 	default:
// 		return nil
// 	}
// }

// // convertToWebTalkOutput converts AssistantTalkOutput to WebTalkOutput.
// func (t *genericRequestor) convertToWebTalkOutput(response *protos.AssistantTalkOutput) *protos.WebTalkOutput {
// 	webOutput := &protos.WebTalkOutput{
// 		Code:    response.Code,
// 		Success: response.Success,
// 	}
// 	switch data := response.Data.(type) {
// 	case *protos.AssistantTalkOutput_Configuration:
// 		webOutput.Data = &protos.WebTalkOutput_Configuration{Configuration: data.Configuration}
// 	case *protos.AssistantTalkOutput_Interruption:
// 		webOutput.Data = &protos.WebTalkOutput_Interruption{Interruption: data.Interruption}
// 	case *protos.AssistantTalkOutput_User:
// 		webOutput.Data = &protos.WebTalkOutput_User{User: data.User}
// 	case *protos.AssistantTalkOutput_Assistant:
// 		webOutput.Data = &protos.WebTalkOutput_Assistant{Assistant: data.Assistant}
// 	case *protos.AssistantTalkOutput_Tool:
// 		webOutput.Data = &protos.WebTalkOutput_Tool{Tool: data.Tool}
// 	case *protos.AssistantTalkOutput_ToolResult:
// 		webOutput.Data = &protos.WebTalkOutput_ToolResult{ToolResult: data.ToolResult}
// 	case *protos.AssistantTalkOutput_Directive:
// 		webOutput.Data = &protos.WebTalkOutput_Directive{Directive: data.Directive}
// 	case *protos.AssistantTalkOutput_Error:
// 		webOutput.Data = &protos.WebTalkOutput_Error{Error: data.Error}
// 	}

// 	return webOutput
// }

// // =============================================================================
// // Notification System
// // =============================================================================

// // Notify sends notifications to websocket for various events.
func (t *genericRequestor) Notify(ctx context.Context, actionDatas ...internal_type.Stream) error {
	ctx, span, _ := t.Tracer().StartSpan(ctx, utils.AssistantNotifyStage)
	defer span.EndSpan(ctx, utils.AssistantNotifyStage)
	for _, actionData := range actionDatas {
		t.streamer.Send(actionData)
	}
	return nil
}

// // processNotification routes notification processing based on action type.
// func (t *genericRequestor) processNotification(ctx context.Context, span tracerSpan, actionData interface{}) {
// 	switch data := actionData.(type) {
// 	case *protos.ConversationUserMessage:
// 		t.notifyUserMessage(ctx, span, data)
// 	case *protos.ConversationAssistantMessage:
// 		t.notifyAssistantMessage(ctx, span, data)
// 	case *protos.ConversationInterruption:
// 		t.notifyInterruption(ctx, span, data)
// 	case *protos.ConversationConfiguration:
// 		t.notifyConversationConfig(ctx, span, data)
// 	case *protos.AssistantTalkOutput_Directive:
// 		t.notifyDirective(ctx, span, data)
// 	default:
// 		t.logger.Warnf("unsupported notification action type: %T", actionData)
// 	}
// }

// // =============================================================================
// // Notification Handlers
// // =============================================================================

// // notifyUserMessage handles user message notifications.
// func (t *genericRequestor) notifyUserMessage(ctx context.Context, span tracerSpan, msg *protos.ConversationUserMessage) {
// 	t.sendMessage(ctx, &protos.AssistantTalkOutput{
// 		Code:    200,
// 		Success: true,
// 		Data:    &protos.AssistantTalkOutput_User{User: msg},
// 	})

// 	span.AddAttributes(ctx,
// 		t.attr("actor", "user"),
// 		t.attr("activity", "user_speaking"),
// 		internal_adapter_telemetry.KV{K: "completed", V: internal_adapter_telemetry.BoolValue(msg.GetCompleted())},
// 		t.attr("messageId", msg.Id),
// 	)

// 	t.addUserMessageTypeAttributes(ctx, span, msg)
// }

// // addUserMessageTypeAttributes adds type-specific attributes for user messages.
// func (t *genericRequestor) addUserMessageTypeAttributes(ctx context.Context, span tracerSpan, msg *protos.ConversationUserMessage) {
// 	switch content := msg.Message.(type) {
// 	case *protos.ConversationUserMessage_Text:
// 		span.AddAttributes(ctx,
// 			t.attr("notification_type", "text"),
// 			internal_adapter_telemetry.KV{K: "content_length", V: internal_adapter_telemetry.IntValue(len(content.Text))},
// 			t.attr("content", content.Text),
// 		)
// 	}
// }

// // notifyAssistantMessage handles assistant message notifications.
// func (t *genericRequestor) notifyAssistantMessage(ctx context.Context, span tracerSpan, msg *protos.ConversationAssistantMessage) {
// 	t.sendMessage(ctx, &protos.AssistantTalkOutput{
// 		Code:    200,
// 		Success: true,
// 		Data:    &protos.AssistantTalkOutput_Assistant{Assistant: msg},
// 	})

// 	span.AddAttributes(ctx,
// 		t.attr("actor", "assistant"),
// 		t.attr("activity", "assistant_speaking"),
// 		internal_adapter_telemetry.KV{K: "completed", V: internal_adapter_telemetry.BoolValue(msg.GetCompleted())},
// 		t.attr("messageId", msg.Id),
// 	)

// 	t.addAssistantMessageTypeAttributes(ctx, span, msg)
// }

// // addAssistantMessageTypeAttributes adds type-specific attributes for assistant messages.
// func (t *genericRequestor) addAssistantMessageTypeAttributes(ctx context.Context, span tracerSpan, msg *protos.ConversationAssistantMessage) {
// 	switch content := msg.Message.(type) {
// 	case *protos.ConversationAssistantMessage_Audio:
// 		span.AddAttributes(ctx,
// 			t.attr("notification_type", "audio"),
// 			internal_adapter_telemetry.KV{K: "content_length", V: internal_adapter_telemetry.IntValue(len(content.Audio))},
// 		)
// 	case *protos.ConversationAssistantMessage_Text:
// 		span.AddAttributes(ctx,
// 			t.attr("notification_type", "text"),
// 			internal_adapter_telemetry.KV{K: "content_length", V: internal_adapter_telemetry.IntValue(len(content.Text))},
// 			t.attr("content", content.Text),
// 		)
// 	}
// }

// // notifyInterruption handles interruption notifications.
// func (t *genericRequestor) notifyInterruption(ctx context.Context, span tracerSpan, msg *protos.ConversationInterruption) {
// 	t.sendMessage(ctx, &protos.AssistantTalkOutput{
// 		Code:    200,
// 		Success: true,
// 		Data:    &protos.AssistantTalkOutput_Interruption{Interruption: msg},
// 	})

// 	span.AddAttributes(ctx,
// 		t.attr("actor", "system"),
// 		t.attr("activity", "interrupting"),
// 		t.attr("messageId", msg.Id),
// 	)
// }

// // notifyConversationConfig handles configuration notifications for conversations.
// func (t *genericRequestor) notifyConversationConfig(ctx context.Context, span tracerSpan, config *protos.ConversationConfiguration) {
// 	utils.Go(ctx, func() {
// 		t.sendMessage(ctx, &protos.AssistantTalkOutput{
// 			Code:    200,
// 			Success: true,
// 			Data:    &protos.AssistantTalkOutput_Configuration{Configuration: config},
// 		})
// 	})

// 	span.AddAttributes(ctx,
// 		t.attr("actor", "system"),
// 		t.attr("activity", "assistant_configuration"),
// 	)
// }

// // notifyDirective handles directive notifications.
// func (t *genericRequestor) notifyDirective(ctx context.Context, span tracerSpan, directive *protos.AssistantTalkOutput_Directive) {
// 	t.sendMessage(ctx, &protos.AssistantTalkOutput{
// 		Code:    200,
// 		Success: true,
// 		Data:    directive,
// 	})

// 	span.AddAttributes(ctx,
// 		t.attr("actor", "action"),
// 		t.attr("activity", "assistant_disconnect"),
// 	)
// }

// // =============================================================================
// // Telemetry Helpers
// // =============================================================================

// // attr creates a string attribute for telemetry.
// func (t *genericRequestor) attr(key, value string) internal_adapter_telemetry.KV {
// 	return internal_adapter_telemetry.KV{K: key, V: internal_adapter_telemetry.StringValue(value)}
// }
