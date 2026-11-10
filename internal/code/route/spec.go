package route

import (
	"errors"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/routeidentity"
)

type Input struct {
	CodexCLIIdentity        bool                       `json:"codex_cli_identity"`
	PaidFallbackTargets     map[string]code.PaidTarget `json:"paid_fallback_targets"`
	ChatToAntigravity       bool                       `json:"chat_to_antigravity"`
	PaidParticipation       bool                       `json:"paid_participation"`
	PaidTargets             map[string]code.PaidTarget `json:"paid_targets"`
	ModelPools              map[string][]string        `json:"model_pools"`
	InitialRecoveryAttempts int                        `json:"initial_recovery_attempts"`
	ModelRewriting          bool                       `json:"model_rewriting"`
	Aliases                 map[string]string          `json:"aliases"`
	ChatToCodex             bool                       `json:"chat_to_codex"`
	MessagesToCodex         bool                       `json:"messages_to_codex"`
	AllowanceOrdering       bool                       `json:"allowance_ordering"`
	Tools                   *code.ResearchTools        `json:"tools"`
	MaxBodyBytes            *int64                     `json:"max_body_bytes"`
	ProjectID               string                     `json:"project_id"`
	Slug                    string                     `json:"slug"`
	PoolID                  string                     `json:"pool_id"`
	Models                  []string                   `json:"models"`
	Enabled                 bool                       `json:"enabled"`
}

type Draft struct {
	input Input
}

func ParseDraft(in Input) (Draft, error) {
	ids, err := access.ParseUUIDs([]string{in.ProjectID, in.PoolID})
	if err != nil {
		return Draft{}, err
	}
	normalized := in
	normalized.ProjectID, normalized.PoolID = ids[0], ids[1]
	if normalized.Aliases == nil {
		normalized.Aliases = map[string]string{}
	}
	if normalized.ModelPools == nil {
		normalized.ModelPools = map[string][]string{}
	}
	if normalized.PaidTargets == nil {
		normalized.PaidTargets = map[string]code.PaidTarget{}
	}
	if normalized.PaidFallbackTargets == nil {
		normalized.PaidFallbackTargets = map[string]code.PaidTarget{}
	}
	candidate := normalized.toRoute()
	if err := validatePaidTargets(candidate); err != nil {
		return Draft{}, err
	}
	if normalized.InitialRecoveryAttempts < 0 || normalized.InitialRecoveryAttempts > 2 {
		return Draft{}, access.Invalid("initial_recovery_attempts", "Use zero to disable recovery or allow at most two additional attempts.")
	}
	if !routeidentity.ValidSlug(normalized.Slug) {
		return Draft{}, access.Invalid("slug", "Use 1–100 lowercase characters starting with a letter or digit.")
	}
	if !bodylimit.Valid(normalized.MaxBodyBytes) {
		return Draft{}, access.Invalid("max_body_bytes", "Use 1 to 1073741824 bytes, or null to inherit the installation limit.")
	}
	if err := code.ValidateModels(normalized.Models); err != nil {
		return Draft{}, access.Invalid("models", err.Error())
	}
	if err := candidate.ValidateAliases(); err != nil {
		return Draft{}, access.Invalid("aliases", err.Error())
	}
	if err := candidate.ValidateModelPools(); err != nil {
		return Draft{}, access.Invalid("model_pools", err.Error())
	}
	if err := normalized.Tools.Validate(); err != nil {
		return Draft{}, access.Invalid("tools", err.Error())
	}
	if normalized.Tools != nil {
		accountID, err := access.ParseUUID(normalized.Tools.AccountID)
		if err != nil {
			return Draft{}, err
		}
		normalized.Tools = &code.ResearchTools{AccountID: accountID, Services: slices.Clone(normalized.Tools.Services)}
	}
	return Draft{input: normalized}, nil
}

func (in Input) Validate() error {
	_, err := ParseDraft(in)
	return err
}

func (d Draft) ProjectID() string { return d.input.ProjectID }
func (d Draft) PoolID() string    { return d.input.PoolID }
func (d Draft) Slug() string      { return d.input.Slug }
func (d Draft) Input() Input      { return d.input }

func (in Input) toRoute() code.Route {
	return code.Route{
		ProjectID: in.ProjectID, Slug: in.Slug, PoolID: in.PoolID, Models: in.Models,
		PaidParticipation: in.PaidParticipation, PaidTargets: maps.Clone(in.PaidTargets),
		PaidFallbackTargets: maps.Clone(in.PaidFallbackTargets),
		Aliases:             maps.Clone(in.Aliases), ModelRewriting: in.ModelRewriting,
		ModelPools:       maps.Clone(in.ModelPools),
		CodexCLIIdentity: in.CodexCLIIdentity, ChatToCodex: in.ChatToCodex,
		MessagesToCodex: in.MessagesToCodex, ChatToAntigravity: in.ChatToAntigravity,
		AllowanceOrdering: in.AllowanceOrdering, InitialRecoveryAttempts: in.InitialRecoveryAttempts,
		Tools: in.Tools, MaxBodyBytes: in.MaxBodyBytes, Enabled: in.Enabled,
	}
}

func validatePaidTargets(candidate code.Route) error {
	err := candidate.ValidatePaidTargets()
	if err == nil {
		return nil
	}
	var refusal *code.Refusal
	if errors.As(err, &refusal) && refusal.Code == "code_paid_fallback_recovery_required" {
		return access.Invalid("paid_fallback_targets", "Enable bounded initial recovery before selecting paid fallback targets.")
	}
	if errors.As(err, &refusal) && refusal.Code == "code_paid_target_conflict" {
		return access.Invalid("paid_fallback_targets", "Choose either direct paid selection or subscription-first fallback for each native model.")
	}
	return access.Invalid("paid_targets", "Enable paid participation and map only published native models to provider and credential slot UUIDs.")
}
