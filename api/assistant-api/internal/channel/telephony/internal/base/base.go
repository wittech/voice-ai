// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.
package internal_telephony_base

import (
	"bytes"
	"context"
	"encoding/base64"
	"sync"

	internal_audio "github.com/rapidaai/api/assistant-api/internal/audio"
	internal_audio_resampler "github.com/rapidaai/api/assistant-api/internal/audio/resampler"
	internal_assistant_entity "github.com/rapidaai/api/assistant-api/internal/entity/assistants"
	internal_conversation_entity "github.com/rapidaai/api/assistant-api/internal/entity/conversations"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/utils"
	"github.com/rapidaai/protos"
)

// internal rapida audio config
var RAPIDA_AUDIO_CONFIG = internal_audio.NewLinear16khzMonoAudioConfig()

//

type BaseTelephonyStreamer struct {
	logger commons.Logger

	// conn       *websocket.Conn
	ctx        context.Context
	cancelFunc context.CancelFunc

	assistant             *internal_assistant_entity.Assistant
	assistantConversation *internal_conversation_entity.AssistantConversation
	version               string

	//
	inputAudioBuffer      *bytes.Buffer
	inputAudioBufferLock  sync.Mutex
	outputAudioBuffer     *bytes.Buffer
	outputAudioBufferLock sync.Mutex

	resampler internal_type.AudioResampler
	//
	encoder         *base64.Encoding
	vaultCredential *protos.VaultCredential
}

func NewBaseTelephonyStreamer(logger commons.Logger, assistant *internal_assistant_entity.Assistant, assistantConversation *internal_conversation_entity.AssistantConversation, vlt *protos.VaultCredential) BaseTelephonyStreamer {
	ctx, cancel := context.WithCancel(context.Background())
	resampler, _ := internal_audio_resampler.GetResampler(logger)
	return BaseTelephonyStreamer{
		logger:                logger,
		ctx:                   ctx,
		cancelFunc:            cancel,
		assistant:             assistant,
		resampler:             resampler,
		assistantConversation: assistantConversation,
		inputAudioBuffer:      new(bytes.Buffer),
		outputAudioBuffer:     new(bytes.Buffer),
		encoder:               base64.StdEncoding,
		vaultCredential:       vlt,
	}
}

func (base *BaseTelephonyStreamer) CreateVoiceRequest(audioData []byte) *protos.ConversationUserMessage {
	return &protos.ConversationUserMessage{
		Message: &protos.ConversationUserMessage_Audio{
			Audio: audioData,
		},
	}
}

func (base *BaseTelephonyStreamer) GetAssistantDefinition() *protos.AssistantDefinition {
	return &protos.AssistantDefinition{
		AssistantId: base.assistant.Id,
		Version:     utils.GetVersionString(base.assistant.AssistantProviderId),
	}
}
func (base *BaseTelephonyStreamer) GetConversationId() uint64 {
	return base.assistantConversation.Id
}

func (base *BaseTelephonyStreamer) Context() context.Context {
	return base.ctx
}

// func (base *BaseTelephonyStreamer) Connection() *websocket.Conn {
// 	return base.conn
// }

// func (base *BaseTelephonyStreamer) Cancel() error {
// 	if base.conn != nil {
// 		base.conn.Close()
// 		base.conn = nil
// 	}
// 	base.cancelFunc()
// 	return nil
// }

// LockInputAudioBuffer locks the input audio buffer and returns it.
// Caller MUST call UnlockInputAudioBuffer().
func (base *BaseTelephonyStreamer) LockInputAudioBuffer() {
	base.inputAudioBufferLock.Lock()
}

// UnlockInputAudioBuffer unlocks the input audio buffer.
func (base *BaseTelephonyStreamer) UnlockInputAudioBuffer() {
	base.inputAudioBufferLock.Unlock()
}

// LockOutputAudioBuffer locks the output audio buffer and returns it.
// Caller MUST call UnlockOutputAudioBuffer().
func (base *BaseTelephonyStreamer) LockOutputAudioBuffer() {
	base.outputAudioBufferLock.Lock()
}

// UnlockOutputAudioBuffer unlocks the output audio buffer.
func (base *BaseTelephonyStreamer) UnlockOutputAudioBuffer() {
	base.outputAudioBufferLock.Unlock()
}

// Encoder returns the base64 encoder used by the streamer.
func (base *BaseTelephonyStreamer) Encoder() *base64.Encoding {
	return base.encoder
}

// Credential returns the vault credential associated with the streamer.
func (base *BaseTelephonyStreamer) Credential() *protos.VaultCredential {
	return base.vaultCredential
}

func (base *BaseTelephonyStreamer) InputBuffer() *bytes.Buffer {
	return base.inputAudioBuffer
}

func (base *BaseTelephonyStreamer) OutputBuffer() *bytes.Buffer {
	return base.outputAudioBuffer
}

func (base *BaseTelephonyStreamer) VaultCredential() *protos.VaultCredential {
	return base.vaultCredential
}

func (base *BaseTelephonyStreamer) CreateConnectionRequest() *protos.ConversationInitialization {
	return &protos.ConversationInitialization{
		AssistantConversationId: base.GetConversationId(),
		Assistant:               base.GetAssistantDefinition(),
		StreamMode:              protos.StreamMode_STREAM_MODE_AUDIO,
	}
}

func (base *BaseTelephonyStreamer) GetAssistatntConversation() *internal_conversation_entity.AssistantConversation {
	return base.assistantConversation
}
