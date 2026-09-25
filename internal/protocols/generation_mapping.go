package protocols

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// ChatAnthropicToolsV1 is the only negotiated client continuation contract the
// built-in mappings serve: ordered Chat tool history over Anthropic Messages.
const ChatAnthropicToolsV1 = "chat-anthropic-tools-v1"

const (
	evidenceText  = "codec-fixture/chat-anthropic-stateless-text/v1"
	evidenceTools = "codec-sdk-fixture/chat-anthropic-tools/v1"
)

// GenerationMappings are the registered qualified generation mappings. Each is
// keyed by source/target dialect identity and the advertised client contract;
// a client contract absent from this table is never claimed by generic code.
func GenerationMappings() []generation.Mapping {
	return []generation.Mapping{
		{Source: DialectChat, Target: DialectAnthropic, Evidence: evidenceText, Lower: lowerChatText, ValidateResult: validateAnthropicTextResult, ProjectResult: projectChatText},
		{Source: DialectChat, Target: DialectAnthropic, ClientContract: ChatAnthropicToolsV1, Evidence: evidenceTools, Lower: lowerChatTools, ProjectResult: projectChatToolsUnary, ProjectEvents: projectChatToolEvents},
	}
}

// Mapping.Source-qualified result admission is optional: the stateless text
// mapping tightens the native result contract before the client projection.

// lowerChatText is a qualified operation mapping, independent of the legacy
// Parts/Calls converter. Its narrow input and guarded output contract keeps
// the native baseline explicit instead of approximating unsupported features.
func lowerChatText(in generation.LowerInput) (generation.Lowered, error) {
	if in.Source.Stream {
		return generation.Lowered{}, generation.Incompatible("state_carrier", "/stream", "translated_event_contract", "Translated streaming requires a separately qualified event and continuation contract.")
	}
	if len(in.Source.Request.Provenance()) != 0 {
		return generation.Lowered{}, generation.Incompatible("resource_affinity", "/request", "stateless_source", "The text mapping cannot consume rewritten resources or transformed input.")
	}
	out := generation.Lowered{Obligations: &oif.Obligations{}}
	out.Obligations.Continuation = "stateless_text_history"
	root := in.Source.Document().Root()
	for _, member := range root.Members() {
		switch member.Name {
		case "model", "messages", "max_tokens", "stream":
		case "max_completion_tokens", "reasoning", "reasoning_effort", "thinking":
			return generation.Lowered{}, generation.Incompatible("reasoning_budget", "/"+member.Name, "budget_scope", "This reasoning or token-budget scope has no qualified equivalent in the target contract.")
		case "tools", "tool_choice", "parallel_tool_calls", "previous_response_id", "conversation", "store", "background":
			return generation.Lowered{}, generation.Incompatible("state_carrier", generation.SafeField(member.Name), "stateless_text_contract", "This mapping does not admit tools, provider state or continuation dependencies.")
		case "response_format":
			return generation.Lowered{}, generation.Incompatible("target_capability", "/response_format", "structured_output_metadata", "Structured-output metadata has no qualified mapping in this text contract.")
		default:
			return generation.Lowered{}, generation.Incompatible("target_capability", generation.SafeField(member.Name), "source_control_mapping", "A source control has no qualified execution and observation mapping.")
		}
	}
	if stream, present := root.Lookup("stream"); present && stream.Raw() != "false" {
		return generation.Lowered{}, generation.Incompatible("target_capability", "/stream", "delivery_presence", "This mapping requires absent stream or explicit false.")
	}
	messages, _ := root.Lookup("messages")
	if messages.Kind() != oif.Array || len(messages.Elements()) == 0 {
		return generation.Lowered{}, generation.Incompatible("target_capability", "/messages", "ordered_text_history", "The text mapping requires a nonempty ordered history.")
	}
	mapped := []map[string]any{}
	previous := ""
	for i, message := range messages.Elements() {
		path := "/messages/" + strconv.Itoa(i)
		if message.Kind() != oif.Object {
			return generation.Lowered{}, generation.Incompatible("target_capability", path, "message_shape", "A message has no qualified text representation.")
		}
		role := generation.Text(generation.Member(message, "role"))
		if role == "system" || role == "developer" {
			return generation.Lowered{}, generation.Incompatible("instruction_scope", path+"/role", "instruction_scope", "The target text contract cannot preserve this instruction scope.")
		}
		if role != "user" && role != "assistant" {
			return generation.Lowered{}, generation.Incompatible("state_carrier", path+"/role", "text_history_role", "Tool or nontext history requires a qualified continuation contract.")
		}
		if role == previous {
			return generation.Lowered{}, generation.Incompatible("instruction_scope", path, "message_boundary", "The target would combine adjacent messages with the same role.")
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
			return generation.Lowered{}, generation.Incompatible(code, path, requirement, "A message member has no qualified representation in the text contract.")
		}
		content := generation.Member(message, "content")
		blocks := []map[string]json.RawMessage{}
		switch content.Kind() {
		case oif.String:
			blocks = append(blocks, map[string]json.RawMessage{"type": json.RawMessage(`"text"`), "text": content.Bytes()})
		case oif.Array:
			if len(content.Elements()) == 0 {
				return generation.Lowered{}, generation.Incompatible("target_capability", path+"/content", "content_presence", "An empty content array has no declared text equivalent.")
			}
			for _, part := range content.Elements() {
				if part.Kind() != oif.Object || generation.Text(generation.Member(part, "type")) != "text" || generation.Member(part, "text").Kind() != oif.String || len(part.Members()) != 2 {
					return generation.Lowered{}, generation.Incompatible("state_carrier", path+"/content", "ordered_text_nodes", "Nontext or annotated content requires a richer qualified contract.")
				}
				blocks = append(blocks, map[string]json.RawMessage{"type": json.RawMessage(`"text"`), "text": generation.Member(part, "text").Bytes()})
			}
		default:
			return generation.Lowered{}, generation.Incompatible("target_capability", path+"/content", "content_presence", "Native null and nontext content have no declared text equivalent.")
		}
		mapped = append(mapped, map[string]any{"role": role, "content": blocks})
	}
	// A final assistant prefill constrains generation in Anthropic but is an
	// ordinary history message in Chat; it is deliberately outside this mapping.
	if previous != "user" {
		return generation.Lowered{}, generation.Incompatible("instruction_scope", "/messages", "assistant_prefill", "The text contract requires a final user turn to avoid introducing assistant prefill semantics.")
	}
	fields := map[string]json.RawMessage{}
	fields["model"], _ = json.Marshal(in.Model)
	fields["messages"], _ = json.Marshal(mapped)
	if stream, present := root.Lookup("stream"); present {
		fields["stream"] = stream.Bytes()
	}
	cap, present := root.Lookup("max_tokens")
	if present {
		fields["max_tokens"] = cap.Bytes()
	}
	for _, name := range slices.Sorted(maps.Keys(in.Defaults)) {
		if name != "max_tokens" {
			return generation.Lowered{}, generation.Incompatible("target_capability", generation.SafeField(name), "introduced_default_mapping", "A target default introduces behavior outside the qualified text contract.")
		}
		if !present {
			fields[name] = in.Defaults[name]
			out.Dispositions = append(out.Dispositions, oif.Disposition{Field: "/max_tokens", Disposition: "introduced", Rule: "declared_target_output_cap", Evidence: evidenceText})
		}
	}
	var limit int64
	if json.Unmarshal(fields["max_tokens"], &limit) != nil || limit < 1 {
		return generation.Lowered{}, generation.Incompatible("reasoning_budget", "/max_tokens", "explicit_output_cap", "The target requires a positive explicit or declared-default output cap; no budget is invented.")
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return generation.Lowered{}, err
	}
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: in.MaxBodyBytes})
	if err != nil {
		return generation.Lowered{}, generation.Incompatible("target_capability", "/request", "bounded_buffering", "The qualified request exceeds its compiled bounds.")
	}
	out.Document = document
	out.Evidence = "qualified stateless ordered Chat text to Anthropic text; explicit nonreasoning output cap"
	out.Dispositions = append(out.Dispositions,
		oif.Disposition{Field: "/messages", Disposition: "mapped", Rule: "ordered_user_assistant_text", Evidence: evidenceText},
		oif.Disposition{Field: "/max_tokens", Disposition: "mapped", Rule: "nonreasoning_output_token_cap", Evidence: evidenceText},
		oif.Disposition{Field: "/model", Disposition: "bound", Rule: "published_serving_identity", Evidence: evidenceText},
		oif.Disposition{Field: "/result", Disposition: "guarded", Rule: "single_text_result_with_usage_and_finish", Evidence: evidenceText})
	return out, nil
}

// projectChatText admits and translates one Anthropic text result for the
// stateless client contract.
func projectChatText(in generation.ProjectInput, result *generation.Native, handle string) (*generation.Continuation, generation.Delivery, error) {
	completion, err := decode(openai.FamilyAnthropic, openai.FamilyChat, result.Result.Source().Bytes(), in.Source.Route, "", openai.NewSourceEnvelope(openai.FamilyChat, in.Source.Route, false, in.Source.Document()))
	if err != nil {
		return nil, generation.Delivery{}, err
	}
	return nil, generation.Delivery{Body: completion.Body}, nil
}

func validateAnthropicTextResult(document oif.Document) error {
	root := document.Root()
	if !generation.OnlyMembers(root, "id type role model content stop_reason stop_sequence usage") {
		return generation.GuardFailure("/result", "unmapped_result_member")
	}
	if generation.Text(generation.Member(root, "type")) != "message" || generation.Text(generation.Member(root, "role")) != "assistant" || generation.Text(generation.Member(root, "id")) == "" || generation.Text(generation.Member(root, "model")) == "" {
		return generation.GuardFailure("/result", "message_identity")
	}
	content := generation.Member(root, "content")
	if content.Kind() != oif.Array || len(content.Elements()) != 1 {
		return generation.GuardFailure("/content", "single_text_boundary")
	}
	part := content.Elements()[0]
	if !generation.OnlyMembers(part, "type text") || generation.Text(generation.Member(part, "type")) != "text" || generation.Member(part, "text").Kind() != oif.String {
		return generation.GuardFailure("/content/0", "unmapped_content_or_state")
	}
	if !slices.Contains([]string{"end_turn", "max_tokens"}, generation.Text(generation.Member(root, "stop_reason"))) {
		return generation.GuardFailure("/stop_reason", "terminal_outcome")
	}
	stop := generation.Member(root, "stop_sequence")
	if stop.Kind() != oif.Null && stop.Kind() != oif.Absent {
		return generation.GuardFailure("/stop_sequence", "unmapped_stop_sequence")
	}
	usage := generation.Member(root, "usage")
	if !generation.OnlyMembers(usage, "input_tokens output_tokens cache_read_input_tokens") {
		return generation.GuardFailure("/usage", "unmapped_usage")
	}
	for _, name := range []string{"input_tokens", "output_tokens"} {
		if !generation.NonnegativeInteger(generation.Member(usage, name)) {
			return generation.GuardFailure("/usage/"+name, "token_usage")
		}
	}
	if cached, present := usage.Lookup("cache_read_input_tokens"); present && !generation.NonnegativeInteger(cached) {
		return generation.GuardFailure("/usage/cache_read_input_tokens", "token_usage")
	}
	return nil
}

// lowerChatTools is the negotiated continuation mapping: ordered Chat tool
// history over Anthropic Messages with an encrypted native dependency.
func lowerChatTools(in generation.LowerInput) (generation.Lowered, error) {
	if in.Hosting != "direct-anthropic" {
		return generation.Lowered{}, generation.ContinuationFailure("/client_contract", "qualified_continuation_version")
	}
	if !in.DurableContinuation {
		return generation.Lowered{}, generation.ContinuationFailure("/client_contract", "encrypted_continuation_authority")
	}
	if !in.AllowProviderState {
		return generation.Lowered{}, generation.Incompatible("policy_conflict", "/client_contract", "allow_provider_state", "This key does not permit retention of the native dependencies needed for a continuation handle.")
	}
	if len(in.Source.Request.Provenance()) != 0 {
		return generation.Lowered{}, generation.ContinuationFailure("/request", "immutable_source_history")
	}
	out := generation.Lowered{}
	root := in.Source.Document().Root()
	fields := map[string]json.RawMessage{}
	for _, entry := range root.Members() {
		switch entry.Name {
		case "model", "messages", "stream", "tools":
		case "stream_options":
			if !generation.OnlyMembers(entry.Value, "include_usage") || generation.Member(entry.Value, "include_usage").Raw() != "true" {
				return generation.Lowered{}, generation.ContinuationFailure("/stream_options", "qualified_usage_observation")
			}
		case "max_tokens", "max_completion_tokens", "reasoning_effort", "thinking", "reasoning":
			return generation.Lowered{}, generation.Incompatible("reasoning_budget", generation.SafeField(entry.Name), "native_budget_baseline", "The translated reasoning workflow requires the target's declared native reasoning and output budget; source budget equivalence is not assumed.")
		default:
			return generation.Lowered{}, generation.Incompatible("target_capability", generation.SafeField(entry.Name), "qualified_source_control", "This source control has no qualified mapping in the negotiated tool contract.")
		}
	}
	if stream, present := root.Lookup("stream"); present && stream.Raw() != "true" && stream.Raw() != "false" {
		return generation.Lowered{}, generation.ContinuationFailure("/stream", "delivery_presence")
	}
	if in.Continuation != nil {
		var err error
		fields, err = reconstructChatTools(in.Source, in.Continuation)
		if err != nil {
			return generation.Lowered{}, err
		}
	} else {
		messages, err := initialToolMessages(generation.Member(root, "messages"))
		if err != nil {
			return generation.Lowered{}, err
		}
		fields["messages"] = messages
		for _, name := range slices.Sorted(maps.Keys(in.Defaults)) {
			switch name {
			case "max_tokens", "thinking", "temperature", "top_p", "top_k", "stop_sequences", "tools", "tool_choice":
				fields[name] = in.Defaults[name]
				out.Dispositions = append(out.Dispositions, oif.Disposition{Field: generation.SafeField(name), Disposition: "introduced", Rule: "declared_native_tool_default", Evidence: evidenceTools})
			default:
				return generation.Lowered{}, generation.Incompatible("target_capability", generation.SafeField(name), "qualified_default_control", "A target default is outside the negotiated native tool contract.")
			}
		}
		if tools, present := root.Lookup("tools"); present {
			mapped, err := chatTools(tools)
			if err != nil {
				return generation.Lowered{}, err
			}
			fields["tools"] = mapped
		}
		var limit int64
		if json.Unmarshal(fields["max_tokens"], &limit) != nil || limit < 1 {
			return generation.Lowered{}, generation.Incompatible("reasoning_budget", "/max_tokens", "declared_native_budget", "The target requires a declared positive native output budget.")
		}
	}
	fields["model"], _ = json.Marshal(in.Model)
	delete(fields, "stream")
	if stream, present := root.Lookup("stream"); present {
		fields["stream"] = stream.Bytes()
	}
	body, err := json.Marshal(fields)
	if err != nil {
		return generation.Lowered{}, err
	}
	document, err := oif.ParseJSON(body, oif.Limits{MaxBytes: in.MaxBodyBytes})
	if err != nil {
		return generation.Lowered{}, generation.ContinuationFailure("/request", "bounded_continuation_history")
	}
	out.Document = document
	out.Evidence = "versioned ordered tool history and encrypted native continuation"
	out.Obligations = &oif.Obligations{
		Continuation:         ChatAnthropicToolsV1,
		Lifetime:             "durable",
		MaxContinuationBytes: 4 << 20,
		Actionability:        "encrypted_native_dependency_before_tool_bytes",
		Submission:           "client_submission_identity",
		Retry:                "recover_same_delivery_never_replay_unknown_work",
	}
	out.Dispositions = append(out.Dispositions,
		oif.Disposition{Field: "/messages", Disposition: "mapped", Rule: "ordered_native_dependency_reconstruction", Evidence: evidenceTools},
		oif.Disposition{Field: "/tools", Disposition: "mapped", Rule: "exact_function_schema", Evidence: evidenceTools},
		oif.Disposition{Field: "/result", Disposition: "guarded", Rule: "ordered_observations_and_durable_actionability", Evidence: evidenceTools})
	return out, nil
}

func initialToolMessages(value oif.Value) ([]byte, error) {
	if value.Kind() != oif.Array || len(value.Elements()) == 0 {
		return nil, generation.ContinuationFailure("/messages", "ordered_user_history")
	}
	out := []json.RawMessage{}
	prior := ""
	for _, message := range value.Elements() {
		role := generation.Text(generation.Member(message, "role"))
		if role == "system" || role == "developer" {
			return nil, generation.Incompatible("instruction_scope", "/messages", "instruction_scope", "The negotiated workflow has no qualified mapping for this instruction scope.")
		}
		if !generation.OnlyMembers(message, "role content") || (role != "user" && role != "assistant") || role == prior || generation.Member(message, "content").Kind() != oif.String {
			return nil, generation.ContinuationFailure("/messages", "unmodified_text_history_or_handle")
		}
		prior = role
		out = append(out, message.Bytes())
	}
	if prior != "user" {
		return nil, generation.Incompatible("instruction_scope", "/messages", "assistant_prefill", "The negotiated workflow requires a final user turn.")
	}
	return json.Marshal(out)
}
func chatTools(value oif.Value) ([]byte, error) {
	if value.Kind() != oif.Array || len(value.Elements()) == 0 {
		return nil, generation.ContinuationFailure("/tools", "function_tools")
	}
	out := []map[string]json.RawMessage{}
	names := map[string]bool{}
	for _, tool := range value.Elements() {
		fn := generation.Member(tool, "function")
		name := generation.Text(generation.Member(fn, "name"))
		if !generation.OnlyMembers(tool, "type function") || generation.Text(generation.Member(tool, "type")) != "function" || !generation.OnlyMembers(fn, "name description parameters") || name == "" || names[name] || generation.Member(fn, "parameters").Kind() != oif.Object {
			return nil, generation.ContinuationFailure("/tools", "exact_function_schema")
		}
		names[name] = true
		mapped := map[string]json.RawMessage{"name": generation.Member(fn, "name").Bytes(), "input_schema": generation.Member(fn, "parameters").Bytes()}
		if description, present := fn.Lookup("description"); present {
			if description.Kind() != oif.String {
				return nil, generation.ContinuationFailure("/tools", "description_presence")
			}
			mapped["description"] = description.Bytes()
		}
		out = append(out, mapped)
	}
	return json.Marshal(out)
}
func reconstructChatTools(source generation.Source, prior *generation.Continuation) (map[string]json.RawMessage, error) {
	if prior.Version != ChatAnthropicToolsV1 {
		return nil, generation.ContinuationFailure("/continuation", "historical_contract_version")
	}
	history, err := oif.ParseJSON(prior.Source, oif.Limits{})
	if err != nil {
		return nil, generation.ContinuationFailure("/continuation", "authenticated_history")
	}
	native, err := oif.ParseJSON(prior.NativeRequest, oif.Limits{})
	if err != nil {
		return nil, generation.ContinuationFailure("/continuation", "authenticated_history")
	}
	root := source.Document().Root()
	for _, old := range history.Root().Members() {
		if old.Name == "messages" || old.Name == "stream" || old.Name == "stream_options" {
			continue
		}
		if !generation.SameValue(old.Value, generation.Member(root, old.Name)) {
			return nil, generation.ContinuationFailure(generation.SafeField(old.Name), "unchanged_continuation_controls")
		}
	}
	for _, next := range root.Members() {
		if next.Name == "messages" || next.Name == "stream" || next.Name == "stream_options" {
			continue
		}
		if !generation.SameValue(next.Value, generation.Member(history.Root(), next.Name)) {
			return nil, generation.ContinuationFailure(generation.SafeField(next.Name), "unchanged_continuation_controls")
		}
	}
	messages := generation.Member(root, "messages").Elements()
	old := generation.Member(history.Root(), "messages").Elements()
	if len(messages) < len(old)+2 {
		return nil, generation.ContinuationFailure("/messages", "complete_corresponding_history")
	}
	for i := range old {
		if !generation.SameValue(messages[i], old[i]) {
			return nil, generation.ContinuationFailure("/messages", "unchanged_corresponding_history")
		}
	}
	assistant, err := oif.ParseJSON(prior.Assistant, oif.Limits{})
	if err != nil || !generation.SameValue(messages[len(old)], assistant.Root()) {
		return nil, generation.ContinuationFailure("/messages", "unchanged_assistant_correspondence")
	}
	blocks, err := oif.ParseJSON(prior.Blocks, oif.Limits{})
	if err != nil || blocks.Root().Kind() != oif.Array {
		return nil, generation.ContinuationFailure("/continuation", "native_blocks")
	}
	calls := []string{}
	for _, block := range blocks.Root().Elements() {
		if generation.Text(generation.Member(block, "type")) == "tool_use" {
			calls = append(calls, generation.Text(generation.Member(block, "id")))
		}
	}
	next := messages[len(old)+1:]
	results := []map[string]json.RawMessage{}
	var finalUser json.RawMessage
	if len(calls) > 0 {
		if len(next) != len(calls) {
			return nil, generation.ContinuationFailure("/messages", "complete_parallel_tool_results")
		}
		for i, message := range next {
			if !generation.OnlyMembers(message, "role tool_call_id content") || generation.Text(generation.Member(message, "role")) != "tool" || generation.Text(generation.Member(message, "tool_call_id")) != calls[i] || generation.Member(message, "content").Kind() != oif.String {
				return nil, generation.ContinuationFailure("/messages", "ordered_tool_result_correspondence")
			}
			results = append(results, map[string]json.RawMessage{"type": json.RawMessage(`"tool_result"`), "tool_use_id": generation.Member(message, "tool_call_id").Bytes(), "content": generation.Member(message, "content").Bytes()})
		}
		content, _ := json.Marshal(results)
		finalUser, _ = json.Marshal(map[string]json.RawMessage{"role": json.RawMessage(`"user"`), "content": content})
	} else {
		if len(next) != 1 || generation.Text(generation.Member(next[0], "role")) != "user" || !generation.OnlyMembers(next[0], "role content") || generation.Member(next[0], "content").Kind() != oif.String {
			return nil, generation.ContinuationFailure("/messages", "next_user_turn")
		}
		finalUser = next[0].Bytes()
	}
	nativeMessages := []json.RawMessage{}
	for _, message := range generation.Member(native.Root(), "messages").Elements() {
		nativeMessages = append(nativeMessages, message.Bytes())
	}
	nativeAssistant, _ := json.Marshal(map[string]json.RawMessage{"role": json.RawMessage(`"assistant"`), "content": prior.Blocks})
	nativeMessages = append(nativeMessages, nativeAssistant, finalUser)
	fields := native.Fields()
	fields["messages"], _ = json.Marshal(nativeMessages)
	return fields, nil
}

func toolIndex(value oif.Value) (int, error) {
	n, err := strconv.Atoi(value.Raw())
	if err != nil || n < 0 || n >= 1024 {
		return 0, generation.GuardFailure("/events/index", "bounded_block_index")
	}
	return n, nil
}

// chatToolProjection is the negotiated ordered client projection over the
// dialect-owned AnthropicTrace reducer.
type chatToolProjection struct {
	validateEvent func(oif.Event) error
	prepared      oif.Prepared
	effective     oif.Document
	route         string
	stream        bool
	trace         *AnthropicTrace
	limit         int
	// Separately bounded resources (A05): dependency is the retained native
	// state materialized into the persisted Continuation; delivery is
	// projected client output retained for replay; transient is live
	// in-flight event memory released as soon as it is no longer held;
	// eventWork is the aggregate admitted-event work counter that also
	// covers events the projection discards (pings, native error envelopes).
	dependency, delivery, transient, eventWork                                  streamBudget
	blocksCharged, contentCharged, callsCharged, usageCharged, observationsHeld int
	blocks                                                                      []json.RawMessage
	active                                                                      map[int]*toolBlock
	frames                                                                      []json.RawMessage
	observations                                                                []json.RawMessage
	nativeUsage                                                                 map[string]json.RawMessage
	blocked                                                                     bool
	id, model                                                                   string
	toolCount                                                                   int
	content                                                                     strings.Builder
	calls                                                                       []map[string]any
}
type toolBlock struct {
	fields                map[string]json.RawMessage
	kind                  string
	text, signature, args strings.Builder
	held                  int // transient bytes currently charged to this open block
}

// streamBudget is one separately bounded resource of a projection. Retained
// budgets (dependency, delivery, eventWork) only grow; the transient budget
// is a gauge — credit is released as soon as the held representation is
// dropped or materializes into a retained budget.
type streamBudget struct {
	resource string
	category generation.LimitCategory
	limit    int
	used     int
}

func (b *streamBudget) charge(n int) error {
	if n < 0 || n > b.limit-b.used {
		return &generation.Exhaustion{Resource: b.resource, Category: b.category, Limit: b.limit}
	}
	b.used += n
	return nil
}

func (b *streamBudget) release(n int) {
	if n >= b.used {
		b.used = 0
		return
	}
	b.used -= n
}

func projectChatToolEvents(in generation.ProjectInput) (generation.Projection, error) {
	if in.Limit < 1 || in.Limit > 4<<20 {
		return nil, generation.ContinuationFailure("/client_contract", "bounded_tool_projection")
	}
	return &chatToolProjection{
		validateEvent: in.ValidateEvent, prepared: in.Prepared, effective: in.Effective, route: in.Source.Route, stream: in.Source.Stream,
		trace: NewAnthropicTrace(in.Limit), limit: in.Limit, active: map[int]*toolBlock{},
		dependency: streamBudget{resource: "retained_dependency_bytes", category: generation.LimitBytes, limit: in.Limit},
		delivery:   streamBudget{resource: "projected_delivery_bytes", category: generation.LimitBytes, limit: in.Limit},
		transient:  streamBudget{resource: "transient_event_bytes", category: generation.LimitBytes, limit: in.Limit},
		eventWork:  streamBudget{resource: "admitted_event_work_bytes", category: generation.LimitEventWork, limit: in.Limit},
	}, nil
}

// Accounting reports the projection's separated resource counters so callers
// and tests can distinguish retained dependency state, projected replay
// delivery, live transient memory and aggregate admitted event work.
func (p *chatToolProjection) Accounting() generation.Accounting {
	return generation.Accounting{
		RetainedDependencyBytes: p.dependency.used,
		ProjectedDeliveryBytes:  p.delivery.used,
		TransientEventBytes:     p.transient.used,
		AdmittedEventWorkBytes:  p.eventWork.used,
	}
}
func (p *chatToolProjection) chunk(delta any, observation map[string]any) ([]byte, error) {
	extension := map[string]any{"version": ChatAnthropicToolsV1, "observation": observation}
	frame, err := json.Marshal(map[string]any{"id": p.id, "object": "chat.completion.chunk", "created": 0, "model": p.route, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}, "olp": extension})
	if err != nil {
		return nil, err
	}
	observationRaw, _ := json.Marshal(observation)
	// The projected frame is delivery retained for replay — it is not
	// retained dependency state. The aggregate observations array is held
	// bookkeeping memory: unary delivery embeds it in the body (charged when
	// Complete serializes that body) while streaming drops it at Complete.
	if err = p.delivery.charge(len(frame)); err != nil {
		return nil, err
	}
	if err = p.transient.charge(len(observationRaw)); err != nil {
		return nil, err
	}
	p.observationsHeld += len(observationRaw)
	p.frames = append(p.frames, json.RawMessage(frame))
	p.observations = append(p.observations, observationRaw)
	if p.blocked {
		return nil, nil
	}
	return frame, nil
}

// Preserve exact provider usage categories in the negotiated extension. The
// ordinary Chat usage object has no fields for Anthropic cache-write TTLs.
// Unknown categories fail closed before a ready result can be published.
func (p *chatToolProjection) recordNativeUsage(usage oif.Value) error {
	if !generation.OnlyMembers(usage, "input_tokens output_tokens cache_read_input_tokens cache_creation_input_tokens cache_creation") {
		return generation.GuardFailure("/usage", "qualified_native_usage_categories")
	}
	if p.nativeUsage == nil {
		p.nativeUsage = map[string]json.RawMessage{}
	}
	for _, field := range usage.Members() {
		if field.Name == "cache_creation" {
			if !generation.OnlyMembers(field.Value, "ephemeral_5m_input_tokens ephemeral_1h_input_tokens") {
				return generation.GuardFailure("/usage/cache_creation", "qualified_native_cache_ttls")
			}
			for _, detail := range field.Value.Members() {
				if !generation.NonnegativeInteger(detail.Value) {
					return generation.GuardFailure("/usage/cache_creation", "native_cache_token_count")
				}
			}
		} else if !generation.NonnegativeInteger(field.Value) {
			return generation.GuardFailure("/usage/"+field.Name, "native_token_count")
		}
		raw := field.Value.Bytes()
		// The retained usage map is in-flight memory until Complete
		// materializes it into the terminal delivery; replacement members
		// release their transient credit.
		if old, present := p.nativeUsage[field.Name]; present {
			p.transient.release(len(old))
			p.usageCharged -= len(old)
		}
		if err := p.transient.charge(len(raw)); err != nil {
			return err
		}
		p.usageCharged += len(raw)
		p.nativeUsage[field.Name] = raw
	}
	return nil
}

// Observe consumes one reducer-admitted native event synchronously. Event
// identity, ordering, block lifecycle and cumulative usage come from the
// dialect-owned AnthropicTrace; this type only applies the qualified
// projection contract on top. Before a tool starts, non-actionable
// text/reasoning observations are incremental. A tool turn's complete
// assistant history (including later blocks/signatures) is required to
// reconstruct the next native request, so that dependency gates its tool
// bytes.
func (p *chatToolProjection) Observe(event oif.Event) ([][]byte, error) {
	if err := p.validateEvent(event); err != nil {
		return nil, err
	}
	tr, err := p.trace.Accept(event)
	if err != nil {
		// The reducer's own bound guards live partial tool arguments — the
		// transient event memory it must hold while a block stays open.
		if errors.Is(err, openai.ErrEventTooLarge) {
			return nil, &generation.Exhaustion{Resource: "transient_event_bytes", Category: generation.LimitBytes, Limit: p.limit}
		}
		return nil, err
	}
	// Every admitted event spends aggregate event work — transport, parse and
	// projection effort proportional to its source — including events the
	// grammar drops. The source envelope itself is transient credit that
	// releases when Observe returns because nothing here retains it.
	sourceBytes := event.Source().Len()
	if err := p.eventWork.charge(sourceBytes); err != nil {
		return nil, err
	}
	if err := p.transient.charge(sourceBytes); err != nil {
		return nil, err
	}
	defer p.transient.release(sourceBytes)
	root := event.Source().Root()
	var frame []byte
	switch tr.Kind {
	case AnthropicPing, AnthropicError:
		// Admitted transport traffic with no projected output; the stream
		// codec reports a native error event's upstream detail itself. The
		// source consumed event work and held a transient envelope, but
		// retains no dependency or delivery state.
		return nil, nil
	case AnthropicMessageStart:
		message := tr.Message
		if !generation.OnlyMembers(root, "type message") || !generation.OnlyMembers(message, "id type role model content stop_reason stop_sequence usage") || len(generation.Member(message, "content").Elements()) != 0 {
			return nil, generation.GuardFailure("/events", "message_start_contract")
		}
		p.id = generation.Text(generation.Member(message, "id"))
		p.model = generation.Text(generation.Member(message, "model"))
		if usage := tr.Usage; usage.Kind() == oif.Object {
			if err := p.recordNativeUsage(usage); err != nil {
				return nil, err
			}
		}
	case AnthropicBlockStart:
		if !generation.OnlyMembers(root, "type index content_block") {
			return nil, generation.GuardFailure("/events", "block_start_contract")
		}
		index, e := toolIndex(generation.Member(root, "index"))
		if e != nil {
			return nil, e
		}
		if index != len(p.blocks) {
			return nil, generation.GuardFailure("/events/index", "ordered_block_start")
		}
		block := tr.Block
		b := &toolBlock{fields: map[string]json.RawMessage{}, kind: generation.Text(generation.Member(block, "type"))}
		held := 0
		for _, f := range block.Members() {
			raw := f.Value.Bytes()
			held += len(raw)
			b.fields[f.Name] = raw
		}
		switch b.kind {
		case "text":
			if !generation.OnlyMembers(block, "type text") || generation.Member(block, "text").Kind() != oif.String {
				return nil, generation.GuardFailure("/content", "text_block")
			}
			text := generation.Text(generation.Member(block, "text"))
			b.text.WriteString(text)
			held += len(text)
		case "thinking":
			if !generation.OnlyMembers(block, "type thinking signature") || generation.Member(block, "thinking").Kind() != oif.String || generation.Member(block, "signature").Kind() != oif.String {
				return nil, generation.GuardFailure("/content", "thinking_block")
			}
			thinking, signature := generation.Text(generation.Member(block, "thinking")), generation.Text(generation.Member(block, "signature"))
			b.text.WriteString(thinking)
			b.signature.WriteString(signature)
			held += len(thinking) + len(signature)
		case "redacted_thinking":
			if !generation.OnlyMembers(block, "type data") || generation.Member(block, "data").Kind() != oif.String {
				return nil, generation.GuardFailure("/content", "opaque_reasoning_block")
			}
		case "tool_use":
			if !generation.OnlyMembers(block, "type id name input") || generation.Text(generation.Member(block, "id")) == "" || generation.Text(generation.Member(block, "name")) == "" || generation.Member(block, "input").Kind() != oif.Object {
				return nil, generation.GuardFailure("/content", "tool_block")
			}
			// Even the name or partial arguments could trigger an ordinary SDK caller.
			p.blocked = true
			if len(generation.Member(block, "input").Members()) != 0 {
				args := generation.Member(block, "input").Raw()
				b.args.WriteString(args)
				held += len(args)
			}
		default:
			return nil, generation.GuardFailure("/content", "qualified_native_block")
		}
		// The open block's member copies and accumulating builders are
		// transient memory for the rest of the block's lifetime.
		if err := p.transient.charge(held); err != nil {
			return nil, err
		}
		b.held = held
		p.blocks = append(p.blocks, nil)
		p.active[index] = b
		change := map[string]any{}
		observation := map[string]any{"index": index, "type": b.kind, "phase": "start"}
		if b.text.Len() > 0 {
			observation["text"] = b.text.String()
			if b.kind == "text" {
				change["content"] = b.text.String()
				if err := p.transient.charge(b.text.Len()); err != nil {
					return nil, err
				}
				p.content.WriteString(b.text.String())
				p.contentCharged += b.text.Len()
			}
		}
		frame, err = p.chunk(change, observation)
	case AnthropicBlockDelta:
		if !generation.OnlyMembers(root, "type index delta") {
			return nil, generation.GuardFailure("/events", "delta_contract")
		}
		index, e := toolIndex(generation.Member(root, "index"))
		if e != nil {
			return nil, e
		}
		b := p.active[index]
		if b == nil {
			return nil, generation.GuardFailure("/events", "active_block")
		}
		delta := tr.Delta
		deltaKind := generation.Text(generation.Member(delta, "type"))
		switch deltaKind {
		case "text_delta", "thinking_delta":
			field, expected := "text", "text"
			if deltaKind == "thinking_delta" {
				field, expected = "thinking", "thinking"
			}
			if b.kind != expected || !generation.OnlyMembers(delta, "type "+field) || generation.Member(delta, field).Kind() != oif.String {
				return nil, generation.GuardFailure("/events/delta", "typed_text_delta")
			}
			text := generation.Text(generation.Member(delta, field))
			if err := p.transient.charge(len(text)); err != nil {
				return nil, err
			}
			b.text.WriteString(text)
			b.held += len(text)
			change := map[string]any{}
			if b.kind == "text" {
				change["content"] = text
				if err := p.transient.charge(len(text)); err != nil {
					return nil, err
				}
				p.content.WriteString(text)
				p.contentCharged += len(text)
			}
			frame, err = p.chunk(change, map[string]any{"index": index, "type": b.kind, "phase": "delta", "text": text})
		case "signature_delta":
			if b.kind != "thinking" || !generation.OnlyMembers(delta, "type signature") || generation.Member(delta, "signature").Kind() != oif.String {
				return nil, generation.GuardFailure("/events/delta", "native_signature_delta")
			}
			signature := generation.Text(generation.Member(delta, "signature"))
			if err := p.transient.charge(len(signature)); err != nil {
				return nil, err
			}
			b.signature.WriteString(signature)
			b.held += len(signature)
		case "input_json_delta":
			if b.kind != "tool_use" || !generation.OnlyMembers(delta, "type partial_json") || generation.Member(delta, "partial_json").Kind() != oif.String {
				return nil, generation.GuardFailure("/events/delta", "native_tool_arguments")
			}
			part := generation.Text(generation.Member(delta, "partial_json"))
			if err := p.transient.charge(len(part)); err != nil {
				return nil, err
			}
			b.args.WriteString(part)
			b.held += len(part)
		default:
			return nil, generation.GuardFailure("/events/delta", "qualified_native_delta")
		}
	case AnthropicBlockStop:
		if !generation.OnlyMembers(root, "type index") {
			return nil, generation.GuardFailure("/events", "block_stop_contract")
		}
		index, e := toolIndex(generation.Member(root, "index"))
		if e != nil {
			return nil, e
		}
		b := p.active[index]
		if b == nil {
			return nil, generation.GuardFailure("/events", "active_block")
		}
		delta := map[string]any{}
		observation := map[string]any{"index": index, "type": b.kind, "phase": "end"}
		hold := func(raw json.RawMessage) error {
			if e := p.transient.charge(len(raw)); e != nil {
				return e
			}
			b.held += len(raw)
			return nil
		}
		switch b.kind {
		case "text":
			raw, _ := json.Marshal(b.text.String())
			if e := hold(raw); e != nil {
				return nil, e
			}
			b.fields["text"] = raw
		case "thinking":
			if b.signature.Len() == 0 {
				return nil, generation.GuardFailure("/content", "complete_native_signature")
			}
			thinking, _ := json.Marshal(b.text.String())
			signature, _ := json.Marshal(b.signature.String())
			if e := hold(thinking); e != nil {
				return nil, e
			}
			if e := hold(signature); e != nil {
				return nil, e
			}
			b.fields["thinking"] = thinking
			b.fields["signature"] = signature
			observation["opaque_state"] = true
		case "redacted_thinking":
			observation["opaque_state"] = true
		case "tool_use":
			args := b.args.String()
			if args == "" {
				args = "{}"
			}
			document, e := oif.ParseJSON([]byte(args), oif.Limits{MaxBytes: p.limit})
			if e != nil || document.Root().Kind() != oif.Object {
				return nil, generation.GuardFailure("/content/input", "complete_native_tool_arguments")
			}
			if e := hold(document.Bytes()); e != nil {
				return nil, e
			}
			b.fields["input"] = document.Bytes()
			id, name := stringField(b.fields, "id"), stringField(b.fields, "name")
			if !declaresTool(p.effective, name) {
				return nil, generation.GuardFailure("/content/name", "declared_tool_correspondence")
			}
			for _, call := range p.calls {
				if call["id"] == id {
					return nil, generation.GuardFailure("/content/id", "unique_tool_identity")
				}
			}
			fn := map[string]any{"name": name, "arguments": args}
			call := map[string]any{"id": id, "type": "function", "function": fn}
			callBytes, _ := json.Marshal(call)
			// The recorded call entry is held until Complete builds the
			// assistant representation.
			if e := p.transient.charge(len(callBytes)); e != nil {
				return nil, e
			}
			p.callsCharged += len(callBytes)
			p.calls = append(p.calls, call)
			delta["tool_calls"] = []any{map[string]any{"index": p.toolCount, "id": id, "type": "function", "function": fn}}
			observation["call_id"] = id
			observation["name"] = name
			p.toolCount++
		}
		p.blocks[index], err = json.Marshal(b.fields)
		if err != nil {
			return nil, err
		}
		// The closed block's marshal is the actual retained dependency
		// representation; the open-block copies and builders release their
		// transient credit.
		if err = p.dependency.charge(len(p.blocks[index])); err != nil {
			return nil, err
		}
		p.blocksCharged += len(p.blocks[index])
		p.transient.release(b.held)
		delete(p.active, index)
		frame, err = p.chunk(delta, observation)
	case AnthropicMessageDelta:
		if !generation.OnlyMembers(root, "type delta usage") || len(p.active) > 0 {
			return nil, generation.GuardFailure("/events", "message_delta_contract")
		}
		if usage := tr.Usage; usage.Kind() == oif.Object {
			if err := p.recordNativeUsage(usage); err != nil {
				return nil, err
			}
		}
		if delta := tr.Delta; delta.Kind() == oif.Object && !generation.OnlyMembers(delta, "stop_reason stop_sequence") {
			return nil, generation.GuardFailure("/events", "terminal_metadata")
		}
		// Interim updates may carry no terminal declaration; once declared,
		// the same native fact is revalidated on every later update.
		if reason, declared := p.trace.StopReason(); declared {
			if reason != "end_turn" && reason != "tool_use" && reason != "max_tokens" && reason != "stop_sequence" {
				return nil, generation.GuardFailure("/events/stop_reason", "known_terminal")
			}
			if (reason == "tool_use") != (p.toolCount > 0) {
				return nil, generation.GuardFailure("/events/stop_reason", "tool_terminal_correspondence")
			}
			// A matched sequence belongs only to a stop_sequence terminal.
			if _, matched := p.trace.StopSequence(); matched && reason != "stop_sequence" {
				return nil, generation.GuardFailure("/events/stop_sequence", "terminal_sequence_correspondence")
			}
		}
	case AnthropicMessageStop:
		reason, declared := p.trace.StopReason()
		sequence, matched := p.trace.StopSequence()
		if !generation.OnlyMembers(root, "type") || !declared || len(p.active) > 0 {
			return nil, generation.GuardFailure("/events", "complete_native_terminal")
		}
		// The native contract defines stop_sequence as the matched sequence
		// that caused the stop: a stop_sequence reason without a declared
		// match — or a declared match under any other reason — contradicts
		// the accepted terminal. An empty match is no match at all and would
		// produce a record the committed carrier itself rejects.
		if (reason == "stop_sequence") != matched || (matched && sequence == "") {
			return nil, generation.GuardFailure("/events/stop_sequence", "terminal_sequence_correspondence")
		}
	default:
		return nil, generation.GuardFailure("/events", "qualified_native_event")
	}
	if err != nil {
		return nil, err
	}
	if len(frame) == 0 {
		return nil, nil
	}
	return [][]byte{frame}, nil
}
func stringField(fields map[string]json.RawMessage, name string) string {
	var out string
	_ = json.Unmarshal(fields[name], &out)
	return out
}
func declaresTool(effective oif.Document, name string) bool {
	for _, tool := range generation.Member(effective.Root(), "tools").Elements() {
		if generation.Text(generation.Member(tool, "name")) == name {
			return true
		}
	}
	return false
}

// Complete runs only after the authoritative native reducer has accepted the
// terminal boundary. It builds a bounded delivery; the caller still must commit
// it and its dependency state before emitting the queued frames or handle.
func (p *chatToolProjection) Complete(native *generation.Native, handle string) (*generation.Continuation, generation.Delivery, error) {
	reason, declared := p.trace.StopReason()
	sequence, matched := p.trace.StopSequence()
	// The terminal/content correspondence is revalidated here so a record can
	// never be completed from a trace whose declared reason and matched
	// sequence contradict each other.
	if !p.trace.Terminal() || native == nil || native.Usage == nil || len(p.nativeUsage) == 0 || handle == "" || !declared || (reason == "stop_sequence") != matched || (matched && sequence == "") {
		return nil, generation.Delivery{}, generation.GuardFailure("/result", "complete_recoverable_result")
	}
	assistant := map[string]any{"role": "assistant", "content": p.content.String()}
	if len(p.calls) > 0 {
		assistant["tool_calls"] = p.calls
	}
	assistantRaw, _ := json.Marshal(assistant)
	blocks, _ := json.Marshal(p.blocks)
	state := &generation.Continuation{Version: ChatAnthropicToolsV1, Source: p.prepared.Request().Document().Bytes(), NativeRequest: p.prepared.Document().Bytes(), Blocks: blocks, Assistant: assistantRaw}
	usage := map[string]any{"prompt_tokens": native.Usage.InputTokens, "completion_tokens": native.Usage.OutputTokens, "total_tokens": native.Usage.TotalTokens}
	if native.Usage.CachedInputTokens != nil {
		usage["prompt_tokens_details"] = map[string]any{"cached_tokens": *native.Usage.CachedInputTokens}
	}
	// The terminal observation comes only from the reducer's admitted native
	// facts — the declared reason, the matched-sequence member presence — with
	// the compatible client finish reason recorded beside it, never derived
	// back from the narrowed projection.
	finish := native.FinishReason
	if reason == "tool_use" {
		finish = "tool_calls"
	}
	terminalRecord := &generation.NativeTerminal{Reason: reason, Finish: finish}
	switch p.trace.StopSequencePresence() {
	case oif.Present:
		sequence, _ := p.trace.StopSequence()
		terminalRecord.Sequence, _ = json.Marshal(sequence)
	case oif.ExplicitNull:
		terminalRecord.Sequence = json.RawMessage("null")
	}
	recordRaw, _ := json.Marshal(terminalRecord)
	// The explicit actionability claim is computed from the admitted native
	// outcome: a tool action exists only when the native terminal yielded
	// tool use and every call's complete arguments and ordered dependencies
	// already validated. Any other terminal — output limit, end of turn or a
	// matched stop sequence — commits ready with an empty claim, never with
	// an implied one.
	actions := &generation.ContinuationActions{ToolCalls: []string{}}
	if reason == "tool_use" {
		for _, call := range p.calls {
			id, _ := call["id"].(string)
			if id == "" {
				return nil, generation.Delivery{}, generation.GuardFailure("/result", "complete_recoverable_result")
			}
			actions.ToolCalls = append(actions.ToolCalls, id)
		}
	}
	actionsRaw, _ := json.Marshal(actions)
	extension := map[string]any{"version": ChatAnthropicToolsV1, "handle": handle, "ready": true, "native_usage": p.nativeUsage, "native_terminal": terminalRecord, "actions": actions}
	terminal, _ := json.Marshal(map[string]any{"id": p.id, "object": "chat.completion.chunk", "created": 0, "model": p.route, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}, "usage": usage, "olp": extension})
	// Retained dependency: the persisted interaction representation — source,
	// native request, serialized assistant and the block array's own
	// serialization overhead (member bytes were charged as blocks closed).
	if err := p.dependency.charge(len(assistantRaw) + len(state.Source) + len(state.NativeRequest) + len(blocks) - p.blocksCharged); err != nil {
		return nil, generation.Delivery{}, err
	}
	// The terminal record and action claim are retained delivery state
	// committed on the carrier in both modes; their standalone serializations
	// charge the delivery budget once here while the copies embedded in the
	// terminal frame or unary body are charged with those serializations.
	if err := p.delivery.charge(len(recordRaw) + len(actionsRaw)); err != nil {
		return nil, generation.Delivery{}, err
	}
	delivery := generation.Delivery{Stream: p.stream, Terminal: terminalRecord, Actions: actions}
	extension["observations"] = p.observations
	if p.stream {
		// The terminal frame is replayed delivery; its serialization is
		// charged where it is actually retained.
		if err := p.delivery.charge(len(terminal)); err != nil {
			return nil, generation.Delivery{}, err
		}
		frames := append([]json.RawMessage{}, p.frames...)
		frames = append(frames, terminal)
		delivery.Frames = frames
	} else {
		body, _ := json.Marshal(map[string]any{"id": p.id, "object": "chat.completion", "created": 0, "model": p.route, "choices": []any{map[string]any{"index": 0, "message": assistant, "finish_reason": finish}}, "usage": usage, "olp": extension})
		if err := p.delivery.charge(len(body)); err != nil {
			return nil, generation.Delivery{}, err
		}
		delivery.Body = body
	}
	// The in-flight content, call, usage and aggregate-observation
	// representations have materialized into the retained state and delivery
	// (or are dropped, for the streaming aggregate); their transient credit
	// releases.
	p.transient.release(p.contentCharged + p.callsCharged + p.usageCharged + p.observationsHeld)
	p.contentCharged, p.callsCharged, p.usageCharged, p.observationsHeld = 0, 0, 0, 0
	return state, delivery, nil
}

// projectChatToolsUnary uses the same native block/correspondence projection
// as streaming, after the normal native result codec has established its
// grammar and usage.
func projectChatToolsUnary(in generation.ProjectInput, result *generation.Native, handle string) (*generation.Continuation, generation.Delivery, error) {
	projection, err := projectChatToolEvents(in)
	if err != nil {
		return nil, generation.Delivery{}, err
	}
	document := result.Result.Source()
	root := document.Root()
	if !generation.OnlyMembers(root, "id type role model content stop_reason stop_sequence usage") {
		return nil, generation.Delivery{}, generation.GuardFailure("/result", "qualified_native_result")
	}
	// The synthesized events are admitted through the same authoritative
	// reducer as wire traffic — no parallel grammar runs for unary results.
	seq := uint64(0)
	observe := func(name string, body any) error {
		raw, _ := json.Marshal(body)
		doc, e := oif.ParseJSON(raw, oif.Limits{MaxBytes: in.Limit})
		if e != nil {
			return e
		}
		event, e := oif.NewEvent(generation.Descriptor(DialectAnthropic, true), doc, name, seq)
		seq++
		if e != nil {
			return e
		}
		_, e = projection.Observe(event)
		return e
	}
	fields := document.Fields()
	fields["content"] = json.RawMessage(`[]`)
	fields["stop_reason"] = json.RawMessage(`null`)
	// Anthropic message_start always opens with both terminal members null;
	// the result's terminal facts enter through the synthesized delta below.
	fields["stop_sequence"] = json.RawMessage(`null`)
	if err = observe("message_start", map[string]any{"type": "message_start", "message": fields}); err != nil {
		return nil, generation.Delivery{}, err
	}
	for i, block := range generation.Member(root, "content").Elements() {
		if err = observe("content_block_start", map[string]any{"type": "content_block_start", "index": i, "content_block": json.RawMessage(block.Bytes())}); err != nil {
			return nil, generation.Delivery{}, err
		}

		if err = observe("content_block_stop", map[string]any{"type": "content_block_stop", "index": i}); err != nil {
			return nil, generation.Delivery{}, err
		}
	}
	// The synthesized update preserves the result's terminal members exactly:
	// an absent stop_sequence stays absent rather than collapsing into null.
	delta := map[string]json.RawMessage{"stop_reason": generation.Member(root, "stop_reason").Bytes()}
	if sequence, present := root.Lookup("stop_sequence"); present {
		delta["stop_sequence"] = sequence.Bytes()
	}
	if err = observe("message_delta", map[string]any{"type": "message_delta", "delta": delta, "usage": json.RawMessage(generation.Member(root, "usage").Bytes())}); err != nil {
		return nil, generation.Delivery{}, err
	}
	if err = observe("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return nil, generation.Delivery{}, err
	}
	return projection.Complete(result, handle)
}
