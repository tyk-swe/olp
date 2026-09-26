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

// normalizedFidelity stores every draft with an explicit mode. Omission, null
// and an empty object declare a strict route; nothing is inherited on edit.
func normalizedFidelity(raw json.RawMessage) (json.RawMessage, error) {
	f, err := runtime.DecodeFidelity(raw)
	if err != nil {
		return nil, access.Invalid("fidelity", err.Error())
	}
	return json.Marshal(f)
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
	if err != nil || !fidelity.Strict() {
		return err
	}
	snapshot, err := runtime.Compile(ctx, tx)
	if err != nil {
		return err
	}
	route := simulationRoute(d.ID, d.Slug, fidelity, d.Operations, d.OverallTimeoutMS, d.MaxAttempts, d.Targets)
	if len(d.ContentPolicy) > 0 {
		route.ContentPolicy, err = contentpolicy.Decode(d.ContentPolicy)
		if err != nil {
			return err
		}
	}
	if err = snapshot.CompileRouteExecution(route); err != nil {
		if refusal := runtime.StrictRefusal(err); refusal != nil {
			return refusal
		}
		return err
	}
	return nil
}
