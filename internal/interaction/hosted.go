package interaction

import (
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/openai"
)

// effectHostedToolCall declares that the provider may execute a hosted tool
// outside the caller's control. It is distinct from client_tool_call, which is
// client-fulfilled work, and from resource effects on retained state.
const effectHostedToolCall = "hosted_tool_call"

// hostedRequestTools maps an admitted Responses tool type to the hosted tool
// family that owns its lifecycle contract. Declaring any other type still
// refuses admission before dispatch.
var hostedRequestTools = map[string]string{
	"web_search":            "web_search",
	"web_search_2025_08_26": "web_search",
}

// hostedItemKinds maps provider-emitted output item types to their hosted tool
// family. An unadmitted hosted item in a result or event is a contract
// violation: the provider ran work the bound request never declared.
var hostedItemKinds = map[string]string{
	"web_search_call":       "web_search",
	"file_search_call":      "file_search",
	"computer_call":         "computer_use",
	"code_interpreter_call": "code_interpreter",
	"image_generation_call": "image_generation",
	"local_shell_call":      "local_shell",
	"shell_call":            "shell",
	"apply_patch_call":      "apply_patch",
	"tool_search_call":      "tool_search",
	"mcp_call":              "mcp",
	"mcp_list_tools":        "mcp",
	"mcp_approval_request":  "mcp",
}

// hostedToolChoices are hosted-tool selector types that may appear inside a
// native tool_choice object. Forcing a hosted tool is equivalent to invoking
// it, so each needs the matching admission.
var hostedToolChoices = map[string]string{
	"web_search":                    "web_search",
	"web_search_2025_08_26":         "web_search",
	"web_search_preview":            "web_search",
	"web_search_preview_2025_03_11": "web_search",
}

// unqualifiedToolChoices are hosted-tool selectors that have no admitted
// lifecycle in this slice; they refuse precisely.
var unqualifiedToolChoices = map[string]bool{
	"file_search": true, "computer": true, "computer_use_preview": true,
	"computer_use": true, "image_generation": true, "code_interpreter": true,
	"mcp": true, "shell": true, "tool_search": true, "local_shell": true,
	"apply_patch": true,
}

// qualifiedIncludes are non-hosted Responses include selectors already covered
// by the admitted surfaces. Hosted-family selectors require their own
// admission; unknown selectors fail closed.
var qualifiedIncludes = map[string]bool{
	"reasoning.encrypted_content":   true,
	"message.output_text.logprobs":  true,
	"message.input_image.image_url": true,
}

var hostedIncludes = map[string]string{
	"web_search_call.results":               "web_search",
	"web_search_call.action.sources":        "web_search",
	"file_search_call.results":              "file_search",
	"code_interpreter_call.outputs":         "code_interpreter",
	"computer_call_output.output.image_url": "computer_use",
}

// classifyRequestTools splits the effective tool declarations into
// client-fulfilled tools and provider-hosted tools. It preserves the existing
// refusal posture: anything outside the admitted shapes fails closed.
func classifyRequestTools(root oif.Value, wire openai.Family) (client bool, hosted []oif.Value, err error) {
	reject := func() (bool, []oif.Value, error) {
		return false, nil, incompatible("state_carrier", "/tools", "hosted_tool_effects", "Provider-hosted or unregistered tool effects require a separately qualified lifecycle contract.")
	}
	if tools, present := root.Lookup("tools"); present && tools.Kind() != oif.Null {
		if tools.Kind() != oif.Array {
			return reject()
		}
		for _, tool := range tools.Elements() {
			kind := valueText(member(tool, "type"))
			switch wire {
			case openai.FamilyChat:
				if kind != "function" {
					return reject()
				}
			case openai.FamilyResponses:
				if family, isHosted := hostedRequestTools[kind]; isHosted {
					if err := validateWebSearchTool(tool, family); err != nil {
						return false, nil, err
					}
					hosted = append(hosted, tool)
					continue
				}
				if kind != "function" {
					return reject()
				}
			case openai.FamilyAnthropic:
				if kind != "" && kind != "custom" || member(tool, "name").Kind() != oif.String || member(tool, "input_schema").Kind() != oif.Object {
					return reject()
				}
			case openai.FamilyGemini:
				if !onlyMembers(tool, "functionDeclarations") || member(tool, "functionDeclarations").Kind() != oif.Array {
					return reject()
				}
			default:
				return reject()
			}
			client = true
		}
	}
	if config, present := root.Lookup("toolConfig"); present && config.Kind() != oif.Null {
		if wire == openai.FamilyBedrock {
			if tools, present := config.Lookup("tools"); present {
				if tools.Kind() != oif.Array {
					return reject()
				}
				for _, tool := range tools.Elements() {
					if !onlyMembers(tool, "toolSpec") || member(tool, "toolSpec").Kind() != oif.Object {
						return reject()
					}
					client = true
				}
			}
		}
	}
	var history func(oif.Value) bool
	history = func(value oif.Value) bool {
		for _, field := range value.Members() {
			if slices.Contains([]string{"tool_calls", "function_call", "functionCall", "functionResponse", "toolUse", "toolResult"}, field.Name) {
				return true
			}
			if field.Name == "type" && slices.Contains([]string{"tool_use", "tool_result", "function_call", "function_call_output"}, valueText(field.Value)) {
				return true
			}
			if history(field.Value) {
				return true
			}
		}
		for _, element := range value.Elements() {
			if history(element) {
				return true
			}
		}
		return false
	}
	for _, name := range []string{"messages", "contents", "input"} {
		if history(member(root, name)) {
			client = true
		}
	}
	return client, hosted, nil
}

// validateWebSearchTool bounds a native OpenAI Responses web_search declaration
// to the controls whose semantics the qualified contract preserves. Unknown
// members or unsupported control values refuse before dispatch.
func validateWebSearchTool(tool oif.Value, family string) error {
	if family != "web_search" {
		return incompatible("state_carrier", "/tools", "hosted_tool_effects", "Provider-hosted or unregistered tool effects require a separately qualified lifecycle contract.")
	}
	if !onlyMembers(tool, "type filters search_context_size user_location") {
		return incompatible("target_capability", "/tools", "hosted_tool_contract", "The web search declaration contains a member outside the qualified lifecycle contract.")
	}
	if filters, present := tool.Lookup("filters"); present && filters.Kind() != oif.Null {
		if !onlyMembers(filters, "allowed_domains") {
			return incompatible("target_capability", "/tools/filters", "hosted_tool_contract", "The web search filters contain a member outside the qualified lifecycle contract.")
		}
		if domains := member(filters, "allowed_domains"); domains.Kind() != oif.Absent && domains.Kind() != oif.Null {
			if domains.Kind() != oif.Array {
				return incompatible("target_capability", "/tools/filters/allowed_domains", "hosted_tool_contract", "Allowed domains must be a domain list.")
			}
			for _, domain := range domains.Elements() {
				if domain.Kind() != oif.String {
					return incompatible("target_capability", "/tools/filters/allowed_domains", "hosted_tool_contract", "Allowed domains must be a domain list.")
				}
			}
		}
	}
	if size, present := tool.Lookup("search_context_size"); present && size.Kind() != oif.Null {
		if !slices.Contains([]string{"low", "medium", "high"}, valueText(size)) {
			return incompatible("target_capability", "/tools/search_context_size", "hosted_tool_contract", "The search context size is outside the qualified web search contract.")
		}
	}
	if location, present := tool.Lookup("user_location"); present && location.Kind() != oif.Null {
		if !onlyMembers(location, "type city country region timezone") {
			return incompatible("target_capability", "/tools/user_location", "hosted_tool_contract", "The user location contains a member outside the qualified web search contract.")
		}
		if kind := valueText(member(location, "type")); kind != "" && kind != "approximate" {
			return incompatible("target_capability", "/tools/user_location/type", "hosted_tool_contract", "The user location type is outside the qualified web search contract.")
		}
		for _, name := range []string{"city", "country", "region", "timezone"} {
			if value := member(location, name); value.Kind() != oif.Absent && value.Kind() != oif.Null && value.Kind() != oif.String {
				return incompatible("target_capability", "/tools/user_location/"+name, "hosted_tool_contract", "The user location value is outside the qualified web search contract.")
			}
		}
	}
	return nil
}

// admitHostedTools authorizes each declared hosted tool against the caller's
// permission and the profile's qualified hosted-tool families, then records the
// distinct effect on the obligations.
func (t *Template) admitHostedTools(tools []oif.Value, context Context, obligations *Obligations) ([]string, error) {
	admitted := map[string]bool{}
	for _, tool := range tools {
		family := hostedRequestTools[valueText(member(tool, "type"))]
		if !context.AllowHostedTools {
			return nil, incompatible("policy_conflict", "/tools", "hosted_tool_authorization", "The invocation declares provider-hosted tool effects but the caller does not permit them.")
		}
		if !slices.Contains(t.profile.HostedTools, family) {
			return nil, incompatible("state_carrier", "/tools", "hosted_tool_lifecycle", "This profile has no qualified lifecycle contract for the declared hosted tool.")
		}
		admitted[family] = true
	}
	if len(admitted) > 0 {
		obligations.Effects = append(obligations.Effects, effectHostedToolCall)
	}
	return slices.Sorted(maps.Keys(admitted)), nil
}

// checkHostedSelectors bounds the native controls that request additional
// hosted observations or force a hosted tool. Both are admitted only when the
// matching hosted family was declared and qualified.
func checkHostedSelectors(root oif.Value, wire openai.Family, admitted []string) error {
	if wire == openai.FamilyChat {
		// Chat web_search_options invokes the provider's hosted search outside
		// the declared tools contract; it has no qualified lifecycle here.
		if options, present := root.Lookup("web_search_options"); present && options.Kind() != oif.Null {
			return incompatible("state_carrier", "/web_search_options", "hosted_tool_effects", "Provider-hosted or unregistered tool effects require a separately qualified lifecycle contract.")
		}
	}
	if wire != openai.FamilyResponses {
		return nil
	}
	if include, present := root.Lookup("include"); present && include.Kind() != oif.Null {
		if include.Kind() != oif.Array {
			return incompatible("state_carrier", "/include", "hosted_tool_effects", "Provider-hosted or unregistered tool effects require a separately qualified lifecycle contract.")
		}
		for _, selector := range include.Elements() {
			name := valueText(selector)
			if family, hosted := hostedIncludes[name]; hosted {
				if !slices.Contains(admitted, family) {
					return incompatible("state_carrier", "/include", "hosted_tool_effects", "Provider-hosted or unregistered tool effects require a separately qualified lifecycle contract.")
				}
				continue
			}
			if !qualifiedIncludes[name] {
				return incompatible("target_capability", "/include", "qualified_include", "An include selector has no qualified observation contract.")
			}
		}
	}
	if choice, present := root.Lookup("tool_choice"); present && choice.Kind() == oif.Object {
		kind := valueText(member(choice, "type"))
		if family, hosted := hostedToolChoices[kind]; hosted {
			if !slices.Contains(admitted, family) {
				return incompatible("state_carrier", "/tool_choice", "hosted_tool_effects", "Provider-hosted or unregistered tool effects require a separately qualified lifecycle contract.")
			}
		} else if unqualifiedToolChoices[kind] {
			return incompatible("state_carrier", "/tool_choice", "hosted_tool_effects", "Provider-hosted or unregistered tool effects require a separately qualified lifecycle contract.")
		}
	}
	return nil
}

// validateHostedResult bounds provider-emitted output for the admitted hosted
// contract. Hosted tool items and citations are observations of work the
// provider already performed; each admitted family declares what those
// observations may contain. Undeclared hosted items violate the contract.
func validateHostedResult(document oif.Document, admitted []string) error {
	output := member(document.Root(), "output")
	if output.Kind() != oif.Array {
		return nil
	}
	for _, item := range output.Elements() {
		if err := checkHostedItem(item, admitted, "/result/output"); err != nil {
			return err
		}
	}
	return nil
}

// checkHostedItem applies the hosted-family rule to one output item. Admitted
// items are schema-bounded; undeclared hosted items are contract violations.
func checkHostedItem(item oif.Value, admitted []string, path string) error {
	if item.Kind() != oif.Object {
		return nil
	}
	kind := valueText(member(item, "type"))
	family, hosted := hostedItemKinds[kind]
	if hosted {
		if !slices.Contains(admitted, family) {
			return guardFailure(path, "unadmitted_hosted_effect")
		}
		if kind == "web_search_call" {
			return validateWebSearchItem(item, path)
		}
		return nil
	}
	if kind == "message" {
		return checkMessageAnnotations(item, admitted, path)
	}
	return nil
}

// validateWebSearchItem bounds a web_search_call observation to the qualified
// item contract: native identity, lifecycle status and a bounded action record.
func validateWebSearchItem(item oif.Value, path string) error {
	if !onlyMembers(item, "id type status action") {
		return guardFailure(path, "hosted_item_members")
	}
	if member(item, "id").Kind() != oif.String {
		return guardFailure(path+"/id", "hosted_item_identity")
	}
	if !slices.Contains([]string{"in_progress", "searching", "completed", "failed"}, valueText(member(item, "status"))) {
		return guardFailure(path+"/status", "hosted_item_lifecycle")
	}
	action := member(item, "action")
	if action.Kind() == oif.Absent {
		return nil
	}
	if action.Kind() != oif.Object {
		return guardFailure(path+"/action", "hosted_item_action")
	}
	return validateWebSearchAction(action, path+"/action")
}

// validateWebSearchAction bounds the provider's record of the action it took.
// sources are URL observations only; they never authorize follow-up fetches.
func validateWebSearchAction(action oif.Value, path string) error {
	if !onlyMembers(action, "type query queries sources url pattern") {
		return guardFailure(path, "hosted_action_members")
	}
	switch valueText(member(action, "type")) {
	case "search":
		for _, name := range []string{"query", "queries"} {
			// Older API revisions serialize the deprecated query member as a
			// scalar or a list; both remain observational strings.
			if value, present := action.Lookup(name); present && value.Kind() != oif.Null {
				if value.Kind() == oif.String {
					continue
				}
				if value.Kind() != oif.Array {
					return guardFailure(path+"/"+name, "hosted_action_queries")
				}
				for _, element := range value.Elements() {
					if element.Kind() != oif.String {
						return guardFailure(path+"/"+name, "hosted_action_queries")
					}
				}
			}
		}
		if sources := member(action, "sources"); sources.Kind() != oif.Absent && sources.Kind() != oif.Null {
			if sources.Kind() != oif.Array {
				return guardFailure(path+"/sources", "hosted_action_sources")
			}
			for _, source := range sources.Elements() {
				if !onlyMembers(source, "type url") || member(source, "url").Kind() != oif.String {
					return guardFailure(path+"/sources", "hosted_action_sources")
				}
			}
		}
	case "open_page":
		if url := member(action, "url"); url.Kind() != oif.Absent && url.Kind() != oif.Null && url.Kind() != oif.String {
			return guardFailure(path+"/url", "hosted_action_url")
		}
	case "find_in_page":
		if member(action, "url").Kind() != oif.String || member(action, "pattern").Kind() != oif.String {
			return guardFailure(path, "hosted_action_find")
		}
	default:
		return guardFailure(path+"/type", "hosted_action_variant")
	}
	return nil
}

// checkMessageAnnotations bounds message citations. A url_citation is a
// web_search observation; every other annotation family cites provider-side
// resources outside this contract.
func checkMessageAnnotations(item oif.Value, admitted []string, path string) error {
	content := member(item, "content")
	if content.Kind() != oif.Array {
		return nil
	}
	for _, part := range content.Elements() {
		annotations := member(part, "annotations")
		if annotations.Kind() != oif.Array {
			continue
		}
		for _, annotation := range annotations.Elements() {
			if err := checkHostedAnnotation(annotation, admitted, path+"/annotations"); err != nil {
				return err
			}
		}
	}
	return nil
}

func checkHostedAnnotation(annotation oif.Value, admitted []string, path string) error {
	if annotation.Kind() != oif.Object {
		return nil
	}
	if valueText(member(annotation, "type")) != "url_citation" {
		return guardFailure(path, "unqualified_annotation")
	}
	if !slices.Contains(admitted, "web_search") {
		return guardFailure(path, "unadmitted_hosted_effect")
	}
	if !onlyMembers(annotation, "type url title start_index end_index") || member(annotation, "url").Kind() != oif.String || member(annotation, "title").Kind() != oif.String || !nonnegativeInteger(member(annotation, "start_index")) || !nonnegativeInteger(member(annotation, "end_index")) {
		return guardFailure(path, "citation_contract")
	}
	return nil
}

// validateHostedEvent applies the hosted contract to one Responses stream
// event: hosted lifecycle events require admission and keep their declared
// member contract, and embedded output items follow the item rule.
func validateHostedEvent(root oif.Value, admitted []string) error {
	kind := valueText(member(root, "type"))
	switch kind {
	case "response.output_item.added", "response.output_item.done":
		return checkHostedItem(member(root, "item"), admitted, "/events/item")
	case "response.output_text.annotation.added":
		return checkHostedAnnotation(member(root, "annotation"), admitted, "/events/annotation")
	}
	if item, phase, ok := hostedEventItem(kind); ok {
		family := hostedItemKinds[item]
		if !slices.Contains(admitted, family) {
			return guardFailure("/events/type", "unadmitted_hosted_effect")
		}
		if item == "web_search_call" {
			if !slices.Contains([]string{"in_progress", "searching", "completed"}, phase) {
				return guardFailure("/events/type", "hosted_event_contract")
			}
			if !onlyMembers(root, "type item_id output_index sequence_number") || member(root, "item_id").Kind() != oif.String || !nonnegativeInteger(member(root, "output_index")) {
				return guardFailure("/events", "hosted_event_contract")
			}
		}
		return nil
	}
	if response := member(root, "response"); response.Kind() == oif.Object {
		if output := member(response, "output"); output.Kind() == oif.Array {
			for _, item := range output.Elements() {
				if err := checkHostedItem(item, admitted, "/events/response/output"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// hostedEventItem recognizes provider-hosted lifecycle event names of the form
// response.<item kind>.<phase>. They are never admitted without a declared
// hosted tool of the matching family.
func hostedEventItem(kind string) (item, phase string, ok bool) {
	rest, found := strings.CutPrefix(kind, "response.")
	if !found {
		return "", "", false
	}
	head, tail, found := strings.Cut(rest, ".")
	if !found {
		return "", "", false
	}
	if _, hosted := hostedItemKinds[head]; !hosted {
		return "", "", false
	}
	return head, tail, true
}
