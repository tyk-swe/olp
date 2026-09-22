package interaction

import (
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func guardFailure(field, requirement string) error {
	return incompatible("fidelity_protocol_violation", field, requirement, "The provider result does not satisfy the admitted interaction contract.")
}

// ValidateUnary runs before projection. Native codecs retain source extensions;
// the qualified text result contract rejects anything its client cannot observe.
func (p *Plan) ValidateUnary(body []byte) error {
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: p.receipt.Obligations.MaxBodyBytes})
	if err != nil || document.Root().Kind() != oif.Object {
		return guardFailure("/result", "result_grammar")
	}
	// This invokes the actual native grammar, never a translator. The executor
	// then uses the admitted client projector once the guard succeeds.
	completion, err := protocols.DecodeRequest(p.Wire(), p.Wire(), body, "strict-result", "", p.EffectiveRequest())
	if err != nil {
		return guardFailure("/result", "native_result_grammar")
	}
	return p.ValidateResult(completion.Native)
}

// ValidateResult consumes a result already checked by its native dialect codec.
// The caller can retain observed usage even when a later projection or policy
// guard refuses delivery. No second native decode runs on the gateway hot path.
func (p *Plan) ValidateResult(result oif.Result) error {
	document := result.Source()
	if !document.Valid() || document.Len() > p.receipt.Obligations.MaxBodyBytes || result.Descriptor().Dialect != openai.Descriptor(p.Wire(), false).Dialect {
		return guardFailure("/result", "native_result_grammar")
	}
	if p.receipt.Class == QualifiedInteraction {
		if err := validateAnthropicTextResult(document); err != nil {
			return err
		}
	}
	if p.template.policy != nil && p.template.policy.HasOutput() {
		if err := outputPolicyCoverage(p.Wire(), document); err != nil {
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
	if !onlyMembers(usage, "input_tokens output_tokens cache_read_input_tokens") {
		return guardFailure("/usage", "unmapped_usage")
	}
	for _, name := range []string{"input_tokens", "output_tokens"} {
		if !nonnegativeInteger(member(usage, name)) {
			return guardFailure("/usage/"+name, "token_usage")
		}
	}
	if cached, present := usage.Lookup("cache_read_input_tokens"); present && !nonnegativeInteger(cached) {
		return guardFailure("/usage/cache_read_input_tokens", "token_usage")
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
	if !p.stream || p.receipt.Class != NativeIdentity {
		return guardFailure("/events", "admitted_event_contract")
	}
	if event.Descriptor().Operation.ID != "generation" || event.Descriptor().Dialect.ID != p.receipt.TargetDialect {
		return guardFailure("/events", "event_dialect")
	}
	if event.Control() != "" {
		if (p.Wire() == openai.FamilyChat || p.Wire() == openai.FamilyResponses) && event.Control() == "[DONE]" {
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
	case openai.FamilyAnthropic:
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
	case openai.FamilyChat:
		if choices, present := root.Lookup("choices"); present && choices.Kind() != oif.Array {
			return guardFailure("/events/choices", "candidate_grammar")
		}
	case openai.FamilyResponses:
		if valueText(member(root, "type")) == "" {
			return guardFailure("/events/type", "event_identity")
		}
	case openai.FamilyGemini:
		if candidates, present := root.Lookup("candidates"); present && candidates.Kind() != oif.Array {
			return guardFailure("/events/candidates", "candidate_grammar")
		}
	}
	return nil
}

func outputPolicyCoverage(wire openai.Family, document oif.Document) error {
	fail := func() error {
		return incompatible("policy_conflict", "/result", "output_policy_coverage", "The output policy cannot inspect native opaque or nontext result content.")
	}
	root := document.Root()
	var contents []oif.Value
	switch wire {
	case openai.FamilyChat:
		if !onlyMembers(root, "id object created model choices usage system_fingerprint service_tier") {
			return fail()
		}
		for _, choice := range member(root, "choices").Elements() {
			if !onlyMembers(choice, "index message finish_reason logprobs") || !emptyOptional(member(choice, "logprobs")) {
				return fail()
			}
			message := member(choice, "message")
			if !onlyMembers(message, "role content refusal annotations") || !emptyOptional(member(message, "annotations")) {
				return fail()
			}
			contents = append(contents, member(message, "content"), member(message, "refusal"))
		}
	case openai.FamilyResponses:
		if !onlyMembers(root, "id object created_at status background error incomplete_details instructions max_output_tokens max_tool_calls model output parallel_tool_calls previous_response_id prompt_cache_key reasoning safety_identifier service_tier store temperature text tool_choice tools top_logprobs top_p truncation usage user metadata conversation") {
			return fail()
		}
		for _, item := range member(root, "output").Elements() {
			if !onlyMembers(item, "id type status role content") || valueText(member(item, "type")) != "message" {
				return fail()
			}
			contents = append(contents, member(item, "content"))
		}
	case openai.FamilyAnthropic:
		if !onlyMembers(root, "id type role model content stop_reason stop_sequence usage") {
			return fail()
		}
		contents = append(contents, member(root, "content"))
	case openai.FamilyGemini:
		if !onlyMembers(root, "candidates usageMetadata modelVersion responseId promptFeedback") {
			return fail()
		}
		if feedback, present := root.Lookup("promptFeedback"); present && !onlyMembers(feedback, "blockReason safetyRatings") {
			return fail()
		}
		for _, candidate := range member(root, "candidates").Elements() {
			if !onlyMembers(candidate, "index content finishReason safetyRatings avgLogprobs tokenCount") {
				return fail()
			}
			content := member(candidate, "content")
			if !onlyMembers(content, "role parts") {
				return fail()
			}
			for _, part := range member(content, "parts").Elements() {
				if !onlyMembers(part, "text") {
					return fail()
				}
				contents = append(contents, member(part, "text"))
			}
		}
	default:
		return fail()
	}
	for _, content := range contents {
		if content.Kind() == oif.Null || content.Kind() == oif.Absent || content.Kind() == oif.String {
			continue
		}
		if content.Kind() != oif.Array {
			return fail()
		}
		for _, part := range content.Elements() {
			if !onlyMembers(part, "type text refusal annotations logprobs") || !emptyOptional(member(part, "annotations")) || !emptyOptional(member(part, "logprobs")) || !slices.Contains([]string{"text", "output_text", "refusal"}, valueText(member(part, "type"))) {
				return fail()
			}
		}
	}
	return nil
}

func emptyOptional(value oif.Value) bool {
	return value.Kind() == oif.Absent || value.Kind() == oif.Null || value.Kind() == oif.Array && len(value.Elements()) == 0
}

func outputRequestCoverage(document oif.Document) error {
	for _, path := range []string{"/tools", "/toolConfig", "/reasoning", "/reasoning_effort", "/thinking", "/generationConfig/thinkingConfig"} {
		if value, present := document.Lookup(path); present && !emptyOptional(value) {
			return incompatible("policy_conflict", path, "output_policy_coverage", "The requested native output or tool state has no complete inspection contract for this output policy.")
		}
	}
	for _, path := range []string{"/logprobs", "/top_logprobs", "/generationConfig/responseLogprobs", "/generationConfig/logprobs"} {
		if value, present := document.Lookup(path); present && value.Kind() != oif.Null && value.Raw() != "false" && value.Raw() != "0" {
			return incompatible("policy_conflict", path, "output_policy_coverage", "Native token alternatives are outside this output policy's inspection contract.")
		}
	}
	return nil
}
