package publishedprompt

import (
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/prompts"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

type parameter struct {
	name  string
	value string
}

func parameterNames(family wire.Format) (string, string) {
	if family == wire.Responses {
		return "max_output_tokens", "top_p"
	}
	if gemini(family) {
		return "maxOutputTokens", "topP"
	}
	return "max_tokens", "top_p"
}

func defaults(family wire.Format, parameters *prompts.Parameters) []parameter {
	if parameters == nil {
		return nil
	}
	maxField, topField := parameterNames(family)
	return slices.Collect(func(yield func(parameter) bool) {
		if parameters.Temperature != nil && !yield(parameter{name: "temperature", value: strconv.FormatFloat(*parameters.Temperature, 'g', -1, 64)}) {
			return
		}
		if parameters.TopP != nil && !yield(parameter{name: topField, value: strconv.FormatFloat(*parameters.TopP, 'g', -1, 64)}) {
			return
		}
		if parameters.MaxOutputTokens != nil {
			yield(parameter{name: maxField, value: strconv.FormatInt(*parameters.MaxOutputTokens, 10)})
		}
	})
}

func applyDefaults(doc oif.Document, family wire.Format, parameters *prompts.Parameters) (oif.Document, error) {
	if gemini(family) {
		normalized, err := normalizeGeminiConfig(doc)
		if err != nil {
			return oif.Document{}, err
		}
		return applyGeminiDefaults(normalized, family, parameters)
	}
	defaults := defaults(family, parameters)
	if len(defaults) == 0 {
		return doc, nil
	}
	return applyDefaultFields(doc, doc.Root(), "", family, defaults)
}

func normalizeGeminiConfig(doc oif.Document) (oif.Document, error) {
	normalized, err := normalizeGeminiFields(doc)
	if err != nil {
		return oif.Document{}, err
	}
	return mergeGeminiConfig(normalized)
}

func mergeGeminiConfig(doc oif.Document) (oif.Document, error) {
	config, hasSnake := doc.Root().Lookup("generation_config")
	camel, hasCamel := doc.Root().Lookup("generationConfig")
	if hasSnake && !hasCamel {
		normalized, err := oif.Apply(doc, []oif.Change{
			{Pointer: "/generation_config", Remove: true, Origin: oif.ResourceBinding, Reason: "prompt parameter normalization"},
			{Pointer: "/generationConfig", Value: config.Raw(), Origin: oif.ResourceBinding, Reason: "prompt parameter normalization"},
		})
		if err != nil {
			return oif.Document{}, err
		}
		return normalizeGeminiFields(normalized)
	}
	if !hasSnake || config.Kind() != oif.Object || camel.Kind() != oif.Object {
		return doc, nil
	}
	changes := slices.Collect(func(yield func(oif.Change) bool) {
		if !yield(oif.Change{Pointer: "/generation_config", Remove: true, Origin: oif.ResourceBinding, Reason: "prompt parameter normalization"}) {
			return
		}
		for _, member := range config.Members() {
			name := geminiParameterName(member.Name)
			if _, exists := camel.Lookup(name); exists {
				continue
			}
			if name != member.Name {
				if _, exists := config.Lookup(name); exists {
					continue
				}
			}
			if !yield(oif.Change{Pointer: oif.Pointer("/generationConfig", name), Value: member.Value.Raw(), Origin: oif.ResourceBinding, Reason: "prompt parameter normalization"}) {
				return
			}
		}
	})
	return oif.Apply(doc, changes)
}

func geminiParameterName(name string) string {
	switch name {
	case "top_p":
		return "topP"
	case "max_output_tokens":
		return "maxOutputTokens"
	default:
		return name
	}
}

func normalizeGeminiFields(doc oif.Document) (oif.Document, error) {
	config, exists := doc.Root().Lookup("generationConfig")
	if !exists || config.Kind() != oif.Object {
		return doc, nil
	}
	changes := slices.Collect(func(yield func(oif.Change) bool) {
		for _, field := range [][2]string{{"top_p", "topP"}, {"max_output_tokens", "maxOutputTokens"}} {
			value, hasSnake := config.Lookup(field[0])
			_, hasCamel := config.Lookup(field[1])
			if !hasSnake || hasCamel {
				continue
			}
			if !yield(oif.Change{Pointer: "/generationConfig/" + field[0], Remove: true, Origin: oif.ResourceBinding, Reason: "prompt parameter normalization"}) ||
				!yield(oif.Change{Pointer: "/generationConfig/" + field[1], Value: value.Raw(), Origin: oif.ResourceBinding, Reason: "prompt parameter normalization"}) {
				return
			}
		}
	})
	return oif.Apply(doc, changes)
}

func applyGeminiDefaults(doc oif.Document, family wire.Format, parameters *prompts.Parameters) (oif.Document, error) {
	defaults := defaults(family, parameters)
	if len(defaults) == 0 {
		return doc, nil
	}
	config, exists := doc.Root().Lookup("generationConfig")
	if !exists {
		fields := slices.Collect(func(yield func(string) bool) {
			for _, value := range defaults {
				if !yield(strconv.Quote(value.name) + ":" + value.value) {
					return
				}
			}
		})
		return oif.Apply(doc, []oif.Change{{Pointer: "/generationConfig", Value: "{" + strings.Join(fields, ",") + "}", Origin: oif.ResourceBinding, Reason: "published prompt defaults"}})
	}
	if config.Kind() != oif.Object {
		return doc, nil
	}
	return applyDefaultFields(doc, config, "/generationConfig", family, defaults)
}

func applyDefaultFields(doc oif.Document, container oif.Value, prefix string, family wire.Format, defaults []parameter) (oif.Document, error) {
	changes := slices.Collect(func(yield func(oif.Change) bool) {
		for _, value := range defaults {
			if _, exists := container.Lookup(value.name); exists {
				continue
			}
			if family == wire.Chat && value.name == "max_tokens" {
				if _, exists := container.Lookup("max_completion_tokens"); exists {
					continue
				}
			}
			if !yield(oif.Change{Pointer: prefix + "/" + value.name, Value: value.value, Origin: oif.ResourceBinding, Reason: "published prompt defaults"}) {
				return
			}
		}
	})
	return oif.Apply(doc, changes)
}
