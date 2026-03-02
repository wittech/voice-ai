// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.

package qwen3asr

import (
	"fmt"
	"net/url"

	commons "github.com/rapidaai/pkg/commons"
	utils "github.com/rapidaai/pkg/utils"
	"github.com/rapidaai/protos"
)

type qwen3AsrOption struct {
	apiKey    string
	logger    commons.Logger
	opts      utils.Option
	serverURL string
	language  string
	model     string
	context   string
	chunkSize float64
}

func NewQwen3AsrOption(
	logger commons.Logger,
	vaultCredential *protos.VaultCredential,
	opts utils.Option,
) (*qwen3AsrOption, error) {
	credentials := vaultCredential.GetValue().AsMap()

	// Get API key from credential
	apiKey := ""
	if v, ok := credentials["key"]; ok {
		apiKey = v.(string)
	}

	// Get server URL (default to localhost)
	serverURL := "ws://localhost:8080"
	if v, err := opts.GetString("listen.server_url"); err == nil {
		serverURL = v
	}

	// Get language (default to auto-detect)
	language := "auto"
	if v, err := opts.GetString("listen.language"); err == nil {
		language = v
	}

	// Get model
	model := "qwen3-asr-1.7b"
	if v, err := opts.GetString("listen.model"); err == nil {
		model = v
	}

	// Get context
	context := ""
	if v, err := opts.GetString("listen.context"); err == nil {
		context = v
	}

	// Get chunk size (in seconds)
	chunkSize := 2.0
	if v, err := opts.GetFloat64("listen.chunk_size_sec"); err == nil {
		chunkSize = v
	}

	return &qwen3AsrOption{
		apiKey:    apiKey,
		logger:    logger,
		opts:      opts,
		serverURL: serverURL,
		language:  language,
		model:     model,
		context:   context,
		chunkSize: chunkSize,
	}, nil
}

func (q *qwen3AsrOption) GetAPIKey() string {
	return q.apiKey
}

func (q *qwen3AsrOption) GetServerURL() string {
	// Construct WebSocket URL
	params := url.Values{}
	params.Add("model", q.model)
	return fmt.Sprintf("%s/ws/v1/asr/qwen?%s", q.serverURL, params.Encode())
}

func (q *qwen3AsrOption) GetLanguage() string {
	return q.language
}

func (q *qwen3AsrOption) GetModel() string {
	return q.model
}

func (q *qwen3AsrOption) GetContext() string {
	return q.context
}

func (q *qwen3AsrOption) GetChunkSize() float64 {
	return q.chunkSize
}
