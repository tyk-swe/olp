package interaction

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

const ContinuationV1 = "chat-anthropic-tools-v1"
const evidenceTools = "codec-sdk-fixture/chat-anthropic-tools/v1"

// Continuation contains an operation's complete immutable next-turn dependency.
// It is sensitive: only the resource owner may persist it, encrypted. Receipts
// never include these values. Native blocks retain opaque signatures exactly.
type Continuation struct {
	Version       string          `json:"version"`
	Source        json.RawMessage `json:"source"`
	NativeRequest json.RawMessage `json:"native_request"`
	Blocks        json.RawMessage `json:"blocks"`
	Assistant     json.RawMessage `json:"assistant"`
}

func continuationFailure(field, requirement string) error {
	return incompatible("state_carrier", field, requirement, "The selected client continuation contract cannot preserve this interaction.")
}
func (t *Template) prepareTools(request *openai.Request, context Context, receipt *Receipt) (oif.Prepared, error) {
	if context.ContinuationVersion != ContinuationV1 || request.Family != openai.FamilyChat || t.wire != openai.FamilyAnthropic || t.profile.Hosting != "direct-anthropic" {
		return oif.Prepared{}, continuationFailure("/client_contract", "qualified_continuation_version")
	}
	if !context.DurableContinuation {
		return oif.Prepared{}, continuationFailure("/client_contract", "encrypted_continuation_authority")
	}
	if len(request.OIF().Provenance()) != 0 {
		return oif.Prepared{}, continuationFailure("/request", "immutable_source_history")
	}
	root := request.OIF().Document().Root()
	fields := map[string]json.RawMessage{}
	for _, entry := range root.Members() {
		switch entry.Name {
		case "model", "messages", "stream", "tools":
		case "stream_options":
			if !onlyMembers(entry.Value, "include_usage") || member(entry.Value, "include_usage").Raw() != "true" {
				return oif.Prepared{}, continuationFailure("/stream_options", "qualified_usage_observation")
			}
		case "max_tokens", "max_completion_tokens", "reasoning_effort", "thinking", "reasoning":
			return oif.Prepared{}, incompatible("reasoning_budget", safeField(entry.Name), "native_budget_baseline", "The translated reasoning workflow requires the target's declared native reasoning and output budget; source budget equivalence is not assumed.")
		default:
			return oif.Prepared{}, incompatible("target_capability", safeField(entry.Name), "qualified_source_control", "This source control has no qualified mapping in the negotiated tool contract.")
		}
	}
	if stream, present := root.Lookup("stream"); present && stream.Raw() != "true" && stream.Raw() != "false" {
		return oif.Prepared{}, continuationFailure("/stream", "delivery_presence")
	}
	if context.Continuation != nil {
		var err error
		fields, err = reconstructTools(request, context.Continuation)
		if err != nil {
			return oif.Prepared{}, err
		}
	} else {
		messages, err := initialToolMessages(member(root, "messages"))
		if err != nil {
			return oif.Prepared{}, err
		}
		fields["messages"] = messages
		for _, name := range slices.Sorted(maps.Keys(t.defaults)) {
			switch name {
			case "max_tokens", "thinking", "temperature", "top_p", "top_k", "stop_sequences", "tools", "tool_choice":
				fields[name] = bytes.Clone(t.defaults[name])
				receipt.Dispositions = append(receipt.Dispositions, Disposition{safeField(name), "introduced", "declared_native_tool_default", evidenceTools})
			default:
				return oif.Prepared{}, incompatible("target_capability", safeField(name), "qualified_default_control", "A target default is outside the negotiated native tool contract.")
			}
		}
		if tools, present := root.Lookup("tools"); present {
			mapped, err := chatTools(tools)
			if err != nil {
				return oif.Prepared{}, err
			}
			fields["tools"] = mapped
		}
		var limit int64
		if json.Unmarshal(fields["max_tokens"], &limit) != nil || limit < 1 {
			return oif.Prepared{}, incompatible("reasoning_budget", "/max_tokens", "declared_native_budget", "The target requires a declared positive native output budget.")
		}
	}
	fields["model"], _ = json.Marshal(t.serving.Model)
	delete(fields, "stream")
	if stream, present := root.Lookup("stream"); present {
		fields["stream"] = stream.Bytes()
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return oif.Prepared{}, err
	}
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: t.config.MaxBodyBytes})
	if err != nil {
		return oif.Prepared{}, continuationFailure("/request", "bounded_continuation_history")
	}
	prepared, err := oif.PrepareDestination(request.OIF(), openai.Descriptor(t.wire, request.Stream), document, oif.QualifiedMapping, "versioned ordered tool history and encrypted native continuation")
	receipt.Evidence = []string{evidenceTools}
	receipt.Obligations.Continuation = ContinuationV1
	receipt.Obligations.Lifetime = "durable"
	receipt.Obligations.Submission = "client_submission_identity"
	receipt.Obligations.Retry = "recover_same_delivery_never_replay_unknown_work"
	receipt.Dispositions = append(receipt.Dispositions, Disposition{"/messages", "mapped", "ordered_native_dependency_reconstruction", evidenceTools}, Disposition{"/tools", "mapped", "exact_function_schema", evidenceTools}, Disposition{"/result", "guarded", "ordered_observations_and_durable_actionability", evidenceTools})
	return prepared, err
}

func initialToolMessages(value oif.Value) ([]byte, error) {
	if value.Kind() != oif.Array || len(value.Elements()) == 0 {
		return nil, continuationFailure("/messages", "ordered_user_history")
	}
	out := []json.RawMessage{}
	prior := ""
	for _, message := range value.Elements() {
		role := valueText(member(message, "role"))
		if role == "system" || role == "developer" {
			return nil, incompatible("instruction_scope", "/messages", "instruction_scope", "The negotiated workflow has no qualified mapping for this instruction scope.")
		}
		if !onlyMembers(message, "role content") || (role != "user" && role != "assistant") || role == prior || member(message, "content").Kind() != oif.String {
			return nil, continuationFailure("/messages", "unmodified_text_history_or_handle")
		}
		prior = role
		out = append(out, message.Bytes())
	}
	if prior != "user" {
		return nil, incompatible("instruction_scope", "/messages", "assistant_prefill", "The negotiated workflow requires a final user turn.")
	}
	return json.Marshal(out)
}
func chatTools(value oif.Value) ([]byte, error) {
	if value.Kind() != oif.Array || len(value.Elements()) == 0 {
		return nil, continuationFailure("/tools", "function_tools")
	}
	out := []map[string]json.RawMessage{}
	names := map[string]bool{}
	for _, tool := range value.Elements() {
		fn := member(tool, "function")
		name := valueText(member(fn, "name"))
		if !onlyMembers(tool, "type function") || valueText(member(tool, "type")) != "function" || !onlyMembers(fn, "name description parameters") || name == "" || names[name] || member(fn, "parameters").Kind() != oif.Object {
			return nil, continuationFailure("/tools", "exact_function_schema")
		}
		names[name] = true
		mapped := map[string]json.RawMessage{"name": member(fn, "name").Bytes(), "input_schema": member(fn, "parameters").Bytes()}
		if description, present := fn.Lookup("description"); present {
			if description.Kind() != oif.String {
				return nil, continuationFailure("/tools", "description_presence")
			}
			mapped["description"] = description.Bytes()
		}
		out = append(out, mapped)
	}
	return json.Marshal(out)
}
func reconstructTools(request *openai.Request, prior *Continuation) (map[string]json.RawMessage, error) {
	if prior.Version != ContinuationV1 {
		return nil, continuationFailure("/continuation", "historical_contract_version")
	}
	source, err := oif.ParseJSON(prior.Source, oif.Limits{})
	if err != nil {
		return nil, continuationFailure("/continuation", "authenticated_history")
	}
	native, err := oif.ParseJSON(prior.NativeRequest, oif.Limits{})
	if err != nil {
		return nil, continuationFailure("/continuation", "authenticated_history")
	}
	root := request.OIF().Document().Root()
	for _, old := range source.Root().Members() {
		if old.Name == "messages" || old.Name == "stream" || old.Name == "stream_options" {
			continue
		}
		if !sameValue(old.Value, member(root, old.Name)) {
			return nil, continuationFailure(safeField(old.Name), "unchanged_continuation_controls")
		}
	}
	for _, next := range root.Members() {
		if next.Name == "messages" || next.Name == "stream" || next.Name == "stream_options" {
			continue
		}
		if !sameValue(next.Value, member(source.Root(), next.Name)) {
			return nil, continuationFailure(safeField(next.Name), "unchanged_continuation_controls")
		}
	}
	messages := member(root, "messages").Elements()
	old := member(source.Root(), "messages").Elements()
	if len(messages) < len(old)+2 {
		return nil, continuationFailure("/messages", "complete_corresponding_history")
	}
	for i := range old {
		if !sameValue(messages[i], old[i]) {
			return nil, continuationFailure("/messages", "unchanged_corresponding_history")
		}
	}
	assistant, err := oif.ParseJSON(prior.Assistant, oif.Limits{})
	if err != nil || !sameValue(messages[len(old)], assistant.Root()) {
		return nil, continuationFailure("/messages", "unchanged_assistant_correspondence")
	}
	blocks, err := oif.ParseJSON(prior.Blocks, oif.Limits{})
	if err != nil || blocks.Root().Kind() != oif.Array {
		return nil, continuationFailure("/continuation", "native_blocks")
	}
	calls := []string{}
	for _, block := range blocks.Root().Elements() {
		if valueText(member(block, "type")) == "tool_use" {
			calls = append(calls, valueText(member(block, "id")))
		}
	}
	next := messages[len(old)+1:]
	results := []map[string]json.RawMessage{}
	var finalUser json.RawMessage
	if len(calls) > 0 {
		if len(next) != len(calls) {
			return nil, continuationFailure("/messages", "complete_parallel_tool_results")
		}
		for i, message := range next {
			if !onlyMembers(message, "role tool_call_id content") || valueText(member(message, "role")) != "tool" || valueText(member(message, "tool_call_id")) != calls[i] || member(message, "content").Kind() != oif.String {
				return nil, continuationFailure("/messages", "ordered_tool_result_correspondence")
			}
			results = append(results, map[string]json.RawMessage{"type": json.RawMessage(`"tool_result"`), "tool_use_id": member(message, "tool_call_id").Bytes(), "content": member(message, "content").Bytes()})
		}
		content, _ := json.Marshal(results)
		finalUser, _ = json.Marshal(map[string]json.RawMessage{"role": json.RawMessage(`"user"`), "content": content})
	} else {
		if len(next) != 1 || valueText(member(next[0], "role")) != "user" || !onlyMembers(next[0], "role content") || member(next[0], "content").Kind() != oif.String {
			return nil, continuationFailure("/messages", "next_user_turn")
		}
		finalUser = next[0].Bytes()
	}
	nativeMessages := []json.RawMessage{}
	for _, message := range member(native.Root(), "messages").Elements() {
		nativeMessages = append(nativeMessages, message.Bytes())
	}
	nativeAssistant, _ := json.Marshal(map[string]json.RawMessage{"role": json.RawMessage(`"assistant"`), "content": prior.Blocks})
	nativeMessages = append(nativeMessages, nativeAssistant, finalUser)
	fields := native.Fields()
	fields["messages"], _ = json.Marshal(nativeMessages)
	return fields, nil
}

// Correspondence compares JSON structure without float conversion. Member order
// is immaterial for SDK serialization; array order and numeric lexemes remain
// authoritative, including signed zero and precision outside IEEE754.
func sameValue(a, b oif.Value) bool {
	if a.Kind() != b.Kind() {
		return false
	}
	switch a.Kind() {
	case oif.Object:
		if len(a.Members()) != len(b.Members()) {
			return false
		}
		for _, m := range a.Members() {
			v, present := b.Lookup(m.Name)
			if !present || !sameValue(m.Value, v) {
				return false
			}
		}
		return true
	case oif.Array:
		aa, bb := a.Elements(), b.Elements()
		if len(aa) != len(bb) {
			return false
		}
		for i := range aa {
			if !sameValue(aa[i], bb[i]) {
				return false
			}
		}
		return true
	case oif.String:
		return valueText(a) == valueText(b)
	default:
		return a.Raw() == b.Raw()
	}
}
func toolIndex(value oif.Value) (int, error) {
	n, err := strconv.Atoi(value.Raw())
	if err != nil || n < 0 || n >= 1024 {
		return 0, guardFailure("/events/index", "bounded_block_index")
	}
	return n, nil
}
