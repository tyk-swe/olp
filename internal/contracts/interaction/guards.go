package interaction

import (
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"

	"github.com/tyk-swe/olp/internal/domain/oif"
)

func guardFailure(field, requirement string) error {
	return incompatible("fidelity_protocol_violation", field, requirement, "The provider result does not satisfy the admitted interaction contract.")
}

// ValidateResult consumes a result already checked by its native dialect codec.
// The caller can retain observed usage even when a later projection or policy
// guard refuses delivery. No second native decode runs on the gateway hot path.
func (p *Plan) ValidateResult(result oif.Result) error {
	document := result.Source()
	if !document.Valid() || document.Len() > p.receipt.Obligations.MaxBodyBytes || result.Descriptor().Dialect != wire.Descriptor(p.Wire(), false).Dialect {
		return guardFailure("/result", "native_result_grammar")
	}
	if p.receipt.Class == QualifiedInteraction && !p.ToolContinuation() {
		if err := validateAnthropicTextResult(document); err != nil {
			return err
		}
	}
	return nil
}
func validateAnthropicTextResult(document oif.Document) error {
	root := document.Root()
	if !onlyMembers(root, "id type role model content stop_reason stop_sequence usage") {
		return guardFailure("/result", "unmapped_result_member")
	}
	if valueText(member(root, "type")) != "message" || valueText(member(root, "role")) != "assistant" || valueText(member(root, "id")) == "" || valueText(member(root, "model")) == "" {
		return guardFailure("/result", "message_identity")
	}
	content := member(root, "content")
	if content.Kind() != oif.Array || len(content.Elements()) != 1 {
		return guardFailure("/content", "single_text_boundary")
	}
	part := content.Elements()[0]
	if !onlyMembers(part, "type text") || valueText(member(part, "type")) != "text" || member(part, "text").Kind() != oif.String {
		return guardFailure("/content/0", "unmapped_content_or_state")
	}
	if !slices.Contains([]string{"end_turn", "max_tokens"}, valueText(member(root, "stop_reason"))) {
		return guardFailure("/stop_reason", "terminal_outcome")
	}
	stop := member(root, "stop_sequence")
	if stop.Kind() != oif.Null && stop.Kind() != oif.Absent {
		return guardFailure("/stop_sequence", "unmapped_stop_sequence")
	}
	usage := member(root, "usage")
	if !onlyMembers(usage, "input_tokens output_tokens cache_read_input_tokens cache_creation_input_tokens cache_creation service_tier") {
		return guardFailure("/usage", "unmapped_usage")
	}
	for _, name := range []string{"input_tokens", "output_tokens"} {
		if !nonnegativeInteger(member(usage, name)) {
			return guardFailure("/usage/"+name, "token_usage")
		}
	}
	for _, name := range []string{"cache_read_input_tokens", "cache_creation_input_tokens"} {
		if count, present := usage.Lookup(name); present && !nonnegativeInteger(count) {
			return guardFailure("/usage/"+name, "token_usage")
		}
	}
	if creation, present := usage.Lookup("cache_creation"); present {
		if !onlyMembers(creation, "ephemeral_5m_input_tokens ephemeral_1h_input_tokens") {
			return guardFailure("/usage/cache_creation", "unmapped_usage")
		}
		for _, detail := range creation.Members() {
			if !nonnegativeInteger(detail.Value) {
				return guardFailure("/usage/cache_creation", "token_usage")
			}
		}
	}
	if tier, present := usage.Lookup("service_tier"); present && tier.Kind() != oif.String && tier.Kind() != oif.Null {
		return guardFailure("/usage/service_tier", "token_usage")
	}
	return nil
}
func onlyMembers(value oif.Value, allowed string) bool {
	if value.Kind() != oif.Object {
		return false
	}
	names := strings.Fields(allowed)
	for _, field := range value.Members() {
		if !slices.Contains(names, field.Name) {
			return false
		}
	}
	return true
}
func nonnegativeInteger(value oif.Value) bool {
	if value.Kind() != oif.Number {
		return false
	}
	n, err := strconv.ParseInt(value.Raw(), 10, 64)
	return err == nil && n >= 0
}

// ValidateEvent validates individual native event envelopes synchronously before
// any projection. Ordering, terminal state, bounded accumulation and completion
// remain enforced by StreamWithEvents' dialect state machine, not mutable Plan.
func (p *Plan) ValidateEvent(event oif.Event) error {
	if !p.stream || p.receipt.Class != NativeIdentity && !p.ToolContinuation() {
		return guardFailure("/events", "admitted_event_contract")
	}
	if event.Descriptor().Operation.ID != "generation" || event.Descriptor().Dialect.ID != p.receipt.TargetDialect {
		return guardFailure("/events", "event_dialect")
	}
	if event.Control() != "" {
		if (p.Wire() == wire.Completions || p.Wire() == wire.Chat || p.Wire() == wire.Responses || p.Wire() == wire.MistralFIM) && event.Control() == "[DONE]" {
			return nil
		}
		return guardFailure("/events", "control_grammar")
	}
	document := event.Source()
	if !document.Valid() || document.Len() > p.receipt.Obligations.MaxEventBytes || document.Root().Kind() != oif.Object {
		return guardFailure("/events", "bounded_event_grammar")
	}
	root := document.Root()
	switch p.Wire() {
	case wire.Anthropic:
		kind := valueText(member(root, "type"))
		if kind == "" || event.Name() != "" && event.Name() != kind {
			return guardFailure("/events/type", "event_identity")
		}
		switch kind {
		case "message_start":
			if member(root, "message").Kind() != oif.Object {
				return guardFailure("/events/message", "message_start")
			}
		case "content_block_start":
			if !nonnegativeInteger(member(root, "index")) || member(root, "content_block").Kind() != oif.Object {
				return guardFailure("/events/content_block", "block_start")
			}
		case "content_block_delta":
			if !nonnegativeInteger(member(root, "index")) || member(root, "delta").Kind() != oif.Object {
				return guardFailure("/events/delta", "block_delta")
			}
		case "content_block_stop":
			if !nonnegativeInteger(member(root, "index")) {
				return guardFailure("/events/index", "block_stop")
			}
		case "message_delta":
			if member(root, "delta").Kind() != oif.Object {
				return guardFailure("/events/delta", "message_delta")
			}
		}
	case wire.Completions, wire.Chat, wire.MistralFIM:
		if choices, present := root.Lookup("choices"); present && choices.Kind() != oif.Array {
			return guardFailure("/events/choices", "candidate_grammar")
		}
	case wire.CohereChat:
		kind := valueText(member(root, "type"))
		if kind == "" || event.Name() != "" && event.Name() != kind {
			return guardFailure("/events/type", "event_identity")
		}
	case wire.Responses:
		if valueText(member(root, "type")) == "" {
			return guardFailure("/events/type", "event_identity")
		}
	case wire.Gemini:
		if candidates, present := root.Lookup("candidates"); present && candidates.Kind() != oif.Array {
			return guardFailure("/events/candidates", "candidate_grammar")
		}
	}
	return nil
}
