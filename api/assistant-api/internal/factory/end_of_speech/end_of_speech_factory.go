// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.
package internal_end_of_speech_factory

import (
	"errors"

	internal_end_of_speech "github.com/rapidaai/api/assistant-api/internal/end_of_speech"
	internal_silence_based_end_of_speech "github.com/rapidaai/api/assistant-api/internal/end_of_speech/silence_based"
	"github.com/rapidaai/pkg/commons"
	"github.com/rapidaai/pkg/utils"
)

type EndOfSpeechIdentifier string

const (
	SilenceBasedEndOfSpeech EndOfSpeechIdentifier = "silence_based_eos"
	LiveKitEndOfSpeech      EndOfSpeechIdentifier = "livekit_eos"
)

func GetEndOfSpeech(aa EndOfSpeechIdentifier, logger commons.Logger, onCallback internal_end_of_speech.EndOfSpeechCallback, opts utils.Option) (internal_end_of_speech.EndOfSpeech, error) {
	switch aa {
	case SilenceBasedEndOfSpeech:
		return internal_silence_based_end_of_speech.NewSilenceBasedEndOfSpeech(logger, onCallback, opts)
	default:
		return nil, errors.New("illegal end of speeh")
	}
}
