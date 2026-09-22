package interaction

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// prepareText is a qualified operation mapping, independent of the legacy
// Parts/Calls converter. Its narrow input and guarded output contract keeps
// the native baseline explicit instead of approximating unsupported features.
func (t *Template) prepareText(request *openai.Request, receipt *Receipt) (oif.Prepared, error) {
	if request.Stream {
		return oif.Prepared{}, incompatible("state_carrier", "/stream", "translated_event_contract", "Translated streaming requires a separately qualified event and continuation contract.")
	}
	if request.Family != openai.FamilyChat || t.wire != openai.FamilyAnthropic {
		return oif.Prepared{}, incompatible("target_capability", "/profile", "qualified_mapping", "No qualified mapping exists for this source and target dialect pair.")
	}
	if len(request.OIF().Provenance()) != 0 {
		return oif.Prepared{}, incompatible("resource_affinity", "/request", "stateless_source", "The text mapping cannot consume rewritten resources or transformed input.")
	}
	root := request.OIF().Document().Root()
	for _, member := range root.Members() {
		switch member.Name {
		case "model", "messages", "max_tokens", "stream":
		case "max_completion_tokens", "reasoning", "reasoning_effort", "thinking":
			return oif.Prepared{}, incompatible("reasoning_budget", "/"+member.Name, "budget_scope", "This reasoning or token-budget scope has no qualified equivalent in the target contract.")
		case "tools", "tool_choice", "parallel_tool_calls", "previous_response_id", "conversation", "store", "background":
			return oif.Prepared{}, incompatible("state_carrier", safeField(member.Name), "stateless_text_contract", "This mapping does not admit tools, provider state or continuation dependencies.")
		case "response_format":
			return oif.Prepared{}, incompatible("target_capability", "/response_format", "structured_output_metadata", "Structured-output metadata has no qualified mapping in this text contract.")
		default:
			return oif.Prepared{}, incompatible("target_capability", safeField(member.Name), "source_control_mapping", "A source control has no qualified execution and observation mapping.")
		}
	}
	if stream, present := root.Lookup("stream"); present && stream.Raw() != "false" {
		return oif.Prepared{}, incompatible("target_capability", "/stream", "delivery_presence", "This mapping requires absent stream or explicit false.")
	}
	messages, _ := root.Lookup("messages")
	if messages.Kind() != oif.Array || len(messages.Elements()) == 0 {
		return oif.Prepared{}, incompatible("target_capability", "/messages", "ordered_text_history", "The text mapping requires a nonempty ordered history.")
	}
	mapped := []map[string]any{}
	previous := ""
	for i, message := range messages.Elements() {
		path := "/messages/" + strconv.Itoa(i)
		if message.Kind() != oif.Object {
			return oif.Prepared{}, incompatible("target_capability", path, "message_shape", "A message has no qualified text representation.")
		}
		role := valueText(member(message, "role"))
		if role == "system" || role == "developer" {
			return oif.Prepared{}, incompatible("instruction_scope", path+"/role", "instruction_scope", "The target text contract cannot preserve this instruction scope.")
		}
		if role != "user" && role != "assistant" {
			return oif.Prepared{}, incompatible("state_carrier", path+"/role", "text_history_role", "Tool or nontext history requires a qualified continuation contract.")
		}
		if role == previous {
			return oif.Prepared{}, incompatible("instruction_scope", path, "message_boundary", "The target would combine adjacent messages with the same role.")
		}
		previous = role
		for _, field := range message.Members() {
			if field.Name == "role" || field.Name == "content" {
				continue
			}
			code, requirement := "target_capability", "message_metadata"
			if field.Name == "tool_calls" || field.Name == "tool_call_id" || field.Name == "function_call" {
				code, requirement = "state_carrier", "tool_correspondence"
			}
			return oif.Prepared{}, incompatible(code, path, requirement, "A message member has no qualified representation in the text contract.")
		}
		content := member(message, "content")
		blocks := []map[string]json.RawMessage{}
		switch content.Kind() {
		case oif.String:
			blocks = append(blocks, map[string]json.RawMessage{"type": json.RawMessage(`"text"`), "text": content.Bytes()})
		case oif.Array:
			if len(content.Elements()) == 0 {
				return oif.Prepared{}, incompatible("target_capability", path+"/content", "content_presence", "An empty content array has no declared text equivalent.")
			}
			for _, part := range content.Elements() {
				if part.Kind() != oif.Object || valueText(member(part, "type")) != "text" || member(part, "text").Kind() != oif.String || len(part.Members()) != 2 {
					return oif.Prepared{}, incompatible("state_carrier", path+"/content", "ordered_text_nodes", "Nontext or annotated content requires a richer qualified contract.")
				}
				blocks = append(blocks, map[string]json.RawMessage{"type": json.RawMessage(`"text"`), "text": member(part, "text").Bytes()})
			}
		default:
			return oif.Prepared{}, incompatible("target_capability", path+"/content", "content_presence", "Native null and nontext content have no declared text equivalent.")
		}
		mapped = append(mapped, map[string]any{"role": role, "content": blocks})
	}
	// A final assistant prefill constrains generation in Anthropic but is an
	// ordinary history message in Chat; it is deliberately outside this mapping.
	if previous != "user" {
		return oif.Prepared{}, incompatible("instruction_scope", "/messages", "assistant_prefill", "The text contract requires a final user turn to avoid introducing assistant prefill semantics.")
	}
	fields := map[string]json.RawMessage{}
	fields["model"], _ = json.Marshal(t.serving.Model)
	fields["messages"], _ = json.Marshal(mapped)
	if stream, present := root.Lookup("stream"); present {
		fields["stream"] = stream.Bytes()
	}
	cap, present := root.Lookup("max_tokens")
	if present {
		fields["max_tokens"] = cap.Bytes()
	}
	for _, name := range slices.Sorted(maps.Keys(t.defaults)) {
		if name != "max_tokens" {
			return oif.Prepared{}, incompatible("target_capability", safeField(name), "introduced_default_mapping", "A target default introduces behavior outside the qualified text contract.")
		}
		if !present {
			fields[name] = t.defaults[name]
			receipt.Dispositions = append(receipt.Dispositions, Disposition{"/max_tokens", "introduced", "declared_target_output_cap", evidenceText})
		}
	}
	var limit int64
	if json.Unmarshal(fields["max_tokens"], &limit) != nil || limit < 1 {
		return oif.Prepared{}, incompatible("reasoning_budget", "/max_tokens", "explicit_output_cap", "The target requires a positive explicit or declared-default output cap; no budget is invented.")
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return oif.Prepared{}, err
	}
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: t.config.MaxBodyBytes})
	if err != nil {
		return oif.Prepared{}, incompatible("target_capability", "/request", "bounded_buffering", "The qualified request exceeds its compiled bounds.")
	}
	descriptor := openai.Descriptor(t.wire, false)
	prepared, err := oif.PrepareDestination(request.OIF(), descriptor, document, oif.QualifiedMapping, "qualified stateless ordered Chat text to Anthropic text; explicit nonreasoning output cap")
	receipt.Dispositions = append(receipt.Dispositions,
		Disposition{"/messages", "mapped", "ordered_user_assistant_text", evidenceText},
		Disposition{"/max_tokens", "mapped", "nonreasoning_output_token_cap", evidenceText},
		Disposition{"/model", "bound", "published_serving_identity", evidenceText},
		Disposition{"/result", "guarded", "single_text_result_with_usage_and_finish", evidenceText})
	return prepared, err
}
func member(value oif.Value, name string) oif.Value { out, _ := value.Lookup(name); return out }
func valueText(value oif.Value) string              { text, _ := value.Text(); return text }
