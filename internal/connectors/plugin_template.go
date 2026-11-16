package connectors

import (
	"errors"
	"regexp"
	"slices"
	"strings"
)

type template struct {
	text  string
	names []string
}

var placeholder = regexp.MustCompile(`\{([^{}]*)\}`)

func parseTemplate(text string, known func(name string) error) (template, error) {
	parsed := template{text: text}
	literal := placeholder.ReplaceAllStringFunc(text, func(match string) string {
		parsed.names = append(parsed.names, match[1:len(match)-1])
		return ""
	})
	if strings.ContainsAny(literal, "{}") {
		return template{}, errors.New("Use braces only around a placeholder.")
	}
	for _, name := range parsed.names {
		if err := known(name); err != nil {
			return template{}, err
		}
	}
	return parsed, nil
}

func (t template) uses(name string) bool { return slices.Contains(t.names, name) }

func (t template) render(values map[string]string) string {
	return placeholder.ReplaceAllStringFunc(t.text, func(match string) string { return values[match[1:len(match)-1]] })
}
