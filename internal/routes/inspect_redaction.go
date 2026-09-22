package routes

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
)

// Inspection is a deliberately lossy presentation of a prepared invocation.
// Keep native documents inside the planner: even property names in arbitrary
// schemas/tool arguments can contain private content.
type inspectedRequest struct {
	Fields               []inspectedField `json:"fields"`
	RedactedNativeFields int              `json:"redacted_native_fields"`
}

type inspectedField struct {
	Field     string  `json:"field"`
	Kind      string  `json:"kind"`
	Origin    string  `json:"origin"`
	Redacted  bool    `json:"redacted"`
	ValueJSON *string `json:"value_json,omitempty"`
	Empty     *bool   `json:"empty,omitempty"`
}

var inspectedRequestFields = strings.Fields("model messages input instructions system systemInstruction contents prompt tools tool_choice toolConfig response_format text thinking reasoning generationConfig inferenceConfig additionalModelRequestFields safetySettings stop stop_sequences temperature top_p top_k max_tokens max_completion_tokens max_output_tokens frequency_penalty presence_penalty seed n candidateCount stream stream_options parallel_tool_calls logprobs top_logprobs reasoning_effort service_tier verbosity truncation store background previous_response_id conversation metadata user")

func inspectRequest(document oif.Document, provenance []oif.Provenance) inspectedRequest {
	result := inspectedRequest{Fields: []inspectedField{}}
	for _, member := range document.Root().Members() {
		if !slices.Contains(inspectedRequestFields, member.Name) {
			result.RedactedNativeFields++
			continue
		}
		field := inspectedField{
			Field: "/" + member.Name, Kind: inspectionKind(member.Value.Kind()),
			Origin: "caller", Redacted: true,
		}
		if member.Value.Kind() == oif.Array {
			empty := len(member.Value.Elements()) == 0
			field.Empty = &empty
		} else if member.Value.Kind() == oif.Object {
			empty := len(member.Value.Members()) == 0
			field.Empty = &empty
		} else if member.Value.Kind() == oif.String {
			empty := member.Value.Raw() == `""`
			field.Empty = &empty
		}
		for _, item := range provenance {
			if item.Pointer == field.Field || (item.Pointer == "" || item.Pointer == "/") && (item.Origin == oif.QualifiedMapping || item.Origin == oif.LegacyMapping || item.Origin == oif.ExplicitTransform) {
				field.Origin = inspectionOrigin(item.Origin)
			}
		}
		if safeInspectionValue(field.Field, member.Value) {
			value := strings.Clone(member.Value.Raw())
			field.ValueJSON = &value
			field.Redacted = false
		}
		result.Fields = append(result.Fields, field)
		// Only selected native control containers expose their known scalar
		// settings. Content, schemas, tools and opaque state never recurse.
		if member.Value.Kind() == oif.Object && slices.Contains([]string{"thinking", "reasoning", "generationConfig", "inferenceConfig"}, member.Name) {
			for _, setting := range member.Value.Members() {
				pointer := field.Field + "/" + setting.Name
				if !knownInspectionSetting(pointer) {
					result.RedactedNativeFields++
					continue
				}
				child := inspectedField{Field: pointer, Kind: inspectionKind(setting.Value.Kind()), Origin: field.Origin, Redacted: true}
				if safeInspectionValue(pointer, setting.Value) {
					value := strings.Clone(setting.Value.Raw())
					child.ValueJSON, child.Redacted = &value, false
				}
				result.Fields = append(result.Fields, child)
			}
		}
	}
	slices.SortFunc(result.Fields, func(a, b inspectedField) int { return strings.Compare(a.Field, b.Field) })
	return result
}

func inspectionKind(kind oif.Kind) string {
	switch kind {
	case oif.Null:
		return "null"
	case oif.Boolean:
		return "boolean"
	case oif.Number:
		return "number"
	case oif.String:
		return "string"
	case oif.Array:
		return "array"
	case oif.Object:
		return "object"
	default:
		return "absent"
	}
}

func inspectionOrigin(origin oif.Origin) string {
	switch origin {
	case oif.Caller, oif.ProviderDefault, oif.ModelDefault, oif.OperationDefault,
		oif.IdentityBinding, oif.ResourceBinding, oif.TransportOption,
		oif.QualifiedMapping, oif.ExplicitTransform, oif.LegacyMapping:
		return string(origin)
	default:
		return "configured"
	}
}

func knownInspectionSetting(pointer string) bool {
	return slices.Contains(strings.Fields("/thinking/type /thinking/budget_tokens /reasoning/effort /reasoning/summary /generationConfig/temperature /generationConfig/topP /generationConfig/topK /generationConfig/maxOutputTokens /generationConfig/candidateCount /inferenceConfig/temperature /inferenceConfig/topP /inferenceConfig/maxTokens"), pointer)
}

func safeInspectionValue(pointer string, value oif.Value) bool {
	if len(value.Raw()) > 256 {
		return false
	}
	if slices.Contains(strings.Fields("/temperature /top_p /top_k /max_tokens /max_completion_tokens /max_output_tokens /frequency_penalty /presence_penalty /seed /n /candidateCount /top_logprobs /thinking/budget_tokens /generationConfig/temperature /generationConfig/topP /generationConfig/topK /generationConfig/maxOutputTokens /generationConfig/candidateCount /inferenceConfig/temperature /inferenceConfig/topP /inferenceConfig/maxTokens"), pointer) {
		return value.Kind() == oif.Number || value.Kind() == oif.Null
	}
	if slices.Contains(strings.Fields("/stream /parallel_tool_calls /logprobs /store /background"), pointer) {
		return value.Kind() == oif.Boolean || value.Kind() == oif.Null
	}
	if value.Kind() != oif.String {
		return false
	}
	var text string
	if json.Unmarshal([]byte(value.Raw()), &text) != nil {
		return false
	}
	var allowed []string
	switch pointer {
	case "/thinking/type":
		allowed = []string{"enabled", "disabled", "adaptive"}
	case "/reasoning/effort", "/reasoning_effort":
		allowed = []string{"none", "minimal", "low", "medium", "high", "xhigh"}
	case "/reasoning/summary":
		allowed = []string{"auto", "concise", "detailed"}
	case "/verbosity":
		allowed = []string{"low", "medium", "high"}
	case "/service_tier":
		allowed = []string{"auto", "default", "flex", "scale", "priority"}
	case "/truncation":
		allowed = []string{"auto", "disabled"}
	case "/tool_choice":
		allowed = []string{"auto", "none", "required", "any"}
	}
	return slices.Contains(allowed, text)
}

// Unknown native pointers are not safe output: JSON property names may contain
// prompt data. Collapse them to a category without emitting names or hashes.
func inspectionField(pointer string) string {
	if pointer == "" || pointer == "/" {
		return "/"
	}
	if slices.Contains([]string{"/profile_id", "/profile_revision", "/provider_profile", "/headers/Anthropic-Version", "/headers/Anthropic-Beta", "/headers/Openai-Beta", "/query/api-version", "/query/$xgafv"}, pointer) {
		return pointer
	}
	for _, field := range inspectedRequestFields {
		prefix := "/" + field
		if pointer == prefix || strings.HasPrefix(pointer, prefix+"/") {
			if knownInspectionSetting(pointer) {
				return pointer
			}
			return prefix
		}
	}
	return "/native_fields"
}
