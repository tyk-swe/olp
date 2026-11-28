package budgets

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/platform/database"
)

const increaseJSON = `to_jsonb(i)||jsonb_build_object('amount',i.amount::text)`

func (a *API) increases(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := database.QueryJSON(r.Context(), a.server.Pool, "SELECT "+increaseJSON+" FROM olp.budget_increases i WHERE i.id<$1 AND ($2 OR i.project_id=ANY($3::uuid[]) OR i.organization_id=ANY($5::uuid[])) ORDER BY i.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1, p.OrganizationIDs())
	return httpkit.ListReply(items, page), err
}

func (a *API) createIncrease(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var in struct {
		Target    BudgetTarget `json:"target"`
		Window    string       `json:"window"`
		Amount    string       `json:"amount"`
		Reason    string       `json:"reason"`
		ExpiresAt *time.Time   `json:"expires_at"`
	}
	if err := httpkit.Decode(r, &in); err != nil {
		return httpkit.Reply{}, err
	}
	if err := in.Target.validate(); err != nil {
		return httpkit.Reply{}, err
	}
	in.Reason = strings.TrimSpace(in.Reason)
	in.Amount = strings.TrimSpace(in.Amount)
	if err := validateIncreaseTerms(in.Window, in.Amount, in.Reason); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p authz.Principal) (increaseTarget, error) {
		owner, project, policy, err := a.target(r, tx, p, in.Target)
		return increaseTarget{owner: owner, project: project, policy: policy}, err
	}, func(tx pgx.Tx, p authz.Principal, target increaseTarget) (httpkit.Reply, access.Event, error) {
		cap := capForWindow(target.policy, in.Window)
		if cap == nil {
			return httpkit.Reply{}, access.Event{}, problem.Invalid("window", "This boundary has no permanent cap for the selected window.")
		}
		var now, end time.Time
		var window int64
		if err := tx.QueryRow(r.Context(), `SELECT now(),window_id,end_at FROM olp.budget_window($1,now())`, in.Window).Scan(&now, &window, &end); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		expires := end
		if in.ExpiresAt != nil && in.ExpiresAt.Before(expires) {
			expires = *in.ExpiresAt
		}
		expires = expires.Truncate(time.Millisecond)
		if !expires.After(now) {
			return httpkit.Reply{}, access.Event{}, problem.Invalid("expires_at", "Expiry must be in the future, within the current budget window.")
		}
		if err := lockBudgetWindow(r.Context(), tx, target.owner, in.Window); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err := checkActiveIncreaseLimit(r.Context(), tx, target.owner, in.Window); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		id, etag := ids.New(), ids.New()
		encoded, _ := json.Marshal(in.Target)
		_, err := tx.Exec(r.Context(), `INSERT INTO olp.budget_increases(id,owner_id,project_id,organization_id,target,window_kind,window_id,amount,reason,starts_at,expires_at,window_ends_at,created_by,etag) VALUES($1,$2,$3,CASE WHEN $4::jsonb->>'kind'='organization' THEN ($4::jsonb->>'id')::uuid ELSE NULL END,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, id, target.owner, target.project, encoded, in.Window, window, in.Amount, in.Reason, now, expires, end, p.UserID(), etag)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = access.AdvanceAuthority(r.Context(), tx); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		var data json.RawMessage
		err = tx.QueryRow(r.Context(), "SELECT "+increaseJSON+" FROM olp.budget_increases i WHERE id=$1", id).Scan(&data)
		reply := httpkit.Reply{Status: 201, Body: data, ETag: etag, Location: "/api/v1/budget-increases/" + id}
		return reply, access.Event{Action: "budget.increase.create", Resource: "budget_increase", ID: id}, err
	})
}

type increaseTarget struct {
	owner   string
	project *string
	policy  *keyauthority.BudgetPolicy
}

func (a *API) revokeIncrease(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "increase_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(a.server, r, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		var target BudgetTarget
		var project *string
		var etag string
		if err := tx.QueryRow(r.Context(), "SELECT target,project_id::text,etag::text FROM olp.budget_increases WHERE id=$1 FOR UPDATE", id).Scan(&target, &project, &etag); err != nil {
			return struct{}{}, err
		}
		if err := budgetIncreaseScope(p, target, project, authz.Change); err != nil {
			return struct{}{}, err
		}
		operations := map[string]authz.Operation{"installation": authz.Settings, "organization": authz.ManageOrganization}
		operation, special := operations[target.Kind]
		if !special {
			operation = authz.Keys
		}
		if err := p.Authorize(operation); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, httpkit.Match(r, etag)
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		if _, err := tx.Exec(r.Context(), `UPDATE olp.budget_increases SET revoked_at=COALESCE(revoked_at,now()),revoked_by=COALESCE(revoked_by,$2),etag=$3 WHERE id=$1`, id, p.UserID(), ids.New()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		_, err := access.AdvanceAuthority(r.Context(), tx)
		return httpkit.Reply{Status: http.StatusNoContent}, access.Event{Action: "budget.increase.revoke", Resource: "budget_increase", ID: id}, err
	})
}

func (a *API) increase(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "increase_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var data json.RawMessage
	var project *string
	var target BudgetTarget
	var etag string
	if err = a.server.Pool.QueryRow(r.Context(), "SELECT "+increaseJSON+",project_id::text,etag::text,target FROM olp.budget_increases i WHERE id=$1", id).Scan(&data, &project, &etag, &target); err != nil {
		return httpkit.Reply{}, err
	}
	if err = budgetIncreaseScope(p, target, project, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(data, etag), nil
}

func validateIncreaseTerms(window, amount, reason string) error {
	if !limits.ValidCostLimit(amount) {
		return problem.Invalid("amount", "Use a positive USD decimal with at most 12 integer and 12 fractional digits.")
	}
	if reason == "" || utf8.RuneCountInString(reason) > 256 || strings.ContainsAny(reason, "\x00\r\n") {
		return problem.Invalid("reason", "Give a single-line reason of 1–256 characters.")
	}
	if window != "day" && window != "week" && window != "month" {
		return problem.Invalid("window", "Choose day, week or month.")
	}
	return nil
}

func capForWindow(policy *keyauthority.BudgetPolicy, window string) *string {
	if policy == nil {
		return nil
	}
	switch window {
	case "day":
		return policy.DailyCostLimit
	case "week":
		return policy.WeeklyCostLimit
	case "month":
		return policy.MonthlyCostLimit
	}
	return nil
}

func lockBudgetWindow(ctx context.Context, tx pgx.Tx, owner, window string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('olp:budget-increase:'||$1||':'||$2,0))`, owner, window)
	return err
}

func checkActiveIncreaseLimit(ctx context.Context, tx pgx.Tx, owner, window string) error {
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM olp.budget_increases WHERE owner_id=$1 AND window_kind=$2 AND revoked_at IS NULL AND expires_at>now()`, owner, window).Scan(&count); err != nil {
		return err
	}
	if count >= 8 {
		return problem.Fail(409, "budget_increase_limit", "At most eight active increases may apply to one budget window.")
	}
	return nil
}

func budgetIncreaseScope(p authz.Principal, target BudgetTarget, project *string, need authz.Need) error {
	if target.Kind == "organization" {
		return p.Organization(target.ID, need)
	}
	return p.Project(project, need)
}
