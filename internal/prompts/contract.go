package prompts

import (
	"encoding/json"
	"errors"
	"iter"
	"maps"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const MaxTemplateBytes = 64 << 10

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
var ErrTemplate = errors.New("invalid prompt template or variables")
var ErrRenderTooLarge = errors.New("rendered prompt exceeds input limit")

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type VariableProperty struct {
	Type string `json:"type"`
}

type VariableSchema struct {
	Type                 string                      `json:"type"`
	Properties           map[string]VariableProperty `json:"properties"`
	Required             []string                    `json:"required"`
	AdditionalProperties bool                        `json:"additionalProperties"`
}

type Parameters struct {
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	MaxOutputTokens *int64   `json:"max_output_tokens,omitempty"`
}

type Definition struct {
	Template          string          `json:"template"`
	Variables         []string        `json:"variables"`
	Messages          []Message       `json:"messages,omitempty"`
	VariableSchema    *VariableSchema `json:"variable_schema,omitempty"`
	DefaultParameters *Parameters     `json:"default_parameters,omitempty"`
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func (d Definition) Clone() Definition {
	return Definition{
		Template:          d.Template,
		Variables:         slices.Clone(d.Variables),
		Messages:          slices.Clone(d.Messages),
		VariableSchema:    cloneSchema(d.VariableSchema),
		DefaultParameters: cloneParameters(d.DefaultParameters),
	}
}

func cloneSchema(schema *VariableSchema) *VariableSchema {
	if schema == nil {
		return nil
	}
	return &VariableSchema{Type: schema.Type, Properties: maps.Clone(schema.Properties), Required: slices.Clone(schema.Required), AdditionalProperties: schema.AdditionalProperties}
}

func cloneParameters(parameters *Parameters) *Parameters {
	if parameters == nil {
		return nil
	}
	return &Parameters{Temperature: clonePointer(parameters.Temperature), TopP: clonePointer(parameters.TopP), MaxOutputTokens: clonePointer(parameters.MaxOutputTokens)}
}

func (d Definition) variableNames() ([]string, error) {
	if d.VariableSchema == nil {
		return d.Variables, nil
	}
	schema := d.VariableSchema
	encoded, err := json.Marshal(schema)
	if err != nil || len(encoded) > MaxTemplateBytes || schema.Type != "object" || schema.AdditionalProperties || len(schema.Properties) > 256 || len(schema.Required) != len(schema.Properties) || len(d.Variables) != 0 {
		return nil, ErrTemplate
	}
	for i, name := range schema.Required {
		property, ok := schema.Properties[name]
		if !ok || property.Type != "string" || slices.Contains(schema.Required[:i], name) {
			return nil, ErrTemplate
		}
	}
	return schema.Required, nil
}

func validParameters(p *Parameters) bool {
	if p == nil {
		return true
	}
	return (p.Temperature == nil || !math.IsNaN(*p.Temperature) && *p.Temperature >= 0 && *p.Temperature <= 2) &&
		(p.TopP == nil || !math.IsNaN(*p.TopP) && *p.TopP >= 0 && *p.TopP <= 1) &&
		(p.MaxOutputTokens == nil || *p.MaxOutputTokens > 0 && *p.MaxOutputTokens <= 2147483647)
}

func (d Definition) promptMessages() []Message {
	if len(d.Messages) == 0 {
		return []Message{{Role: "user", Content: d.Template}}
	}
	return d.Messages
}

func validMessages(messages []Message, names []string, remaining int, first bool) bool {
	if len(messages) == 0 {
		return true
	}
	message := messages[0]
	if !slices.Contains([]string{"system", "user", "assistant"}, message.Role) || message.Role == "system" && !first || !utf8.ValidString(message.Content) || len(message.Content) > remaining {
		return false
	}
	parts := strings.Split(message.Content, "{{")
	if strings.Contains(parts[0], "}}") {
		return false
	}
	for _, part := range parts[1:] {
		name, rest, ok := strings.Cut(part, "}}")
		if !ok || !slices.Contains(names, name) || strings.Contains(rest, "}}") || !namePattern.MatchString(name) {
			return false
		}
	}
	return validMessages(messages[1:], names, remaining-len(message.Content), false)
}

func (d Definition) Validate() error {
	names, err := d.variableNames()
	if err != nil {
		return ErrTemplate
	}
	return d.validate(names)
}

func (d Definition) validate(names []string) error {
	if len(names) > 256 || !validParameters(d.DefaultParameters) {
		return ErrTemplate
	}
	for i, name := range names {
		if !namePattern.MatchString(name) || slices.Contains(names[:i], name) {
			return ErrTemplate
		}
	}
	if len(d.Messages) > 0 && (d.Template != "" || d.VariableSchema == nil || len(d.Messages) > 256) {
		return ErrTemplate
	}
	messages := d.promptMessages()
	if !validMessages(messages, names, MaxTemplateBytes, true) {
		return ErrTemplate
	}
	for _, name := range names {
		if !slices.ContainsFunc(messages, func(message Message) bool { return strings.Contains(message.Content, "{{"+name+"}}") }) {
			return ErrTemplate
		}
	}
	return nil
}

func render(template string, vars map[string]string, limit int64) (string, error) {
	var out strings.Builder
	parts := strings.Split(template, "{{")
	if int64(len(parts[0])) > limit {
		return "", ErrRenderTooLarge
	}
	out.WriteString(parts[0])
	for _, part := range parts[1:] {
		name, rest, _ := strings.Cut(part, "}}")
		if int64(out.Len())+int64(len(vars[name]))+int64(len(rest)) > limit {
			return "", ErrRenderTooLarge
		}
		out.WriteString(vars[name])
		out.WriteString(rest)
	}
	return out.String(), nil
}

type renderedMessage struct {
	message Message
	err     error
}

func renderMessages(messages []Message, vars map[string]string, remaining int64) iter.Seq[renderedMessage] {
	return func(yield func(renderedMessage) bool) {
		if len(messages) == 0 {
			return
		}
		content, err := render(messages[0].Content, vars, remaining)
		if !yield(renderedMessage{message: Message{Role: messages[0].Role, Content: content}, err: err}) || err != nil {
			return
		}
		renderMessages(messages[1:], vars, remaining-int64(len(content)))(yield)
	}
}

func (d Definition) RenderMessages(vars map[string]string, limit int64) ([]Message, error) {
	names, err := d.variableNames()
	if err != nil || d.validate(names) != nil || len(vars) != len(names) {
		return nil, ErrTemplate
	}
	for name, value := range vars {
		if !slices.Contains(names, name) || !utf8.ValidString(value) || len(d.Messages) == 0 && len(value) > MaxTemplateBytes {
			return nil, ErrTemplate
		}
	}
	if len(d.Messages) == 0 {
		return d.renderLegacyMessages(vars, limit)
	}
	return collectMessages(d.promptMessages(), vars, limit)
}

func collectMessages(messages []Message, vars map[string]string, limit int64) ([]Message, error) {
	results := slices.Collect(renderMessages(messages, vars, limit))
	if len(results) > 0 && results[len(results)-1].err != nil {
		return nil, results[len(results)-1].err
	}
	return slices.Collect(func(yield func(Message) bool) {
		for _, result := range results {
			if !yield(result.message) {
				return
			}
		}
	}), nil
}

func (d Definition) renderLegacyMessages(vars map[string]string, limit int64) ([]Message, error) {
	messages, err := collectMessages(d.promptMessages(), vars, min(limit, MaxTemplateBytes))
	if err != nil && limit >= MaxTemplateBytes {
		return nil, ErrTemplate
	}
	return messages, err
}

func (d Definition) Render(vars map[string]string) (string, error) {
	messages, err := d.RenderMessages(vars, MaxTemplateBytes)
	if err != nil || len(messages) != 1 {
		return "", ErrTemplate
	}
	return messages[0].Content, nil
}
