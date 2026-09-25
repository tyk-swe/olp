package protocols

import (
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operations/generation"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// This file carries the dialect-owned admission contracts the interaction
// planner used to hard-code. Each hook is registered on its dialect; generic
// orchestration only invokes them.

// wireEffects is the dialect's declared-effects contract: provider-state and
// tool declarations in the effective document amend the obligation record and
// report the provider-hosted tool families admitted under the caller's
// permission and the profile's qualified lifecycle contracts. The grammar
// positions are the dialect's own.
func wireEffects(family openai.Family) func(effective oif.Document, in generation.StateInput, obligations *oif.Obligations) ([]string, error) {
	return func(effective oif.Document, in generation.StateInput, obligations *oif.Obligations) ([]string, error) {
		root := effective.Root()
		background, _ := root.Lookup("background")
		queued := background.Raw() == "true"
		if queued && (family != openai.FamilyResponses || generation.Member(root, "store").Raw() == "false") {
			return nil, generation.Incompatible("state_carrier", "/background", "durable_lifecycle", "Background Responses require retained native state.")
		}
		retained := generation.Member(root, "store").Raw() == "true"
		if family == openai.FamilyResponses {
			store, present := root.Lookup("store")
			retained = !present || store.Kind() == oif.Null || store.Raw() != "false"
		}
		if queued {
			retained = true
			obligations.Submission = "queued"
		}
		referenced := false
		unsupportedReference := false
		for _, name := range []string{"previous_response_id", "conversation", "cachedContent"} {
			value, present := root.Lookup(name)
			if !present || value.Kind() == oif.Null {
				continue
			}
			referenced = true
			if name != "previous_response_id" {
				unsupportedReference = true
			}
			if name == "conversation" {
				retained = true
			}
			if in.RequiredServing == nil {
				return nil, generation.Incompatible("resource_affinity", "/"+name, "resolved_resource_affinity", "Provider resources require resolved historical serving authority.")
			}
		}
		if (retained || referenced) && !in.AllowProviderState {
			return nil, generation.Incompatible("policy_conflict", "/store", "provider_state_authorization", "The native invocation retains or reads provider state but the caller does not permit it.")
		}
		if retained || referenced {
			if family != openai.FamilyResponses || !in.RetainedResponses || unsupportedReference {
				return nil, generation.Incompatible("state_carrier", "/resources", "historical_resource_contract", "Provider-retained strict continuation requires a qualified historical serving and resource reconstruction contract.")
			}
			obligations.Lifetime = "durable"
			obligations.Continuation = "native_response_resource"
			if retained {
				obligations.Effects = append(obligations.Effects, "resource_mutation")
			}
			if referenced {
				obligations.Effects = append(obligations.Effects, "resource_read")
			}
		}
		client, hostedTools, err := classifyRequestTools(root, family)
		if err != nil {
			return nil, err
		}
		if client {
			obligations.Effects = append(obligations.Effects, "client_tool_call")
		}
		hosted, err := admitHostedTools(hostedTools, in, obligations)
		if err != nil {
			return nil, err
		}
		if err := checkHostedSelectors(root, family, hosted); err != nil {
			return nil, err
		}
		return hosted, nil
	}
}

func responsesEffects(effective oif.Document, in generation.StateInput, obligations *oif.Obligations) ([]string, error) {
	return wireEffects(openai.FamilyResponses)(effective, in, obligations)
}
func anthropicEffects(effective oif.Document, in generation.StateInput, obligations *oif.Obligations) ([]string, error) {
	return wireEffects(openai.FamilyAnthropic)(effective, in, obligations)
}
func bedrockEffects(effective oif.Document, in generation.StateInput, obligations *oif.Obligations) ([]string, error) {
	return wireEffects(openai.FamilyBedrock)(effective, in, obligations)
}

// admittedFileKinds are the only Responses part types whose file_id member a
// resolved binding may occupy. Every other provider-asset position keeps the
// historical refusal; a local ID is never a claim the request document can
// widen into a new position.
var admittedFileKinds = map[string]bool{"input_file": true, "input_image": true, "computer_screenshot": true}

// wireAssets surveys the dialect-owned provider-asset positions of one
// effective document in document order. Collecting a reference never grants
// it meaning: file-reference members are reported for the orchestrator's
// resource-authority binding check — Bindable marks the positions a verified
// binding may occupy — while provider-asset forms no binding can qualify
// report Refuse. A tool's own JSON may contain identically named keys without
// referring to provider state, so only the dialect's media positions are
// visited.
func wireAssets(family openai.Family) func(effective oif.Document) []generation.AssetRef {
	return func(document oif.Document) []generation.AssetRef {
		var refs []generation.AssetRef
		visit := func(value oif.Value, pointer, kind string) {
			id, text := value.Text()
			refs = append(refs, generation.AssetRef{Pointer: pointer, ID: id, Text: text, Bindable: family == openai.FamilyResponses && admittedFileKinds[kind]})
		}
		refuse := func(pointer string) {
			refs = append(refs, generation.AssetRef{Pointer: pointer, Refuse: true})
		}
		openaiFilePart := func(part oif.Value, path, kind string) {
			if kind != "file" && !admittedFileKinds[kind] {
				return
			}
			if value, present := part.Lookup("file_id"); present && value.Kind() != oif.Null {
				visit(value, path+"/file_id", kind)
			}
			if value, present := generation.Member(part, "file").Lookup("file_id"); present && value.Kind() != oif.Null {
				visit(value, path+"/file/file_id", kind)
			}
		}
		var parts func(oif.Value, string)
		parts = func(value oif.Value, path string) {
			for index, part := range value.Elements() {
				at := oif.Pointer(path, strconv.Itoa(index))
				kind := generation.Text(generation.Member(part, "type"))
				switch family {
				case openai.FamilyChat:
					openaiFilePart(part, at, kind)
				case openai.FamilyAnthropic:
					if kind == "image" || kind == "document" || kind == "container_upload" {
						if value, present := part.Lookup("file_id"); present && value.Kind() != oif.Null {
							visit(value, at+"/file_id", kind)
						}
						source := generation.Member(part, "source")
						if value, present := source.Lookup("file_id"); present && value.Kind() != oif.Null {
							visit(value, at+"/source/file_id", kind)
						} else if generation.Text(generation.Member(source, "type")) == "file" {
							refuse(at + "/source")
						}
					}
					if kind == "tool_result" {
						parts(generation.Member(part, "content"), at+"/content")
					}
				case openai.FamilyGemini, openai.FamilyGeminiStream:
					uri := generation.Text(generation.Member(generation.Member(part, "fileData"), "fileUri"))
					if uri != "" {
						parsed, err := url.Parse(uri)
						if err != nil || strings.HasPrefix(strings.TrimPrefix(uri, "/"), "files/") || strings.EqualFold(strings.TrimSuffix(parsed.Hostname(), "."), "generativelanguage.googleapis.com") || strings.EqualFold(parsed.Scheme, "gs") || strings.EqualFold(parsed.Scheme, "s3") {
							refuse(at + "/fileData/fileUri")
						}
					}
				case openai.FamilyBedrock:
					for _, kind := range []string{"image", "document", "video"} {
						if _, present := generation.Member(generation.Member(part, kind), "source").Lookup("s3Location"); present {
							refuse(at + "/" + kind + "/source/s3Location")
						}
					}
					parts(generation.Member(generation.Member(part, "toolResult"), "content"), at+"/toolResult/content")
				}
			}
		}
		root := document.Root()
		if family == openai.FamilyResponses {
			// Responses file references live on input items themselves and
			// inside message content or tool-result output parts.
			for index, item := range generation.Member(root, "input").Elements() {
				at := oif.Pointer("/input", strconv.Itoa(index))
				kind := generation.Text(generation.Member(item, "type"))
				openaiFilePart(item, at, kind)
				field := "content"
				switch kind {
				case "", "message":
				case "function_call_output", "custom_tool_call_output", "computer_call_output":
					field = "output"
				default:
					continue
				}
				container := generation.Member(item, field)
				elements := container.Elements()
				if container.Kind() == oif.Object {
					elements = []oif.Value{container}
				}
				for i, part := range elements {
					partAt := at + "/" + field
					if container.Kind() == oif.Array {
						partAt = oif.Pointer(partAt, strconv.Itoa(i))
					}
					openaiFilePart(part, partAt, generation.Text(generation.Member(part, "type")))
				}
			}
			return refs
		}
		for _, field := range []string{"messages", "input", "contents"} {
			for index, message := range generation.Member(root, field).Elements() {
				path := "/" + field + "/" + strconv.Itoa(index)
				partName := "content"
				if field == "contents" {
					partName = "parts"
				}
				parts(generation.Member(message, partName), path+"/"+partName)
			}
		}
		return refs
	}
}

// inputFields is the dialect's inspectable request-field set. Unknown native
// controls fail closed when a policy must inspect the input.
func inputFields(wire openai.Family) []string {
	common := "model stream tools tool_choice temperature top_p top_k max_tokens metadata"
	switch wire {
	case openai.FamilyChat:
		return strings.Fields(common + " messages max_completion_tokens stream_options stop n presence_penalty frequency_penalty logit_bias seed response_format parallel_tool_calls logprobs top_logprobs user service_tier store")
	case openai.FamilyResponses:
		return strings.Fields(common + " input instructions max_output_tokens text parallel_tool_calls truncation service_tier store previous_response_id conversation background include")
	case openai.FamilyAnthropic:
		return strings.Fields(common + " messages system stop_sequences anthropic_version")
	case openai.FamilyGemini:
		return strings.Fields("contents systemInstruction generationConfig tools toolConfig safetySettings cachedContent")
	case openai.FamilyBedrock:
		return strings.Fields("messages system inferenceConfig toolConfig guardrailConfig additionalModelResponseFieldPaths requestMetadata performanceConfig")
	}
	return nil
}

func wireInputCoverage(wire openai.Family) func(effective oif.Document) error {
	return func(document oif.Document) error {
		for _, field := range document.Root().Members() {
			if !slices.Contains(inputFields(wire), field.Name) {
				return generation.Incompatible("policy_conflict", generation.SafeField(field.Name), "input_policy_coverage", "The input policy cannot inspect an unknown or opaque native control.")
			}
		}
		for name, allowed := range map[string]string{
			"generationConfig": "temperature topP topK candidateCount maxOutputTokens stopSequences presencePenalty frequencyPenalty seed responseMimeType responseSchema responseJsonSchema responseLogprobs logprobs",
			"inferenceConfig":  "maxTokens temperature topP stopSequences",
			"stream_options":   "include_usage include_obfuscation",
		} {
			if value, present := document.Root().Lookup(name); present && value.Kind() != oif.Null && !generation.OnlyMembers(value, allowed) {
				return generation.Incompatible("policy_conflict", "/"+name, "input_policy_coverage", "A native control contains a member without a policy inspection contract.")
			}
		}
		if format, present := document.Root().Lookup("response_format"); present && format.Kind() != oif.Null && !inspectableFormat(format, false) {
			return generation.Incompatible("policy_conflict", "/response_format", "input_policy_coverage", "The response format has no complete policy inspection contract.")
		}
		if text, present := document.Root().Lookup("text"); present && text.Kind() != oif.Null {
			if !generation.OnlyMembers(text, "format verbosity") {
				return generation.Incompatible("policy_conflict", "/text", "input_policy_coverage", "The text format contains an unknown native control.")
			}
			if format, present := text.Lookup("format"); present && !inspectableFormat(format, true) {
				return generation.Incompatible("policy_conflict", "/text/format", "input_policy_coverage", "The text schema has no complete policy inspection contract.")
			}
		}
		// Tool schemas and argument/result JSON are declared data, not opaque
		// native state. Outside those positions, unknown message/part members
		// fail closed.
		for _, name := range []string{"messages", "contents", "input", "system", "systemInstruction"} {
			if value, present := document.Root().Lookup(name); present {
				if err := inspectableContent(value, name == "input" && wire == openai.FamilyResponses); err != nil {
					return err
				}
			}
		}
		for _, name := range []string{"tools", "toolConfig"} {
			if value, present := document.Root().Lookup(name); present && !inspectableTools(wire, name, value) {
				return generation.Incompatible("policy_conflict", "/"+name, "input_policy_coverage", "The input policy has no inspection contract for this native tool definition.")
			}
		}
		return nil
	}
}

func inspectableContent(value oif.Value, responseInput bool) error {
	switch value.Kind() {
	case oif.Absent, oif.Null, oif.String:
		return nil
	case oif.Array:
		for _, element := range value.Elements() {
			if err := inspectableContent(element, responseInput); err != nil {
				return err
			}
		}
	case oif.Object:
		for _, field := range value.Members() {
			switch field.Name {
			case "role", "type", "name", "id", "tool_call_id", "tool_use_id", "is_error":
			case "text", "refusal", "content", "parts":
				if err := inspectableContent(field.Value, responseInput); err != nil {
					return err
				}
			case "tool_calls", "function_call", "functionCall", "functionResponse", "toolUse", "toolResult", "input", "output":
				if hasOpaqueNative(field.Value) {
					return generation.Incompatible("policy_conflict", "/messages", "input_policy_coverage", "The input includes opaque native tool or reasoning state.")
				}
			case "status", "action", "sources", "results", "annotations":
				// Replayed hosted-tool observations are declared data; their
				// queries, URLs and titles are all inspectable strings.
				if hasOpaqueNative(field.Value) {
					return generation.Incompatible("policy_conflict", "/messages", "input_policy_coverage", "The input includes opaque native tool or reasoning state.")
				}
			default:
				return generation.Incompatible("policy_conflict", "/messages", "input_policy_coverage", "The input policy has no inspection contract for a native message member.")
			}
		}
		kind := generation.Text(generation.Member(value, "type"))
		if kind != "" && !slices.Contains([]string{"text", "input_text", "output_text", "message", "tool_use", "tool_result", "function_call", "function_call_output", "refusal", "web_search_call"}, kind) {
			return generation.Incompatible("policy_conflict", "/messages", "input_policy_coverage", "The input policy cannot inspect this native content type.")
		}
	default:
		return generation.Incompatible("policy_conflict", "/messages", "input_policy_coverage", "The input policy requires inspectable native content.")
	}
	return nil
}
func hasOpaqueNative(value oif.Value) bool {
	for _, field := range value.Members() {
		if slices.Contains([]string{"signature", "thoughtSignature", "thought_signature", "encrypted_content", "redacted_thinking", "inlineData", "inline_data", "fileData", "image_url", "input_audio", "document", "bytes"}, field.Name) {
			return true
		}
		if hasOpaqueNative(field.Value) {
			return true
		}
	}
	for _, element := range value.Elements() {
		if hasOpaqueNative(element) {
			return true
		}
	}
	return false
}

func inspectableTools(wire openai.Family, name string, value oif.Value) bool {
	if value.Kind() == oif.Null {
		return true
	}
	if name == "toolConfig" {
		switch wire {
		case openai.FamilyGemini:
			return generation.OnlyMembers(value, "functionCallingConfig") && generation.OnlyMembers(generation.Member(value, "functionCallingConfig"), "mode allowedFunctionNames streamFunctionCallArguments")
		case openai.FamilyBedrock:
			if !generation.OnlyMembers(value, "tools toolChoice") {
				return false
			}
			value = generation.Member(value, "tools")
		default:
			return false
		}
	}
	if value.Kind() != oif.Array {
		return false
	}
	for _, tool := range value.Elements() {
		switch wire {
		case openai.FamilyChat:
			if !generation.OnlyMembers(tool, "type function") || generation.Text(generation.Member(tool, "type")) != "function" || !generation.OnlyMembers(generation.Member(tool, "function"), "name description parameters strict") {
				return false
			}
		case openai.FamilyResponses:
			kind := generation.Text(generation.Member(tool, "type"))
			if hostedRequestTools[kind] == "web_search" {
				// Hosted tool declarations are inspectable control data; deep
				// validation happens at admission against the hosted contract.
				if !generation.OnlyMembers(tool, "type filters search_context_size user_location") {
					return false
				}
				continue
			}
			if !generation.OnlyMembers(tool, "type name description parameters strict") || kind != "function" {
				return false
			}
		case openai.FamilyAnthropic:
			if !generation.OnlyMembers(tool, "name description input_schema cache_control") || generation.Member(tool, "name").Kind() != oif.String {
				return false
			}
		case openai.FamilyGemini:
			if !generation.OnlyMembers(tool, "functionDeclarations") {
				return false
			}
			declarations := generation.Member(tool, "functionDeclarations")
			if declarations.Kind() != oif.Array {
				return false
			}
			for _, function := range declarations.Elements() {
				if !generation.OnlyMembers(function, "name description parameters parametersJsonSchema") {
					return false
				}
			}
		case openai.FamilyBedrock:
			if !generation.OnlyMembers(tool, "toolSpec") || !generation.OnlyMembers(generation.Member(tool, "toolSpec"), "name description inputSchema") {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func inspectableFormat(value oif.Value, responses bool) bool {
	if value.Kind() != oif.Object {
		return false
	}
	switch generation.Text(generation.Member(value, "type")) {
	case "text", "json_object":
		return generation.OnlyMembers(value, "type")
	case "json_schema":
		if responses {
			return generation.OnlyMembers(value, "type name description schema strict") && generation.Member(value, "schema").Kind() == oif.Object
		}
		return generation.OnlyMembers(value, "type json_schema") && generation.OnlyMembers(generation.Member(value, "json_schema"), "name description schema strict") && generation.Member(generation.Member(value, "json_schema"), "schema").Kind() == oif.Object
	}
	return false
}

// requestCoverage is the output-policy admission contract on the request: the
// declared positions whose output semantics a policy cannot inspect must not
// enter a covered interaction. hosted carries the provider-hosted tool
// families the plan admitted; a tool list of admitted hosted search alone is
// inspectable declared data.
func requestCoverage(effective oif.Document, hosted []string) error {
	for _, path := range []string{"/tools", "/toolConfig", "/reasoning", "/reasoning_effort", "/thinking", "/generationConfig/thinkingConfig"} {
		value, present := effective.Lookup(path)
		if !present || generation.EmptyOptional(value) {
			continue
		}
		if path == "/tools" && toolsAllHostedSearch(value, hosted) {
			continue
		}
		return generation.Incompatible("policy_conflict", path, "output_policy_coverage", "The requested native output or tool state has no complete inspection contract for this output policy.")
	}
	for _, path := range []string{"/logprobs", "/top_logprobs", "/generationConfig/responseLogprobs", "/generationConfig/logprobs"} {
		if value, present := effective.Lookup(path); present && value.Kind() != oif.Null && value.Raw() != "false" && value.Raw() != "0" {
			return generation.Incompatible("policy_conflict", path, "output_policy_coverage", "Native token alternatives are outside this output policy's inspection contract.")
		}
	}
	return nil
}

// toolsAllHostedSearch reports whether every declared tool is admitted hosted
// web search — the only tool-declared output the output policy can inspect in
// this slice. Client tool calls remain uninspectable actionable bytes.
func toolsAllHostedSearch(tools oif.Value, hosted []string) bool {
	if tools.Kind() != oif.Array || len(tools.Elements()) == 0 || !slices.Contains(hosted, "web_search") {
		return false
	}
	for _, tool := range tools.Elements() {
		if hostedRequestTools[generation.Text(generation.Member(tool, "type"))] != "web_search" {
			return false
		}
	}
	return true
}

// wireResultCoverage is the output-policy inspection contract over the
// dialect's result document. Opaque or nontext result members fail closed.
// hosted carries the provider-hosted tool families the plan admitted; admitted
// hosted observations are inspectable declared data.
func wireResultCoverage(wire openai.Family) func(result oif.Document, hosted []string) error {
	fail := func() error {
		return generation.Incompatible("policy_conflict", "/result", "output_policy_coverage", "The output policy cannot inspect native opaque or nontext result content.")
	}
	return func(document oif.Document, hosted []string) error {
		root := document.Root()
		var contents []oif.Value
		switch wire {
		case openai.FamilyChat:
			if !generation.OnlyMembers(root, "id object created model choices usage system_fingerprint service_tier") {
				return fail()
			}
			for _, choice := range generation.Member(root, "choices").Elements() {
				if !generation.OnlyMembers(choice, "index message finish_reason logprobs") || !generation.EmptyOptional(generation.Member(choice, "logprobs")) {
					return fail()
				}
				message := generation.Member(choice, "message")
				if !generation.OnlyMembers(message, "role content refusal annotations") || !generation.EmptyOptional(generation.Member(message, "annotations")) {
					return fail()
				}
				contents = append(contents, generation.Member(message, "content"), generation.Member(message, "refusal"))
			}
		case openai.FamilyResponses:
			if !generation.OnlyMembers(root, "id object created_at status background error incomplete_details instructions max_output_tokens max_tool_calls model output parallel_tool_calls previous_response_id prompt_cache_key reasoning safety_identifier service_tier store temperature text tool_choice tools top_logprobs top_p truncation usage user metadata conversation") {
				return fail()
			}
			for _, item := range generation.Member(root, "output").Elements() {
				switch generation.Text(generation.Member(item, "type")) {
				case "web_search_call":
					// Admitted hosted search output is inspectable declared data.
					if !slices.Contains(hosted, "web_search") || !generation.OnlyMembers(item, "id type status action") {
						return fail()
					}
					continue
				}
				if !generation.OnlyMembers(item, "id type status role content") || generation.Text(generation.Member(item, "type")) != "message" {
					return fail()
				}
				contents = append(contents, generation.Member(item, "content"))
			}
		case openai.FamilyAnthropic:
			if !generation.OnlyMembers(root, "id type role model content stop_reason stop_sequence usage") {
				return fail()
			}
			contents = append(contents, generation.Member(root, "content"))
		case openai.FamilyGemini:
			if !generation.OnlyMembers(root, "candidates usageMetadata modelVersion responseId promptFeedback") {
				return fail()
			}
			if feedback, present := root.Lookup("promptFeedback"); present && !generation.OnlyMembers(feedback, "blockReason safetyRatings") {
				return fail()
			}
			for _, candidate := range generation.Member(root, "candidates").Elements() {
				if !generation.OnlyMembers(candidate, "index content finishReason safetyRatings avgLogprobs tokenCount") {
					return fail()
				}
				content := generation.Member(candidate, "content")
				if !generation.OnlyMembers(content, "role parts") {
					return fail()
				}
				for _, part := range generation.Member(content, "parts").Elements() {
					if !generation.OnlyMembers(part, "text") {
						return fail()
					}
					contents = append(contents, generation.Member(part, "text"))
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
				if !generation.OnlyMembers(part, "type text refusal annotations logprobs") || !generation.EmptyOptional(generation.Member(part, "logprobs")) || !slices.Contains([]string{"text", "output_text", "refusal"}, generation.Text(generation.Member(part, "type"))) {
					return fail()
				}
				annotations := generation.Member(part, "annotations")
				if annotations.Kind() == oif.Absent || annotations.Kind() == oif.Null || annotations.Kind() == oif.Array && len(annotations.Elements()) == 0 {
					continue
				}
				if annotations.Kind() != oif.Array || !slices.Contains(hosted, "web_search") {
					return fail()
				}
				for _, annotation := range annotations.Elements() {
					if !inspectableURLCitation(annotation) {
						return fail()
					}
				}
			}
		}
		return nil
	}
}

// inspectableURLCitation bounds a web citation to its observable members; every
// value is plain text the output policy can read.
func inspectableURLCitation(annotation oif.Value) bool {
	return generation.OnlyMembers(annotation, "type url title start_index end_index") &&
		generation.Text(generation.Member(annotation, "type")) == "url_citation" &&
		generation.Member(annotation, "url").Kind() == oif.String &&
		generation.Member(annotation, "title").Kind() == oif.String &&
		generation.NonnegativeInteger(generation.Member(annotation, "start_index")) &&
		generation.NonnegativeInteger(generation.Member(annotation, "end_index"))
}

// wireEstimate is the dialect's conservative reservation shape: prompt tokens,
// the largest allowed reply, and the candidates asked for.
func wireEstimate(family openai.Family) func(effective oif.Document) generation.Estimate {
	field := func(doc oif.Document, name string) []byte {
		return generation.Field(doc, name)
	}
	return func(effective oif.Document) generation.Estimate {
		out := generation.Estimate{Candidates: 1}
		switch family {
		case openai.FamilyChat:
			out.Input = generation.EstimateItems(field(effective, "messages"))
			out.Input = generation.AddBounded(out.Input, generation.EstimateTools(field(effective, "tools")))
		case openai.FamilyResponses:
			out.Input = generation.AddBounded(generation.EstimateItems(field(effective, "input")), generation.EstimateText(field(effective, "instructions")))
			out.Input = generation.AddBounded(out.Input, generation.EstimateTools(field(effective, "tools")))
		default:
			out.Input = generation.EstimateNative(field(effective, "messages"))
			out.Input = generation.AddBounded(out.Input, generation.EstimateNative(field(effective, "system")))
			out.Input = generation.AddBounded(out.Input, generation.EstimateNative(field(effective, "contents")))
			out.Input = generation.AddBounded(out.Input, generation.EstimateNative(field(effective, "systemInstruction")))
			out.Input = generation.AddBounded(out.Input, generation.EstimateNative(field(effective, "generateContentRequest")))
			out.Input = generation.AddBounded(out.Input, generation.EstimateSchema(field(effective, "tools")))
		}
		if family == openai.FamilyBedrock {
			out.Input = generation.AddBounded(out.Input, generation.EstimateSchema(field(effective, "toolConfig")))
		}
		outputFields := []string{"max_completion_tokens", "max_tokens"}
		if family == openai.FamilyResponses {
			outputFields = []string{"max_output_tokens"}
		}
		for _, name := range outputFields {
			if value, ok := generation.Integer(field(effective, name)); ok {
				out.Output = &value
				break
			}
		}
		if family == openai.FamilyBedrock {
			if value, ok := generation.Integer(generation.JSONObject(field(effective, "inferenceConfig"))["maxTokens"]); ok {
				out.Output = &value
			}
		}
		if family == openai.FamilyGemini || family == openai.FamilyGeminiStream {
			config := generation.JSONObject(field(effective, "generationConfig"))
			if v, ok := generation.Integer(config["maxOutputTokens"]); ok {
				out.Output = &v
			}
			if v, ok := generation.Integer(config["candidateCount"]); ok {
				out.Candidates = v
			}
		}
		if family == openai.FamilyChat {
			if value, ok := generation.Integer(field(effective, "n")); ok {
				out.Candidates = value
			}
		}
		return out
	}
}
