package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/runtime"
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
	fidelity, err := routing.DecodeFidelity(d.Fidelity)
	if err != nil || !fidelity.Strict() {
		return err
	}
	snapshot, err := runtime.ReadSnapshot(ctx, tx)
	if err != nil {
		return err
	}
	route := simulationRoute(d.ID, d.Slug, fidelity, d.Operations, d.OverallTimeoutMS, d.MaxAttempts, d.Targets)
	route.ProjectID = d.ProjectID
	var publishedID string
	if err = tx.QueryRow(ctx, "SELECT id::text FROM olp.routes WHERE slug=$1 AND project_id IS NOT DISTINCT FROM $2::uuid", d.Slug, d.ProjectID).Scan(&publishedID); err == nil {
		route.ID = publishedID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT policy_revision_id::text,position FROM olp.guardrail_draft_attachments WHERE draft_id=$1 ORDER BY position", d.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var revisionID string
		var position int
		if err = rows.Scan(&revisionID, &position); err != nil {
			rows.Close()
			return err
		}
		if snapshot.Guardrails == nil {
			rows.Close()
			return errors.New("draft guardrail policy is unavailable")
		}
		// Replace only the staged revision of the same additive slot for this
		// read-only validation; promotion checks policy identity before writing.
		attachments := snapshot.Guardrails.Attachments[:0]
		for _, a := range snapshot.Guardrails.Attachments {
			if a.Scope != "route" || a.ScopeID != route.ID || a.Order != position {
				attachments = append(attachments, a)
			}
		}
		snapshot.Guardrails.Attachments = append(attachments, guardrails.Attachment{ID: revisionID, Scope: "route", ScopeID: route.ID, PolicyRevisionID: revisionID, Order: position})
	}
	rows.Close()
	if err = rows.Err(); err != nil {
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
