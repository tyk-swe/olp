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

func requireFidelityExecution(raw json.RawMessage) error {
	f, err := runtime.DecodeFidelity(raw)
	if err != nil {
		return access.Invalid("fidelity", err.Error())
	}
	if err = runtime.RequireRouteExecution(f); err != nil {
		return access.Fail(422, "strict_execution_unavailable", err.Error())
	}
	return nil
}
