// Copyright (c) 2023-2025 RapidaAI
// Author: Prashant Srivastav <prashant@rapida.ai>
//
// Licensed under GPL-2.0 with Rapida Additional Terms.
// See LICENSE.md or contact sales@rapida.ai for commercial usage.
package internal_sentence_aggregator

import (
	"context"

	internal_default_aggregator "github.com/rapidaai/api/assistant-api/internal/aggregator/text/internal/default"
	internal_type "github.com/rapidaai/api/assistant-api/internal/type"
	"github.com/rapidaai/pkg/commons"
)

func GetLLMTextAggregator(
	ctx context.Context,
	logger commons.Logger,
) (internal_type.LLMTextAggregator, error) {
	return internal_default_aggregator.NewDefaultLLMTextAggregator(ctx, logger)
}
