package connectors

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// DefaultSet is owned by one operation/dialect contract. NativeOptions remains
// payload data; none of its members can change addressing, credentials or state
// authority. Null inside Values is a native value, never a management reset.
type DefaultSet struct {
	Dialect       string                     `json:"dialect"`
	Values        map[string]json.RawMessage `json:"values,omitempty"`
	NativeOptions map[string]json.RawMessage `json:"native_options,omitempty"`
}

// Binding identifies a configured serving environment separately from secrets.
// PrincipalID/Snapshot are operator declarations until independently observed;
// omitted values remain unknown. Credential secret versions are not identities.
type Binding struct {
	Model         string                `json:"model,omitempty"`
	Deployment    string                `json:"deployment,omitempty"`
	PrincipalID   string                `json:"principal_id,omitempty"`
	Snapshot      string                `json:"snapshot,omitempty"`
	Region        string                `json:"region,omitempty"`
	ResourceScope string                `json:"resource_scope,omitempty"`
	Defaults      map[string]DefaultSet `json:"defaults,omitempty"`
}

// DefaultProvenance identifies a configured omission filler. Actual request
// members, including null/zero/false/empty, always win atomically over defaults.
type DefaultProvenance struct {
	Pointer string `json:"pointer"`
	Source  string `json:"source"`
	Dialect string `json:"dialect"`
}

func defaultFields(dialect, operation string) []string {
	switch operation {
	case "generation":
		switch dialect {
		case "openai-chat":
			return strings.Fields("temperature top_p max_tokens max_completion_tokens frequency_penalty presence_penalty logit_bias seed stop response_format tools tool_choice parallel_tool_calls stream_options n reasoning_effort service_tier verbosity logprobs top_logprobs")
		case "openai-responses":
			return strings.Fields("temperature top_p max_output_tokens reasoning text tools tool_choice parallel_tool_calls truncation service_tier")
		case "anthropic-messages":
			return strings.Fields("max_tokens temperature top_p top_k stop_sequences thinking output_config tools tool_choice service_tier")
		case "gemini-generate-content":
			return strings.Fields("generationConfig tools toolConfig safetySettings")
		case "bedrock-converse":
			return strings.Fields("inferenceConfig additionalModelRequestFields toolConfig additionalModelResponseFieldPaths")
		}
	case "embeddings":
		return strings.Fields("dimensions encoding_format output_dimension output_dtype input_type truncate truncation task_type taskType title autoTruncate outputDimensionality parameters normalize embeddingTypes")
	case "rerank":
		return strings.Fields("top_n return_documents max_chunks_per_doc truncation")
	case "image_generation", "image_edit", "image_variation":
		return strings.Fields("size quality n response_format style output_format output_compression background moderation")
	case "speech":
		return strings.Fields("voice response_format speed instructions stream_format")
	case "transcription":
		return strings.Fields("language response_format temperature timestamp_granularities chunking_strategy")
	case "video_create":
		return strings.Fields("seconds size")
	}
	return nil
}

func reservedOption(name string) bool {
	if name == "" || len(name) > 128 || strings.HasPrefix(name, "/") || strings.ContainsAny(name, "\x00\r\n") {
		return true
	}
	switch strings.ToLower(name) {
	case "model", "stream", "messages", "input", "contents", "system", "systeminstruction", "generatecontentrequest", "routing", "provider", "route", "headers", "query", "url", "base_url", "endpoint", "authorization", "api_key", "credential", "credentials", "previous_response_id", "conversation", "background", "store", "file", "file_id", "input_file_id", "image", "mask", "anthropic_version", "cachedcontent":
		return true
	}
	return false
}

func validateDefaultSet(p Profile, operation string, defaults DefaultSet) error {
	if !slices.Contains(p.Operations, operation) || defaults.Dialect != p.Dialect {
		return errors.New("defaults must name an operation and dialect owned by the selected profile")
	}
	if len(defaults.Values)+len(defaults.NativeOptions) > 64 {
		return errors.New("use at most 64 operation defaults")
	}
	for name, raw := range defaults.Values {
		if !slices.Contains(defaultFields(defaults.Dialect, operation), name) {
			return fmt.Errorf("control %s is outside the operation/dialect schema; explicit extensions belong in native_options", name)
		}
		if _, collision := defaults.NativeOptions[name]; collision {
			return fmt.Errorf("control %s collides with native_options", name)
		}
		if err := validateDefaultValue(name, raw); err != nil {
			return err
		}
	}
	for name, raw := range defaults.NativeOptions {
		if err := validateDefaultValue(name, raw); err != nil {
			return err
		}
	}
	for _, pair := range [][2]string{{"max_tokens", "max_completion_tokens"}, {"dimensions", "output_dimension"}} {
		if _, one := defaults.Values[pair[0]]; one {
			if _, other := defaults.Values[pair[1]]; other {
				return fmt.Errorf("defaults contain conflicting controls %s and %s", pair[0], pair[1])
			}
		}
	}
	return nil
}

func validateDefaultValue(name string, raw json.RawMessage) error {
	if reservedOption(name) || len(raw) == 0 || len(raw) > 256<<10 || !json.Valid(raw) {
		return fmt.Errorf("default %s is reserved, malformed or exceeds 256 KiB", name)
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	switch name {
	case "temperature", "top_p", "frequency_penalty", "presence_penalty", "speed":
		var number json.Number
		if json.Unmarshal(raw, &number) != nil {
			return fmt.Errorf("default %s must be a number or native null", name)
		}
	case "max_tokens", "max_completion_tokens", "max_output_tokens", "dimensions", "output_dimension", "n", "top_n", "top_k":
		var number int64
		if json.Unmarshal(raw, &number) != nil || number < 1 {
			return fmt.Errorf("default %s must be a positive integer or native null", name)
		}
	case "parallel_tool_calls", "logprobs", "return_documents", "autoTruncate", "normalize":
		var boolean bool
		if json.Unmarshal(raw, &boolean) != nil {
			return fmt.Errorf("default %s must be a boolean or native null", name)
		}
	case "tools", "safetySettings", "stop_sequences", "embeddingTypes", "additionalModelResponseFieldPaths":
		var array []json.RawMessage
		if json.Unmarshal(raw, &array) != nil || array == nil {
			return fmt.Errorf("default %s must be an atomic array", name)
		}
	case "response_format", "reasoning", "text", "thinking", "output_config", "generationConfig", "toolConfig", "inferenceConfig", "additionalModelRequestFields", "parameters", "stream_options":
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			return fmt.Errorf("default %s must be an atomic object", name)
		}
	}
	return nil
}

func (c Config) validateDefaultsAndBindings(p Profile) error {
	for operation, defaults := range c.OperationDefaults {
		if err := validateDefaultSet(p, operation, defaults); err != nil {
			return err
		}
	}
	if len(c.Bindings) > 2000 {
		return errors.New("use at most 2000 serving bindings")
	}
	for name, binding := range c.Bindings {
		if !ModelValid(c.Kind, name) || binding.Model != "" && !ModelValid(c.Kind, binding.Model) || binding.Deployment != "" && !ModelValid(c.Kind, binding.Deployment) {
			return errors.New("serving binding model or deployment is invalid")
		}
		if binding.Model != "" && binding.Deployment != "" {
			return errors.New("a binding cannot provide both model and deployment")
		}
		if binding.Deployment != "" && c.Kind != "azure_openai" {
			return errors.New("deployment bindings belong to Azure hosting")
		}
		if binding.Region != "" && binding.Region != c.CloudRegion {
			return errors.New("binding region differs from the connection region")
		}
		for _, value := range []string{binding.PrincipalID, binding.Snapshot, binding.ResourceScope} {
			if len(value) > 512 || strings.ContainsAny(value, "\r\n\x00") {
				return errors.New("serving identity field is invalid")
			}
		}
		for operation, defaults := range binding.Defaults {
			if err := validateDefaultSet(p, operation, defaults); err != nil {
				return err
			}
		}
	}
	return nil
}

// DefaultsFor resolves provider -> binding inheritance without recursive merges.
// A whole member (including an array/schema/null) from the binding replaces the
// provider member. Request member presence is applied by the operation lowerer.
func (c Config) DefaultsFor(operation, model string) (map[string]json.RawMessage, []DefaultProvenance, error) {
	p, err := c.Profile()
	if err != nil {
		return nil, nil, err
	}
	if err := c.validateDefaultsAndBindings(p); err != nil {
		return nil, nil, err
	}
	out := map[string]json.RawMessage{}
	origins := map[string]string{}
	var scopes = []struct {
		source string
		value  DefaultSet
	}{{"provider_default", c.OperationDefaults[operation]}, {"binding_default", c.Bindings[model].Defaults[operation]}}
	for _, scope := range scopes {
		for name, value := range scope.value.Values {
			out[name], origins[name] = bytes.Clone(value), scope.source
		}
		for name, value := range scope.value.NativeOptions {
			// Across scopes, a native option cannot shadow a shared control.
			for _, other := range scopes {
				if _, collision := other.value.Values[name]; collision {
					return nil, nil, fmt.Errorf("native option %s collides with a configured control", name)
				}
			}
			out[name], origins[name] = bytes.Clone(value), scope.source+"_native_option"
		}
	}
	names := slices.Sorted(maps.Keys(out))
	provenance := make([]DefaultProvenance, 0, len(names))
	for _, name := range names {
		pointer := "/" + strings.ReplaceAll(strings.ReplaceAll(name, "~", "~0"), "/", "~1")
		provenance = append(provenance, DefaultProvenance{Pointer: pointer, Source: origins[name], Dialect: p.Dialect})
	}
	return out, provenance, nil
}
