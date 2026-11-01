package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
)

type TypeSafeConfig struct {
	Route string `json:"route"`
}

type TypeSafePolicy struct {
	Entry    Entry
	Revision Revision
	Config   TypeSafeConfig
}

type typeSafeDetector struct {
	config TypeSafeConfig
}

func compileTypeSafe(raw json.RawMessage) (detector, error) {
	var config TypeSafeConfig
	if err := decodeConfig(raw, &config); err != nil {
		return nil, err
	}
	if !identifier.MatchString(config.Route) {
		return nil, errors.New("typesafe requires a decision route")
	}
	return typeSafeDetector{config: config}, nil
}

func (typeSafeDetector) width() int { return 0 }

func (typeSafeDetector) detect(context.Context, Evaluation) (Verdict, error) {
	return Verdict{Action: "pass"}, nil
}

func (e *Engine) TypeSafePolicies() []TypeSafePolicy {
	if e == nil {
		return nil
	}
	return slices.Collect(func(yield func(TypeSafePolicy) bool) {
		for _, entry := range e.entries {
			detector, ok := entry.detector.(typeSafeDetector)
			if !ok || entry.entry.Phase != Input || entry.entry.Monitor {
				continue
			}
			if !yield(TypeSafePolicy{Entry: entry.entry, Revision: entry.definition, Config: detector.config}) {
				return
			}
		}
	})
}

func (e *Engine) OnlyTypeSafe() bool {
	return e != nil && len(e.entries) > 0 && !slices.ContainsFunc(e.entries, func(entry compiledEntry) bool {
		_, ok := entry.detector.(typeSafeDetector)
		return !ok
	})
}
