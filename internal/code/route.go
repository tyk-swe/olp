package code

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/bodylimit"
)

type PaidTarget struct {
	ProviderID string `json:"provider_id"`
	SlotID     string `json:"slot_id"`
}

type Route struct {
	CodexCLIIdentity        bool                  `json:"codex_cli_identity,omitempty"`
	PaidFallbackTargets     map[string]PaidTarget `json:"paid_fallback_targets,omitempty"`
	ChatToAntigravity       bool                  `json:"chat_to_antigravity,omitempty"`
	PaidParticipation       bool                  `json:"paid_participation,omitempty"`
	PaidTargets             map[string]PaidTarget `json:"paid_targets,omitempty"`
	ModelPools              map[string][]string   `json:"model_pools,omitempty"`
	InitialRecoveryAttempts int                   `json:"initial_recovery_attempts,omitempty"`
	ModelRewriting          bool                  `json:"model_rewriting,omitempty"`
	Aliases                 map[string]string     `json:"aliases,omitempty"`
	ChatToCodex             bool                  `json:"chat_to_codex,omitempty"`
	MessagesToCodex         bool                  `json:"messages_to_codex,omitempty"`
	AllowanceOrdering       bool                  `json:"allowance_ordering,omitempty"`
	Tools                   *ResearchTools        `json:"tools,omitempty"`
	MaxBodyBytes            *int64                `json:"max_body_bytes,omitempty"`
	ID                      string                `json:"id"`
	ProjectID               string                `json:"project_id"`
	Slug                    string                `json:"slug"`
	PoolID                  string                `json:"pool_id"`
	Models                  []string              `json:"models"`
	Enabled                 bool                  `json:"enabled"`
	RevisionID              string                `json:"revision_id"`
	Revision                int                   `json:"revision"`
	ETag                    string                `json:"etag"`
	PublishedAt             *time.Time            `json:"published_at"`
}

func (r Route) BasePath() string { return BasePath + r.Slug }

func ValidateModels(models []string) error {
	if len(models) < 1 || len(models) > 100 {
		return fmt.Errorf("use between 1 and 100 native models")
	}
	seen := map[string]bool{}
	for _, model := range models {
		if !identifier.MatchString(model) || len(model) > 200 || seen[model] {
			return fmt.Errorf("use unique native model identifiers")
		}
		seen[model] = true
	}
	return nil
}

func (r Route) Validate() error {
	if err := r.ValidatePaidTargets(); err != nil {
		return err
	}
	if r.InitialRecoveryAttempts < 0 || r.InitialRecoveryAttempts > 2 {
		return fmt.Errorf("use zero to disable recovery or allow at most two additional attempts")
	}
	if !bodylimit.Valid(r.MaxBodyBytes) {
		return fmt.Errorf("use 1 to 1073741824 bytes, or null to inherit the installation limit")
	}
	if err := ValidateModels(r.Models); err != nil {
		return err
	}
	if err := r.ValidateAliases(); err != nil {
		return err
	}
	if err := r.ValidateModelPools(); err != nil {
		return err
	}
	return r.Tools.Validate()
}

func (r Route) ValidatePaidTargets() error {
	if len(r.PaidTargets)+len(r.PaidFallbackTargets) > 100 || (len(r.PaidTargets) > 0 || len(r.PaidFallbackTargets) > 0) && !r.PaidParticipation {
		return Refuse(400, "code_paid_participation_disabled")
	}
	if len(r.PaidFallbackTargets) > 0 && r.InitialRecoveryAttempts < 1 {
		return Refuse(400, "code_paid_fallback_recovery_required")
	}
	for model := range r.PaidFallbackTargets {
		if _, direct := r.PaidTargets[model]; direct {
			return Refuse(400, "code_paid_target_conflict")
		}
	}
	for model, target := range r.AllPaidTargets() {
		if !slices.Contains(r.Models, model) {
			return Refuse(400, "code_paid_model_invalid")
		}
		for _, id := range []string{target.ProviderID, target.SlotID} {
			parsed, err := uuid.Parse(id)
			if err != nil || parsed == uuid.Nil || parsed.String() != id {
				return Refuse(400, "code_paid_target_invalid")
			}
		}
	}
	return nil
}

func (r Route) PaidTarget(model string, fallback bool) PaidTarget {
	if fallback {
		return r.PaidFallbackTargets[model]
	}
	return r.PaidTargets[model]
}

func (r Route) AllPaidTargets() map[string]PaidTarget {
	return maps.Collect(func(yield func(string, PaidTarget) bool) {
		for _, targets := range []map[string]PaidTarget{r.PaidTargets, r.PaidFallbackTargets} {
			for model, target := range targets {
				if !yield(model, target) {
					return
				}
			}
		}
	})
}

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
	target := pins[pinned].TargetModel
	if target == "" {
		target = pins[pinned].Model
	}
	if !slices.Contains(r.Models, target) {
		return Operation{}, Refuse(403, "code_model_denied")
	}
	requested := resolved.RequestedModel
	if target != resolved.Model {
		requested = resolved.PublicModel()
	}
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

func (r Route) PaidFallbackModel(operation Operation, pins []Pin) (*Operation, error) {
	operations, err := r.RecoveryModels(operation, pins)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(operations, func(candidate Operation) bool { _, allowed := r.PaidFallbackTargets[candidate.Model]; return allowed })
	if index < 0 {
		return nil, nil
	}
	return &operations[index], nil
}
