package routes

import (
	"math"
	"slices"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
)

type contextWindowPreview struct {
	InputBreakdown         *estimate.Breakdown `json:"input_breakdown"`
	RetainedContextUnknown bool                `json:"retained_context_unknown"`
	RemainingTokens        *int64              `json:"remaining_tokens"`
}

func (d *simulatedDemand) contextWindow(decision routing.Decision) *contextWindowPreview {
	if !d.sampleGeneration {
		return nil
	}
	measured := d.estimates[decision.TargetID]
	unknown := d.retainedContextUnknown || measured.retainedContextUnknown
	return &contextWindowPreview{
		InputBreakdown:         measured.Breakdown,
		RetainedContextUnknown: unknown,
		RemainingTokens:        remainingContext(decision, unknown),
	}
}

func remainingContext(decision routing.Decision, retainedUnknown bool) *int64 {
	if decision.ContextLength == nil || decision.EstimatedInputTokens == nil || retainedUnknown {
		return nil
	}
	remaining := *decision.ContextLength - *decision.EstimatedInputTokens
	if decision.RequestedOutputTokens == nil {
		return &remaining
	}
	output := *decision.RequestedOutputTokens
	return new(max(remaining, math.MinInt64+output) - output)
}

func hasUnmeasuredContext(request *wire.Request) bool {
	document := request.OIF().Document()
	switch request.Family {
	case wire.Responses:
		if presentValue(document, "/previous_response_id") || presentValue(document, "/conversation") {
			return true
		}
		input, _ := document.Lookup("/input")
		return slices.ContainsFunc(input.Elements(), func(item oif.Value) bool {
			kind, _ := item.Lookup("type")
			name, _ := kind.Chars()
			_, hasID := item.Lookup("id")
			encrypted, hasEncrypted := item.Lookup("encrypted_content")
			return wire.IsResponsesItemReference(name, hasID) ||
				(name == "reasoning" && hasEncrypted && encrypted.Kind() != oif.Null)
		})
	case wire.Anthropic:
		messages, _ := document.Lookup("/messages")
		return slices.ContainsFunc(messages.Elements(), func(message oif.Value) bool {
			content, _ := message.Lookup("content")
			return slices.ContainsFunc(content.Elements(), func(block oif.Value) bool {
				kind, _ := block.Lookup("type")
				name, _ := kind.Chars()
				return name == "redacted_thinking"
			})
		})
	case wire.Gemini, wire.GeminiStream:
		return presentValue(document, "/cachedContent")
	default:
		return false
	}
}

func presentValue(document oif.Document, path string) bool {
	value, ok := document.Lookup(path)
	return ok && value.Kind() != oif.Null
}
