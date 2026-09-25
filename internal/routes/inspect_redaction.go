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
	Structure            []inspectedTurn  `json:"structure"`
	OmittedTurns         int              `json:"omitted_turns"`
	RedactedNativeFields int              `json:"redacted_native_fields"`
}

// Structure is a bounded, content-free view of the *prepared* native request.
// Only fixed vocabulary and ordinal relationships leave the server. Tool IDs,
// names, schema keys, message text and opaque reasoning never leave it.
type inspectedTurn struct {
	Scope        string          `json:"scope"`
	Index        int             `json:"index"`
	Role         string          `json:"role"`
	Parts        []inspectedPart `json:"parts"`
	OmittedParts int             `json:"omitted_parts"`
}

type inspectedPart struct {
	Kind        string `json:"kind"`
	CallOrdinal *int   `json:"call_ordinal,omitempty"`
}

type inspectedField struct {
	Field     string  `json:"field"`
	Kind      string  `json:"kind"`
	Origin    string  `json:"origin"`
	Redacted  bool    `json:"redacted"`
	ValueJSON *string `json:"value_json,omitempty"`
	Empty     *bool   `json:"empty,omitempty"`
}

var inspectedRequestFields = strings.Fields("model messages input inputs instructions system systemInstruction contents content prompt prompt_name tools tool_choice toolConfig response_format text texts thinking reasoning generationConfig inferenceConfig additionalModelRequestFields safetySettings stop stop_sequences temperature top_p top_k top_n max_tokens max_completion_tokens max_output_tokens max_tokens_per_doc frequency_penalty presence_penalty priority seed n candidateCount stream stream_options parallel_tool_calls logprobs top_logprobs reasoning_effort service_tier verbosity truncation truncate truncation_direction store background previous_response_id conversation metadata user quality size style moderation output_format output_compression partial_images voice speed stream_format input_fidelity language timestamp_granularities include chunking_strategy known_speaker_names known_speaker_references seconds mask file image dimensions encoding_format output_dimension output_dtype input_type normalize embeddingTypes embedding_types outputDimensionality taskType title parameters inputText instances requests documents query return_documents raw_scores return_text images source_sentence sentences converse invokeModel generateContentRequest add_special_tokens")

// Result member vocabulary is the registered-schema names codecs read or
// project: usage categories, ranked/vector records, moderation outcomes.
// Names only — result member values are never emitted.
var inspectedResultFields = strings.Fields("model data results usage meta object id embedding embeddings values predictions statistics embeddingsByType inputTextTokenCount index relevance_score score text document flagged categories category_scores applied_input_types category_applied_input_types input_tokens output_tokens prompt_tokens total_tokens completion_tokens search_units billed_units cachedContentTokenCount tokenCount token_count tokens token_ids offset_mapping special_tokens warnings finish_reason content parts output output_text summary_text refusal image_url url modality function_call functionCall functionResponse tool_use arguments input_text thresholds label start stop special created citations")

func inspectRequest(document oif.Document, provenance []oif.Provenance) inspectedRequest {
	result := inspectedRequest{Fields: []inspectedField{}, Structure: []inspectedTurn{}}
	result.Structure, result.OmittedTurns = inspectStructure(document.Root())
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
			// Per-pointer provenance marks the members the construction
			// actually changed; untouched members stay caller-owned. The
			// root mapping marker is class evidence, not a per-field claim.
			if item.Pointer == field.Field {
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
	if slices.Contains(strings.Fields("/temperature /top_p /top_k /top_n /max_tokens /max_completion_tokens /max_output_tokens /max_tokens_per_doc /priority /dimensions /output_dimension /outputDimensionality /frequency_penalty /presence_penalty /seed /n /candidateCount /top_logprobs /speed /partial_images /output_compression /thinking/budget_tokens /generationConfig/temperature /generationConfig/topP /generationConfig/topK /generationConfig/maxOutputTokens /generationConfig/candidateCount /inferenceConfig/temperature /inferenceConfig/topP /inferenceConfig/maxTokens"), pointer) {
		return value.Kind() == oif.Number || value.Kind() == oif.Null
	}
	if slices.Contains(strings.Fields("/stream /parallel_tool_calls /logprobs /store /background /return_documents /normalize /raw_scores /return_text /add_special_tokens"), pointer) {
		return value.Kind() == oif.Boolean || value.Kind() == oif.Null
	}
	// Truncation controls are booleans in some dialects and string enums in
	// others; a boolean answer is always safe, a string falls to the enum.
	if pointer == "/truncation" || pointer == "/truncate" {
		if value.Kind() == oif.Boolean || value.Kind() == oif.Null {
			return true
		}
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
	case "/encoding_format":
		allowed = []string{"float", "base64"}
	case "/output_dtype":
		allowed = []string{"float", "int8", "uint8", "binary", "ubinary", "base64"}
	case "/input_type":
		allowed = []string{"query", "document", "search_document", "search_query", "classification", "clustering", "image"}
	case "/truncation_direction":
		allowed = []string{"left", "right", "Left", "Right"}
	case "/truncate":
		allowed = []string{"NONE", "START", "END"}
	case "/taskType":
		allowed = []string{"TASK_TYPE_UNSPECIFIED", "RETRIEVAL_QUERY", "RETRIEVAL_DOCUMENT", "SEMANTIC_SIMILARITY", "CLASSIFICATION", "CLUSTERING", "QUESTION_ANSWERING", "FACT_VERIFICATION", "CODE_RETRIEVAL_QUERY"}
	}
	return slices.Contains(allowed, text)
}

// Unknown native pointers are not safe output: JSON property names may contain
// prompt data. Collapse them to a category without emitting names or hashes.
func inspectionField(pointer string) string {
	if pointer == "" || pointer == "/" {
		return "/"
	}
	if pointer == "/$native" || pointer == "/native_fields" {
		return pointer
	}
	if rest, ok := strings.CutPrefix(pointer, "/result/"); ok {
		// Result-projection fields are declared by registered mappings, but
		// the same rule holds: only registered result member names pass.
		segments := strings.Split(rest, "/")
		if !slices.Contains(inspectedResultFields, segments[0]) {
			return "/native_fields"
		}
		for _, segment := range segments[1:] {
			ordinal := true
			for i := 0; i < len(segment); i++ {
				ordinal = ordinal && segment[i] >= '0' && segment[i] <= '9'
			}
			if !ordinal && !slices.Contains(inspectedResultFields, segment) {
				return "/result/" + segments[0]
			}
		}
		return pointer
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
