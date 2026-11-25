package routes

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
)

type TargetInput struct {
	ProviderID      *string         `json:"provider_id"`
	ProviderModel   *string         `json:"provider_model"`
	ProviderModelID *string         `json:"provider_model_id"`
	Priority        int             `json:"priority"`
	Weight          int64           `json:"weight"`
	TimeoutMS       int             `json:"timeout_ms"`
	Tags            []string        `json:"tags,omitempty"`
	Shadow          *routing.Shadow `json:"shadow,omitempty"`
}

type DraftInput struct {
	routing.Behavior
	Slug             string          `json:"slug"`
	Operations       []string        `json:"operations"`
	OverallTimeoutMS int             `json:"overall_timeout_ms"`
	MaxAttempts      int             `json:"max_attempts"`
	Targets          []TargetInput   `json:"targets"`
	Fidelity         json.RawMessage `json:"fidelity,omitempty"`
	ProjectID        *string         `json:"project_id"`
}

func sameProject(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func ValidateAndNormalizeDraftInput(ctx context.Context, q database.Queryer, in *DraftInput, projectID *string, existing []routing.PublishedTarget) ([]routing.PublishedTarget, error) {
	if !routeidentity.ValidSlug(in.Slug) {
		return nil, httpkit.Invalid("slug", "Use 1–100 lowercase letters, digits, dots, underscores, or hyphens, starting with a letter or digit.")
	}
	if len(in.Operations) == 0 {
		in.Operations = []string{"generation"}
	}
	for _, op := range in.Operations {
		if !slices.Contains(supportedOperations, op) && !registeredOperation(op) {
			return nil, httpkit.Fail(422, "operation_unavailable", "The "+op+" operation is not available in this release.")
		}
	}
	in.Operations = slices.Compact(slices.Sorted(slices.Values(in.Operations)))
	if in.OverallTimeoutMS < 1 || in.OverallTimeoutMS > maxTimeoutMS {
		return nil, httpkit.Invalid("overall_timeout_ms", "Use a timeout from 1 to 3600000 milliseconds.")
	}
	if in.MaxAttempts < 1 || in.MaxAttempts > maxAttempts {
		return nil, httpkit.Invalid("max_attempts", "Use an attempt budget from 1 to 32767.")
	}
	var fidelityErr error
	in.Fidelity, fidelityErr = normalizedFidelity(in.Fidelity)
	if fidelityErr != nil {
		return nil, fidelityErr
	}
	targets, err := validateDraftTargets(ctx, q, in, projectID, existing)
	if err != nil {
		return nil, err
	}
	if err := validateDraftBehavior(ctx, q, in, targets); err != nil {
		return nil, err
	}
	return targets, nil
}

func validateDraftTargets(ctx context.Context, q database.Queryer, in *DraftInput, projectID *string, existing []routing.PublishedTarget) ([]routing.PublishedTarget, error) {
	if len(in.Targets) == 0 || len(in.Targets) > maxTargets {
		return nil, httpkit.Invalid("targets", "Use 1–64 targets.")
	}
	previous := map[string]routing.PublishedTarget{}
	for _, t := range existing {
		previous[t.ProviderModelID] = t
	}
	targets := make([]routing.PublishedTarget, 0, len(in.Targets))
	seen := map[string]bool{}
	for i, t := range in.Targets {
		if t.Priority < 0 || t.Priority > maxPriority {
			return nil, httpkit.Invalid("targets.priority", "Use a priority from 0 to 32767.")
		}
		if t.Weight < 1 || t.Weight > maxTargetWeight {
			return nil, httpkit.Invalid("targets.weight", "Use a weight from 1 to 1000000.")
		}
		if t.TimeoutMS < 1 || t.TimeoutMS > in.OverallTimeoutMS {
			return nil, httpkit.Invalid("targets.timeout_ms", "Use a target timeout from 1 millisecond up to the overall timeout.")
		}
		if !routing.ValidTargetTags(t.Tags) {
			return nil, httpkit.Invalid("targets.tags", "Use up to 8 distinct tags of lowercase letters, digits, underscores or hyphens.")
		}
		if t.Shadow != nil {
			if err := t.Shadow.Validate(); err != nil {
				return nil, err
			}
		}
		modelID, providerID, providerName, providerModel, err := lookupDraftTarget(ctx, q, t, projectID, i)
		if err != nil {
			return nil, err
		}
		if seen[modelID] {
			return nil, httpkit.Invalid("targets", "Each provider model may appear once per route.")
		}
		seen[modelID] = true
		id := ids.New()
		if prev, ok := previous[modelID]; ok {
			id = prev.ID
		}
		targets = append(targets, routing.PublishedTarget{ID: id, ProviderModelID: modelID, ProviderID: providerID, ProviderName: providerName, ProviderModel: providerModel, Priority: t.Priority, Weight: t.Weight, TimeoutMS: int64(t.TimeoutMS), Position: i, Tags: sortedTags(t.Tags), Shadow: t.Shadow})
	}
	if slices.IndexFunc(targets, func(t routing.PublishedTarget) bool { return t.Shadow == nil }) < 0 {
		return nil, httpkit.Invalid("targets", "Declare at least one target that serves callers rather than shadowing them.")
	}
	return targets, nil
}

func lookupDraftTarget(ctx context.Context, q database.Queryer, t TargetInput, projectID *string, i int) (modelID, providerID, providerName, providerModel string, err error) {
	var providerProject *string
	switch {
	case t.ProviderModelID != nil:
		if modelID, err = httpkit.ParseUUID(*t.ProviderModelID); err != nil {
			return "", "", "", "", httpkit.Invalid("targets.provider_model_id", "Use a provider model identifier.")
		}
		err = q.QueryRow(ctx, "SELECT p.id::text,p.name,m.upstream_model,p.project_id::text FROM olp.provider_models m JOIN olp.providers p ON p.id=m.provider_id WHERE m.id=$1", modelID).Scan(&providerID, &providerName, &providerModel, &providerProject)
	case t.ProviderID != nil && t.ProviderModel != nil:
		if providerID, err = httpkit.ParseUUID(*t.ProviderID); err != nil {
			return "", "", "", "", httpkit.Invalid("targets.provider_id", "Use a provider identifier.")
		}
		err = q.QueryRow(ctx, "SELECT m.id::text,p.name,m.upstream_model,p.project_id::text FROM olp.provider_models m JOIN olp.providers p ON p.id=m.provider_id WHERE p.id=$1 AND m.upstream_model=$2", providerID, *t.ProviderModel).Scan(&modelID, &providerName, &providerModel, &providerProject)
	default:
		return "", "", "", "", httpkit.Invalid("targets", "Name each target by provider_model_id or by provider_id and provider_model.")
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", "", httpkit.Invalid("targets."+strconv.Itoa(i)+".provider_model", "Target "+strconv.Itoa(i)+" names a model this installation does not know.")
	}
	if err != nil {
		return "", "", "", "", err
	}
	if !sameProject(providerProject, projectID) {
		return "", "", "", "", httpkit.Fail(422, "target_project_mismatch", "Target "+strconv.Itoa(i)+" belongs to a different project than the draft.")
	}
	return modelID, providerID, providerName, providerModel, nil
}

func validateDraftBehavior(ctx context.Context, q database.Queryer, in *DraftInput, targets []routing.PublishedTarget) error {
	behavior := in.Behavior
	fidelity, _ := routing.DecodeFidelity(in.Fidelity)
	if err := (routing.Route{Fidelity: fidelity, Behavior: behavior}).ValidateCaching(); err != nil {
		return httpkit.Invalid("cache", err.Error())
	}
	tagged := func(tag string) bool {
		return slices.ContainsFunc(targets, func(t routing.PublishedTarget) bool { return t.Shadow == nil && slices.Contains(t.Tags, tag) })
	}
	if err := behavior.Validate(in.Slug, tagged); err != nil {
		return err
	}
	if behavior.ResponsesMCPItems && fidelity.Strict() {
		return httpkit.Invalid("responses_mcp_items", "Responses MCP items require transformed fidelity.")
	}
	route := routing.Route{Slug: in.Slug, Operations: in.Operations, OverallTimeout: int64(in.OverallTimeoutMS), MaxAttempts: in.MaxAttempts, Fidelity: fidelity, Behavior: behavior}
	for _, t := range targets {
		route.Targets = append(route.Targets, routing.Target{ID: t.ID, ProviderID: t.ProviderID, ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, Timeout: t.TimeoutMS, RoutingID: t.ProviderModelID, Tags: t.Tags, Shadow: t.Shadow})
	}
	if err := route.ValidatePassthrough(); err != nil {
		return PassthroughAccessError(err)
	}
	return reserveOrdinarySlug(ctx, q, in.Slug)
}

func sortedTags(tags []string) []string {
	if len(tags) == 0 {
		return nil
	}
	return slices.Sorted(slices.Values(tags))
}

func reserveOrdinarySlug(ctx context.Context, q database.Queryer, slug string) error {
	var conflict bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_routes WHERE slug=$1)`, slug).Scan(&conflict); err != nil {
		return err
	}
	if conflict {
		return httpkit.Invalid("slug", "This slug belongs to a code route.")
	}
	return nil
}

func registeredOperation(operation string) bool {
	if retainedapi.Operation(operation) {
		return true
	}
	for _, d := range operationregistry.Catalog().Dialects() {
		if d.Operation.ID == operation {
			return true
		}
	}
	return false
}
