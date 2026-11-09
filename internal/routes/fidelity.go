package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	guardrailattachment "github.com/tyk-swe/olp/internal/guardrails/attachment"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

// normalizedFidelity stores every draft with an explicit mode. Omission, null
// and an empty object declare a strict route; nothing is inherited on edit.
func normalizedFidelity(raw json.RawMessage) (json.RawMessage, error) {
	f, err := routing.DecodeFidelity(raw)
	if err != nil {
		return nil, access.Invalid("fidelity", err.Error())
	}
	return json.Marshal(f)
}

// ValidateFidelityPolicy is shared by draft validation, activation and
// configuration promotion so a direct draft SQL write cannot evade the rule.
func ValidateFidelityPolicy(fidelity, rawPolicy json.RawMessage) error {
	f, err := routing.DecodeFidelity(fidelity)
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
	if err = routing.ValidateRouteFidelity(f, policy); errors.Is(err, routing.ErrFidelityPolicyConflict) {
		return access.Fail(422, "fidelity_policy_conflict", err.Error())
	}
	return err
}

// compileDraftExecution checks the same configured links used by release
// installation. It is read-only and never performs provider inference or probes.
func compileDraftExecution(ctx context.Context, tx pgx.Tx, d *draft) error {
	route, err := draftRoute(d)
	if err != nil {
		return err
	}
	fidelity := route.Fidelity
	if !fidelity.Strict() && !route.IsPassthrough() {
		return nil
	}
	snapshot, err := release.ReadSnapshot(ctx, tx)
	if err != nil {
		return err
	}
	var publishedID string
	if err = tx.QueryRow(ctx, "SELECT id::text FROM olp.routes WHERE slug=$1 AND project_id IS NOT DISTINCT FROM $2::uuid", d.Slug, d.ProjectID).Scan(&publishedID); err == nil {
		route.ID = publishedID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	staged, err := guardrailattachment.Staged(ctx, tx, d.ID, route.ID)
	if err != nil {
		return err
	}
	if snapshot.Guardrails, err = snapshot.Guardrails.WithStaged(staged); err != nil {
		return err
	}
	if len(d.ContentPolicy) > 0 {
		route.ContentPolicy, err = contentpolicy.Decode(d.ContentPolicy)
		if err != nil {
			return err
		}
	}
	if err = snapshot.ValidateRouteContracts(route); err != nil {
		if refusal := access.ContractProblem(err); refusal != nil {
			return refusal
		}
		return err
	}
	return nil
}
