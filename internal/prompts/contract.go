// Package prompts owns the bounded, non-executable prompt template language.
// Template bodies are operator configuration; variables and rendered messages
// must stay transient and are never part of a published revision or approval.
package prompts

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const MaxTemplateBytes = 64 << 10

var namePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
var ErrTemplate = errors.New("invalid prompt template or variables")

type Definition struct {
	Template  string   `json:"template"`
	Variables []string `json:"variables"`
}

func (d Definition) Validate() error {
	if len(d.Template) > MaxTemplateBytes || !utf8.ValidString(d.Template) || len(d.Variables) > 256 {
		return ErrTemplate
	}
	seen := map[string]bool{}
	for _, name := range d.Variables {
		if !namePattern.MatchString(name) || seen[name] {
			return ErrTemplate
		}
		seen[name] = true
	}
	parts := strings.Split(d.Template, "{{")
	if strings.Contains(parts[0], "}}") {
		return ErrTemplate
	}
	used := map[string]bool{}
	for _, part := range parts[1:] {
		name, rest, ok := strings.Cut(part, "}}")
		if !ok || !seen[name] || strings.Contains(rest, "}}") || !namePattern.MatchString(name) {
			return ErrTemplate
		}
		used[name] = true
	}
	if len(used) != len(seen) {
		return ErrTemplate
	}
	return nil
}

// Render never persists inputs and does not interpret substituted delimiters.
// It is separate from native codecs; strict native requests cannot render it.
func (d Definition) Render(vars map[string]string) (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	if len(vars) != len(d.Variables) {
		return "", ErrTemplate
	}
	for name, v := range vars {
		if !slices.Contains(d.Variables, name) || !utf8.ValidString(v) || len(v) > MaxTemplateBytes {
			return "", ErrTemplate
		}
	}
	var out strings.Builder
	parts := strings.Split(d.Template, "{{")
	out.WriteString(parts[0])
	for _, part := range parts[1:] {
		name, rest, _ := strings.Cut(part, "}}")
		if out.Len()+len(vars[name])+len(rest) > MaxTemplateBytes {
			return "", ErrTemplate
		}
		out.WriteString(vars[name])
		out.WriteString(rest)
	}
	return out.String(), nil
}
