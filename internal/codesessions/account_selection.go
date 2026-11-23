package codesessions

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/routing"
)

type codeCandidate struct {
	account       codemode.Account
	outcome       codemode.Candidate
	cooldownRetry *time.Time
}

type codeSelection struct {
	account    codemode.Account
	candidates []codemode.Candidate
}

func (s codeSelection) chosen() (codemode.Candidate, bool) {
	index := slices.IndexFunc(s.candidates, func(candidate codemode.Candidate) bool {
		return candidate.AccountID == s.account.ID
	})
	if index < 0 {
		return codemode.Candidate{}, false
	}
	return s.candidates[index], true
}

// codeAccount selects the pool account that serves an admission: an eligible
// one of the admission's providers, so its adapter serves the request's path
// and the gateway can reach it. It is the account id when given, else the
// first of tree that serves the model, else the first available account of an
// adapter no account of tree has. An account of tree serves only while no
// earlier one has its adapter, now or as the root's pins recorded it, since a
// republish can change or drop an account's connection; a selected account
// that is unavailable refuses the admission.
func codeAccount(ctx context.Context, tx pgx.Tx, in Admission, id, root string, tree []string) (codeSelection, error) {
	var none codeSelection
	if _, paid := in.Route.PaidTargets[in.Operation.Model]; paid {
		return none, codemode.Refuse(503, "code_account_unavailable")
	}
	required := []string{in.Operation.Model}
	if in.Operation.Name == "connect" {
		required = in.Route.Models
	}
	if id != "" && (in.Operation.Name == "connect" || in.Operation.Name == "connect_model") {
		required = []string{}
	}
	cooldownModels := func() []string {
		if in.Operation.Name != "connect" {
			return []string{in.Operation.Model}
		}
		if id != "" {
			return []string{}
		}
		return in.Route.Models
	}()
	requiredJSON, err := json.Marshal(required)
	if err != nil {
		return none, err
	}
	routeID, err := uuid.Parse(in.Route.ID)
	if err != nil {
		return none, err
	}
	rows, err := tx.Query(ctx, `SELECT a.id::text,a.project_id::text,a.provider_id::text,a.credential_id::text,a.principal,a.models,a.name,a.enabled,a.etag::text,a.health,a.allowance,olp.code_account_available(a),(a.health IN ('unavailable','quota_limited') AND NOT coalesce(a.unavailable_until<=now(),false)),coalesce(array_position($6::text[],a.id::text),2147483647),pa.weight,a.unavailable_until,EXISTS(SELECT 1 FROM olp.code_account_cooldowns cd WHERE cd.account_id=a.id AND cd.retry_at>now() AND (cd.model IS NULL OR cd.model=ANY($10::text[]))),(SELECT max(cd.retry_at) FROM olp.code_account_cooldowns cd WHERE cd.account_id=a.id AND cd.retry_at>now() AND (cd.model IS NULL OR cd.model=ANY($10::text[]))),a.extra_usage_enabled,a.cycle_anchor::text
		FROM olp.code_accounts a JOIN olp.code_pool_accounts pa ON pa.account_id=a.id
		JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=a.provider_id
		JOIN olp.code_route_revisions v ON v.id=$7 CROSS JOIN LATERAL (SELECT `+codeadapter.SQL("v.connections->(a.provider_id::text)")+` adapter) f
		WHERE pa.pool_id=$1 AND a.project_id=$2 AND a.enabled AND p.state<>'disabled' AND p.project_id=a.project_id
		AND ($4<>'' OR NOT a.draining)
		AND c.provider_id=a.provider_id AND c.principal=a.principal AND c.revoked_at IS NULL AND `+grants.LiveCondition+`
		AND ($9::uuid IS NULL OR NOT EXISTS(SELECT 1 FROM olp.code_recovery_chain($9) rejected WHERE rejected.account_id=a.id AND rejected.nonexecution))
		AND a.models @> $3::jsonb AND ($4='' OR a.id::text=$4) AND a.provider_id::text=ANY($5::text[])
		AND NOT EXISTS(SELECT 1 FROM olp.code_accounts t WHERE t.id::text=ANY($6::text[])
			AND coalesce(array_position($6::text[],t.id::text)<array_position($6::text[],a.id::text),true)
			AND (`+codeadapter.SQL("v.connections->(t.provider_id::text)")+` IS NOT DISTINCT FROM f.adapter
				OR EXISTS(SELECT 1 FROM olp.code_pins p WHERE p.root_id=nullif($8,'')::uuid AND p.account_id=t.id AND p.adapter IS NOT DISTINCT FROM f.adapter)))`,
		in.Route.PoolID, in.Route.ProjectID, requiredJSON, id, in.Providers, tree, in.Route.RevisionID, root, codeRecoveryExclusion(in.Recovery), cooldownModels)
	if err != nil {
		return none, err
	}
	now := time.Now().UTC()
	candidates, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (codeCandidate, error) {
		var candidate codeCandidate
		var weight int64
		var cooling bool
		if err := row.Scan(&candidate.account.ID, &candidate.account.ProjectID, &candidate.account.ProviderID, &candidate.account.CredentialID, &candidate.account.Principal, &candidate.account.Models, &candidate.account.Name, &candidate.account.Enabled, &candidate.account.ETag, &candidate.account.Health, &candidate.account.Allowance, &candidate.outcome.Available, &candidate.outcome.Cooldown, &candidate.outcome.Position, &weight, &candidate.account.UnavailableUntil, &cooling, &candidate.cooldownRetry, &candidate.account.ExtraUsageEnabled, &candidate.account.CycleAnchor); err != nil {
			return candidate, err
		}
		accountID, err := uuid.Parse(candidate.account.ID)
		if err != nil {
			return candidate, err
		}
		outcome := codemode.Candidate{AccountID: candidate.account.ID, Available: candidate.outcome.Available && !cooling, Cooldown: candidate.outcome.Cooldown || cooling, Position: candidate.outcome.Position, Score: routing.Score(routeID, accountID, weight, in.Operation.Name, "code", in.Operation.Model, []byte(in.APIKeyID+":"+in.Operation.Identity.Conversation))}
		return codeCandidate{account: candidate.account, outcome: outcome, cooldownRetry: candidate.cooldownRetry}, nil
	})
	if err != nil {
		return none, err
	}
	checkCapacity := codeGeneration(in.Operation.Name) || id == "" && root == ""
	chosen, outcomes, err := chooseCodeAccount(ctx, tx, checkCapacity, candidates)
	if err != nil {
		return none, err
	}
	if !chosen.Eligible() {
		return unavailableCodeAccount(chosen, candidates, outcomes, now)
	}
	index := slices.IndexFunc(candidates, func(candidate codeCandidate) bool {
		return candidate.account.ID == chosen.AccountID
	})
	return codeSelection{account: candidates[index].account, candidates: outcomes}, nil
}

func unavailableCodeAccount(chosen codemode.Candidate, candidates []codeCandidate, outcomes []codemode.Candidate, now time.Time) (codeSelection, error) {
	if len(candidates) == 0 {
		return codeSelection{}, codemode.Refuse(503, "code_account_unavailable")
	}
	if slices.ContainsFunc(outcomes, func(c codemode.Candidate) bool { return c.Position == chosen.Position && c.Busy }) {
		return codeSelection{candidates: outcomes}, codeAccountBusy()
	}
	deadlines := slices.Collect(func(yield func(*time.Time) bool) {
		for _, candidate := range candidates {
			if candidate.outcome.Position != chosen.Position {
				continue
			}
			if (candidate.account.Health == "unavailable" || candidate.account.Health == "quota_limited") && candidate.account.UnavailableUntil == nil {
				if !yield(nil) {
					return
				}
				continue
			}
			unavailableUntil := func() *time.Time {
				if candidate.account.Health == "unavailable" || candidate.account.Health == "quota_limited" {
					return candidate.account.UnavailableUntil
				}
				return nil
			}()
			if !yield(codemode.BlockingDeadline(now, unavailableUntil, candidate.cooldownRetry, candidate.account.Allowance)) {
				return
			}
		}
	})
	return codeSelection{candidates: outcomes}, &codemode.Refusal{Status: 503, Code: "code_account_unavailable", RetryAt: codemode.EarliestRecovery(now, deadlines...)}
}
