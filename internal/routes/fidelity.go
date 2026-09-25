package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/runtime"
)

// PublishedFidelity inherits a same-project contract for omission-safe edits
// that begin by creating another draft for an existing slug.
func PublishedFidelity(ctx context.Context, q access.Queryer, slug string, project *string) (json.RawMessage, error) {
	var raw []byte
	err := q.QueryRow(ctx, `SELECT v.fidelity FROM olp_go.routes r JOIN olp_go.route_revisions v ON v.id=r.latest_revision_id
        WHERE r.slug=$1 AND r.project_id IS NOT DISTINCT FROM $2::uuid`, slug, project).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}

// ValidateFidelityMigration keeps the contract advertised by a published slug
// consistent even while unsupported readers retain an older runtime snapshot.
func ValidateFidelityMigration(ctx context.Context, q access.Queryer, slug string, raw json.RawMessage) error {
	fidelity, err := runtime.DecodeFidelity(raw)
	if err != nil {
		return access.Invalid("fidelity", err.Error())
	}
	var strict bool
	err = q.QueryRow(ctx, "SELECT strict_contract FROM olp_go.routes WHERE slug=$1", slug).Scan(&strict)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if strict != (runtime.FidelityMode(fidelity) == runtime.FidelityStrict) {
		return access.Fail(422, "route_fidelity_migration_required", "Changing a published route's strict contract requires a migration draft with a new slug.")
	}
	return nil
}

// requireStatedFidelity refuses to publish a draft that omits its contract
// once the route has published an explicit one. Storage enforces the same rule;
// checking it here reports the draft as the caller's to fix.
func requireStatedFidelity(ctx context.Context, q access.Queryer, slug string, raw json.RawMessage) error {
	if len(raw) > 0 {
		return nil
	}
	var explicit bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp_go.routes r JOIN olp_go.route_revisions v ON v.route_id=r.id
        WHERE r.slug=$1 AND v.fidelity IS NOT NULL)`, slug).Scan(&explicit)
	if err != nil {
		return err
	}
	if explicit {
		return access.Fail(422, "route_fidelity_required", "This route has published an explicit contract; state the draft's fidelity mode.")
	}
	return nil
}

func normalizedFidelity(raw json.RawMessage) (json.RawMessage, error) {
	f, err := runtime.DecodeFidelity(raw)
	if err != nil {
		return nil, access.Invalid("fidelity", err.Error())
	}
	if f == nil {
		return nil, nil
	}
	out, err := json.Marshal(f)
	return out, err
}

// ValidateFidelityPolicy is shared by draft validation, activation and
// configuration promotion so a direct draft SQL write cannot evade the rule.
func ValidateFidelityPolicy(fidelity, rawPolicy json.RawMessage) error {
	f, err := runtime.DecodeFidelity(fidelity)
	if err != nil {
		return access.Invalid("fidelity", err.Error())
	}
	var policy *contentpolicy.Policy
	if len(rawPolicy) > 0 && !bytes.Equal(bytes.TrimSpace(rawPolicy), []byte("null")) {
		policy, err = contentpolicy.Decode(rawPolicy)
		if err != nil {
			return access.Invalid("content_policy", err.Error())
		}
	}
	if err = runtime.ValidateRouteFidelity(f, policy); errors.Is(err, runtime.ErrFidelityPolicyConflict) {
		return access.Fail(422, "fidelity_policy_conflict", err.Error())
	}
	return err
}

// compileDraftExecution checks the same configured links used by release
// installation. It is read-only and never performs provider inference or probes.
func compileDraftExecution(ctx context.Context, tx pgx.Tx, d *draft) error {
	fidelity, err := runtime.DecodeFidelity(d.Fidelity)
	if err != nil || runtime.FidelityMode(fidelity) != runtime.FidelityStrict {
		return err
	}
	snapshot, err := runtime.Compile(ctx, tx)
	if err != nil {
		return err
	}
	route := simulationRoute(d.ID, d.Slug, d.Operations, d.OverallTimeoutMS, d.MaxAttempts, d.Targets)
	route.Fidelity = fidelity
	if len(d.ContentPolicy) > 0 {
		route.ContentPolicy, err = contentpolicy.Decode(d.ContentPolicy)
		if err != nil {
			return err
		}
	}
	if err = snapshot.CompileRouteExecution(route); err != nil {
		var failure interface {
			Incompatibility() (code, field, requirement, message string)
		}
		if errors.As(err, &failure) {
			code, _, _, message := failure.Incompatibility()
			return access.Fail(422, code, message)
		}
		return err
	}
	return nil
}
