package codemode

import (
	"fmt"
	"maps"
	"slices"
)

func (r Route) ValidateAliases() error {
	if len(r.Aliases) > 100 || len(r.Aliases) > 0 && !r.ModelRewriting {
		return fmt.Errorf("use at most 100 aliases and explicitly enable model rewriting")
	}
	for alias, target := range r.Aliases {
		if !identifier.MatchString(alias) || len(alias) > 200 || slices.Contains(r.Models, alias) || !slices.Contains(r.Models, target) {
			return fmt.Errorf("use distinct model aliases targeting one published native model")
		}
	}
	return nil
}

func (r Route) ResolveModel(operation Operation) (Operation, error) {
	requested := operation.PublicModel()
	if slices.Contains(r.Models, requested) {
		return operation, nil
	}
	target, ok := r.Aliases[requested]
	if !r.ModelRewriting || !ok || !slices.Contains(r.Models, target) {
		return Operation{}, Refuse(403, "code_model_denied")
	}
	return Operation{Name: operation.Name, Model: target, RequestedModel: requested, Identity: operation.Identity}, nil
}

func (r Route) PublicModels() []string {
	if !r.ModelRewriting {
		return slices.Clone(r.Models)
	}
	return slices.Concat(r.Models, slices.Sorted(maps.Keys(r.Aliases)))
}

func (r Route) ResolvePinnedModel(operation Operation, pins []Pin) (Operation, error) {
	resolved, err := r.ResolveModel(operation)
	if err != nil {
		return Operation{}, err
	}
	pinned := slices.IndexFunc(pins, func(pin Pin) bool { return pin.Model == resolved.PublicModel() })
	if pinned < 0 {
		return resolved, nil
	}
	target := func() string {
		if pins[pinned].TargetModel != "" {
			return pins[pinned].TargetModel
		}
		return pins[pinned].Model
	}()
	if !slices.Contains(r.Models, target) {
		return Operation{}, Refuse(403, "code_model_denied")
	}
	return Operation{Name: resolved.Name, Model: target, RequestedModel: resolved.RequestedModel, Identity: resolved.Identity}, nil
}
