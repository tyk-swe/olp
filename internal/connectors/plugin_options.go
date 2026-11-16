package connectors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tyk-swe/olp/plugin/abi"
)

type OptionError struct {
	Option  string
	Message string
}

func (e *OptionError) Error() string { return "option " + e.Option + ": " + e.Message }

func (p *PluginProfile) ValidateOptions(values map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if !slices.ContainsFunc(p.declared.Options, func(option abi.Option) bool { return option.Name == name }) {
			return &OptionError{Option: name, Message: "The profile declares no option with this name."}
		}
	}
	for _, option := range p.declared.Options {
		value, set := values[option.Name]
		switch {
		case !set && !option.Optional:
			return &OptionError{Option: option.Name, Message: "Set " + option.Label + "; the profile requires it."}
		case !set:
		case !validOptionValue(value):
			return &OptionError{Option: option.Name, Message: "Use 1–256 characters without control characters."}
		case len(option.Enum) > 0 && !slices.Contains(option.Enum, value):
			return &OptionError{Option: option.Name, Message: "Choose one of " + strings.Join(option.Enum, ", ") + "."}
		case option.Pattern != "" && !p.patterns[option.Name].MatchString(value):
			return &OptionError{Option: option.Name, Message: "Use a value matching " + option.Pattern + "."}
		case (value == "." || value == "..") && p.hosting.address.uses(optionPlaceholder+option.Name):
			return &OptionError{Option: option.Name, Message: "Use a value other than . or .., which would move the address."}
		}
	}
	return nil
}

func parseOptions(declared []abi.Option) (map[string]*regexp.Regexp, error) {
	if len(declared) > 16 {
		return nil, &ProfileError{Field: "options", Message: "Declare at most 16 options."}
	}
	patterns := map[string]*regexp.Regexp{}
	for i, option := range declared {
		field := fmt.Sprintf("options[%d]", i)
		switch {
		case !valueName.MatchString(option.Name):
			return nil, &ProfileError{Field: field + ".name", Message: "Name the option with 1–64 lowercase letters, digits and underscores, starting with a letter."}
		case slices.ContainsFunc(declared[:i], func(prior abi.Option) bool { return prior.Name == option.Name }):
			return nil, &ProfileError{Field: field + ".name", Message: "Declare each option once."}
		case !PlainText(option.Label, 1, 100):
			return nil, &ProfileError{Field: field + ".label", Message: "Label the option with 1–100 characters, without control characters."}
		case !PlainText(option.Description, 0, 500):
			return nil, &ProfileError{Field: field + ".description", Message: "Describe the option in at most 500 characters, without control characters."}
		case len(option.Enum) > 0 && option.Pattern != "":
			return nil, &ProfileError{Field: field + ".pattern", Message: "Declare an enum or a pattern, not both: the enum already fixes the values."}
		case len(option.Enum) > 64:
			return nil, &ProfileError{Field: field + ".enum", Message: "List at most 64 values."}
		}
		for j, value := range option.Enum {
			if !validOptionValue(value) || slices.Contains(option.Enum[:j], value) {
				return nil, &ProfileError{Field: fmt.Sprintf("%s.enum[%d]", field, j), Message: "List distinct values of 1–256 characters without control characters."}
			}
		}
		if option.Pattern == "" {
			continue
		}
		pattern, err := regexp.Compile(option.Pattern)
		if err != nil || len(option.Pattern) > 512 {
			return nil, &ProfileError{Field: field + ".pattern", Message: "Use a regular expression of at most 512 characters in RE2 syntax."}
		}
		patterns[option.Name] = pattern
	}
	return patterns, nil
}

func validOptionValue(value string) bool { return PlainText(value, 1, 256) }

func PlainText(text string, least, most int) bool {
	n := utf8.RuneCountInString(text)
	return utf8.ValidString(text) && n >= least && n <= most && !strings.ContainsFunc(text, unicode.IsControl)
}

func optionsSchema(declared []abi.Option) json.RawMessage {
	var properties bytes.Buffer
	required := []string{}
	properties.WriteByte('{')
	for i, option := range declared {
		schema := map[string]any{"type": "string", "title": option.Label, "minLength": 1, "maxLength": 256}
		if option.Description != "" {
			schema["description"] = option.Description
		}
		if len(option.Enum) > 0 {
			schema["enum"] = option.Enum
		}
		if option.Pattern != "" {
			schema["pattern"] = option.Pattern
		}
		if !option.Optional {
			required = append(required, option.Name)
		}
		value, _ := json.Marshal(schema)
		if i > 0 {
			properties.WriteByte(',')
		}
		writeMember(&properties, option.Name, value)
	}
	properties.WriteByte('}')
	schema, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": json.RawMessage(properties.Bytes())})
	return schema
}
