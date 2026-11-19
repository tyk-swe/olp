package access

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/limits"
)

const increaseJSON = `to_jsonb(i)||jsonb_build_object('amount',i.amount::text)`

func (s *Server) budgetIncreases(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+increaseJSON+" FROM olp.budget_increases i WHERE i.id<$1 AND ($2 OR i.project_id=ANY($3::uuid[]) OR i.organization_id=ANY($5::uuid[])) ORDER BY i.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1, p.OrganizationIDs())
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	return httpkit.ListReply(items, page), err
}

func (s *Server) createBudgetIncrease(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
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
	if !limits.ValidCostLimit(in.Amount) {
		return httpkit.Reply{}, httpkit.Invalid("amount", "Use a positive USD decimal with at most 12 integer and 12 fractional digits.")
	}
	if in.Reason == "" || utf8.RuneCountInString(in.Reason) > 256 || strings.ContainsAny(in.Reason, "\x00\r\n") {
		return httpkit.Reply{}, httpkit.Invalid("reason", "Give a single-line reason of 1–256 characters.")
	}
	if in.Window != "day" && in.Window != "week" && in.Window != "month" {
		return httpkit.Reply{}, httpkit.Invalid("window", "Choose day, week or month.")
	}
	return Mutate(s, r, in, func(tx pgx.Tx, p authz.Principal) (increaseTarget, error) {
		owner, project, policy, err := s.budgetIncreaseTarget(r, tx, p, in.Target)
		return increaseTarget{owner: owner, project: project, policy: policy}, err
	}, func(tx pgx.Tx, p authz.Principal, target increaseTarget) (httpkit.Reply, Event, error) {
		var cap *string
		if target.policy != nil {
			switch in.Window {
			case "day":
				cap = target.policy.DailyCostLimit
			case "week":
				cap = target.policy.WeeklyCostLimit
			case "month":
				cap = target.policy.MonthlyCostLimit
			}
		}
		if cap == nil {
			return httpkit.Reply{}, Event{}, httpkit.Invalid("window", "This boundary has no permanent cap for the selected window.")
		}
		var now, end time.Time
		var window int64
		if err := tx.QueryRow(r.Context(), `SELECT now(),window_id,end_at FROM olp.budget_window($1,now())`, in.Window).Scan(&now, &window, &end); err != nil {
			return httpkit.Reply{}, Event{}, err
		}
		expires := end
		if in.ExpiresAt != nil && in.ExpiresAt.Before(expires) {
			expires = *in.ExpiresAt
		}
		expires = expires.Truncate(time.Millisecond)
		if !expires.After(now) {
			return httpkit.Reply{}, Event{}, httpkit.Invalid("expires_at", "Expiry must be in the future, within the current budget window.")
		}
		var count int
		if err := tx.QueryRow(r.Context(), `SELECT count(*) FROM olp.budget_increases WHERE owner_id=$1 AND window_kind=$2 AND revoked_at IS NULL AND expires_at>now()`, target.owner, in.Window).Scan(&count); err != nil {
			return httpkit.Reply{}, Event{}, err
		}
		if count >= 8 {
			return httpkit.Reply{}, Event{}, httpkit.Fail(409, "budget_increase_limit", "At most eight active increases may apply to one budget window.")
		}
		id, etag := httpkit.NewID(), httpkit.NewID()
		encoded, _ := json.Marshal(in.Target)
		_, err := tx.Exec(r.Context(), `INSERT INTO olp.budget_increases(id,owner_id,project_id,organization_id,target,window_kind,window_id,amount,reason,starts_at,expires_at,window_ends_at,created_by,etag) VALUES($1,$2,$3,CASE WHEN $4::jsonb->>'kind'='organization' THEN ($4::jsonb->>'id')::uuid ELSE NULL END,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, id, target.owner, target.project, encoded, in.Window, window, in.Amount, in.Reason, now, expires, end, p.UserID(), etag)
		if err != nil {
			return httpkit.Reply{}, Event{}, err
		}
		if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
			return httpkit.Reply{}, Event{}, err
		}
		var data json.RawMessage
		err = tx.QueryRow(r.Context(), "SELECT "+increaseJSON+" FROM olp.budget_increases i WHERE id=$1", id).Scan(&data)
		reply := httpkit.Reply{Status: 201, Body: data, ETag: etag, Location: "/api/v1/budget-increases/" + id}
		return reply, Event{Action: "budget.increase.create", Resource: "budget_increase", ID: id}, err
	})
}

type increaseTarget struct {
	owner   string
	project *string
	policy  *keyauthority.BudgetPolicy
}

func (s *Server) revokeBudgetIncrease(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "increase_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return Transact(s, r, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
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
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, Event, error) {
		if _, err := tx.Exec(r.Context(), `UPDATE olp.budget_increases SET revoked_at=COALESCE(revoked_at,now()),revoked_by=COALESCE(revoked_by,$2),etag=$3 WHERE id=$1`, id, p.UserID(), httpkit.NewID()); err != nil {
			return httpkit.Reply{}, Event{}, err
		}
		_, err := AdvanceAuthority(r.Context(), tx)
		return httpkit.Reply{Status: http.StatusNoContent}, Event{Action: "budget.increase.revoke", Resource: "budget_increase", ID: id}, err
	})
}

func (s *Server) budgetIncrease(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "increase_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var data json.RawMessage
	var project *string
	var target BudgetTarget
	var etag string
	if err = s.Pool.QueryRow(r.Context(), "SELECT "+increaseJSON+",project_id::text,etag::text,target FROM olp.budget_increases i WHERE id=$1", id).Scan(&data, &project, &etag, &target); err != nil {
		return httpkit.Reply{}, err
	}
	if err = budgetIncreaseScope(p, target, project, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(data, etag), nil
}

func budgetIncreaseScope(p authz.Principal, target BudgetTarget, project *string, need authz.Need) error {
	if target.Kind == "organization" {
		return p.Organization(target.ID, need)
	}
	return p.Project(project, need)
}
