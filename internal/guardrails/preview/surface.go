package preview

import (
	"slices"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/routing"
)

func compatibility(route routing.Route, in selection, window int) (guardrails.Compatibility, error) {
	if !slices.Contains(route.Operations, in.operation) {
		return guardrails.Compatibility{}, httpkit.Invalid("operation", "Choose an operation on the serving route.")
	}
	if in.retainsOutput && (in.operation != wire.OperationGeneration || in.surface != wire.Dialect(wire.Responses)) {
		return guardrails.Compatibility{}, httpkit.Invalid("retains_output", "Retained output is only available for OpenAI Responses.")
	}
	if family, ok := generationFamily(in.surface); in.operation == wire.OperationGeneration && ok {
		if family == wire.GeminiInteractions || (family == wire.Bedrock && !route.UsesPreparedBedrockConverse()) {
			return guardrails.Compatibility{Uninspected: true}, nil
		}
		uninspected := !interaction.HasGuardrailInputCoverage(family)
		if in.mode == "streaming" {
			return guardrails.Compatibility{
				StreamWindow: window, RetainsOutput: in.retainsOutput,
				Uninspected: uninspected, TypeSafeInput: route.ToolExecution == "gateway",
				StreamTextRewriteUnavailable: !family.CanRewriteGuardrailStreamText(),
			}, nil
		}
		return guardrails.Compatibility{
			RetainsOutput: in.retainsOutput, Uninspected: uninspected, TypeSafeInput: route.ToolExecution == "gateway",
		}, nil
	}
	if dialect, ok := operationregistry.Lookup(in.surface); ok && dialect.Operation.ID == in.operation && in.mode == "unary" {
		if route.Fidelity.Strict() || operationregistry.PolicyFor(in.operation).TransformedUnary {
			return guardrails.Compatibility{TypedUnary: true}, nil
		}
		if transformedSurface(in.surface) {
			return guardrails.Compatibility{}, nil
		}
	}
	if uninspectedSurface(in) {
		return guardrails.Compatibility{Uninspected: true}, nil
	}
	return guardrails.Compatibility{}, httpkit.Invalid("surface", "Choose a supported surface for this operation, fidelity and mode.")
}

func generationFamily(surface string) (wire.Format, bool) {
	if family, native := protocols.NativeGenerationFamily(surface); native {
		return family, true
	}
	for _, family := range [...]wire.Format{wire.Chat, wire.Completions, wire.Responses, wire.Anthropic, wire.Gemini, wire.GeminiInteractions, wire.Bedrock} {
		if wire.Dialect(family) == surface {
			return family, true
		}
	}
	return "", false
}

func transformedSurface(surface string) bool {
	switch surface {
	case wire.Dialect(wire.Embeddings), wire.Dialect(wire.Moderation), wire.Dialect(wire.Rerank),
		wire.Dialect(wire.InputTokens), wire.Dialect(wire.AnthropicCount), wire.Dialect(wire.GeminiCount):
		return true
	}
	return false
}

func uninspectedSurface(in selection) bool {
	if in.operation == "bedrock_invoke" && in.surface == "bedrock-invoke" {
		return true
	}
	if in.surface == "openai" && slices.Contains(vendors.MediaOperations(), in.operation) {
		return in.mode == "unary" || media.Operation(in.operation).SupportsStreaming()
	}
	if in.mode != "unary" {
		return false
	}
	return in.operation == "realtime" && slices.Contains([]string{"openai-realtime", "gemini-live", "native-duplex"}, in.surface) ||
		in.operation == "batch" && slices.Contains([]string{"openai-batch", "anthropic-batch", "gemini-batch", "cloud-batch"}, in.surface) ||
		in.operation == "passthrough" && in.surface == "native"
}
