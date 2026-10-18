package connectors

import (
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

var tgiCompletionFields = strings.Fields("model prompt max_tokens temperature top_p stream seed repetition_penalty frequency_penalty stop stream_options")

// ValidateCompletionRequest qualifies the narrower TGI endpoint after defaults
// and transport options are applied. Its per-prompt streaming usage is not an
// aggregate request counter, so only single-prompt streams are admitted.
func (c Config) ValidateCompletionRequest(document oif.Document) error {
	if c.ProfileID != "tgi-completions" {
		return nil
	}
	root := document.Root()
	refuse := func(field string) error {
		return &wire.RequestError{Code: "unsupported_parameter", Param: field, Message: "The TGI completion profile cannot preserve this request control."}
	}
	for _, field := range root.Members() {
		if !slices.Contains(tgiCompletionFields, field.Name) {
			return refuse(field.Name)
		}
	}
	for name, bits := range map[string]int{"max_tokens": 32, "seed": 64} {
		if value, ok := root.Lookup(name); ok && value.Kind() != oif.Null {
			if _, err := strconv.ParseUint(value.Raw(), 10, bits); err != nil {
				return refuse(name)
			}
		}
	}
	prompt, _ := root.Lookup("prompt")
	if prompt.Kind() != oif.String {
		if prompt.Kind() != oif.Array || len(prompt.Elements()) == 0 {
			return refuse("prompt")
		}
		for _, value := range prompt.Elements() {
			if value.Kind() != oif.String {
				return refuse("prompt")
			}
		}
		stream, _ := root.Lookup("stream")
		if stream.Raw() == "true" && len(prompt.Elements()) > 1 {
			return refuse("prompt")
		}
	}
	if options, ok := root.Lookup("stream_options"); ok && options.Kind() != oif.Null {
		if options.Kind() != oif.Object {
			return refuse("stream_options")
		}
		for _, option := range options.Members() {
			if option.Name != "include_usage" {
				return refuse("stream_options/" + option.Name)
			}
		}
	}
	if stop, ok := root.Lookup("stop"); ok && stop.Kind() != oif.Null {
		if stop.Kind() != oif.Array || len(stop.Elements()) > 4 {
			return refuse("stop")
		}
		for _, value := range stop.Elements() {
			if value.Kind() != oif.String {
				return refuse("stop")
			}
		}
	}
	return nil
}

// Each native text-completion endpoint has an explicit profile. Generic
// OpenAI-compatible hosting alone never qualifies the legacy dialect.
func registerCompletionProfiles() {
	for _, vendor := range []string{"openai", "vllm", "tgi", "fireworks", "together"} {
		kind, hosting := "openai_compatible", "direct-compatible"
		if vendor == "openai" {
			kind, hosting = "openai", "direct-openai"
		}
		p := Profile{ID: vendor + "-completions", Revision: ProfileRevision, Label: vendor + " text completions", Kind: kind, Dialect: "openai-completions", DialectRevision: "m9-baseline-1", Hosting: hosting, Authentication: []string{"api_key", "headers", "none"}, Transport: "http", Operations: []string{"generation"}, OperationDialects: map[string]string{"generation": "openai-completions"}, SemanticHeaders: []string{}, QuerySettings: []string{}, Documentation: "https://platform.openai.com/docs/api-reference/completions", Strict: true}
		completeProfileMetadata(&p)
		profileRegistry = append(profileRegistry, p)
	}
}
