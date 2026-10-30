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
	requested := func() string {
		if target != resolved.Model {
			return resolved.PublicModel()
		}
		return resolved.RequestedModel
	}()
	return Operation{Name: resolved.Name, Model: target, RequestedModel: requested, Identity: resolved.Identity}, nil
}

func (r Route) ValidateModelPools() error {
	if len(r.ModelPools) > 100 || len(r.ModelPools) > 0 && !r.ModelRewriting {
		return fmt.Errorf("use at most 100 model pools and explicitly enable model rewriting")
	}
	for public, targets := range r.ModelPools {
		primary, err := r.ResolveModel(Operation{Model: public})
		if err != nil || len(targets) < 2 || len(targets) > 100 || targets[0] != primary.Model {
			return fmt.Errorf("each model pool must start with its published exact target and include an alternate")
		}
		for i, target := range targets {
			if !slices.Contains(r.Models, target) || slices.Contains(targets[:i], target) {
				return fmt.Errorf("model pools require distinct published native targets")
			}
		}
	}
	return nil
}

func (r Route) RecoveryModels(operation Operation, pins []Pin) ([]Operation, error) {
	selected, err := r.ResolvePinnedModel(operation, pins)
	if err != nil {
		return nil, err
	}
	return slices.AppendSeq([]Operation{selected}, func(yield func(Operation) bool) {
		if !r.ModelRewriting {
			return
		}
		for _, model := range r.ModelPools[selected.PublicModel()] {
			if model != selected.Model && slices.Contains(r.Models, model) && !yield(Operation{Name: selected.Name, Model: model, RequestedModel: selected.PublicModel(), Identity: selected.Identity}) {
				return
			}
		}
	}), nil
}
