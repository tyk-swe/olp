package access

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/limits"
)

const increaseJSON = `to_jsonb(i)||jsonb_build_object('amount',i.amount::text)`

func (s *Server) budgetIncreases(r *http.Request, p Principal) (Reply, error) {
	page, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+increaseJSON+" FROM olp.budget_increases i WHERE i.id<$1 AND ($2 OR i.project_id=ANY($3::uuid[]) OR i.organization_id=ANY($5::uuid[])) ORDER BY i.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1, p.OrganizationIDs())
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, page), err
}

func (s *Server) createBudgetIncrease(r *http.Request, _ Principal) (Reply, error) {
	var in struct {
		Target    BudgetTarget `json:"target"`
		Window    string       `json:"window"`
		Amount    string       `json:"amount"`
		Reason    string       `json:"reason"`
		ExpiresAt *time.Time   `json:"expires_at"`
	}
	if err := Decode(r, &in); err != nil {
		return Reply{}, err
	}
	if err := in.Target.validate(); err != nil {
		return Reply{}, err
	}
	in.Reason = strings.TrimSpace(in.Reason)
	in.Amount = strings.TrimSpace(in.Amount)
	if !limits.ValidCostLimit(in.Amount) {
		return Reply{}, Invalid("amount", "Use a positive USD decimal with at most 12 integer and 12 fractional digits.")
	}
	if in.Reason == "" || utf8.RuneCountInString(in.Reason) > 256 || strings.ContainsAny(in.Reason, "\x00\r\n") {
		return Reply{}, Invalid("reason", "Give a single-line reason of 1–256 characters.")
	}
	if in.Window != "day" && in.Window != "week" && in.Window != "month" {
		return Reply{}, Invalid("window", "Choose day, week or month.")
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	owner, project, policy, err := s.budgetIncreaseTarget(r, tx, p, in.Target)
	if err != nil {
		return Reply{}, err
	}
	claim, replay, err := s.Replay(r, tx, p, in)
	if err != nil {
		return Reply{}, err
	}
	if replay != nil {
		return Commit(r, tx, *replay)
	}
	var cap *string
	if policy != nil {
		switch in.Window {
		case "day":
			cap = policy.DailyCostLimit
		case "week":
			cap = policy.WeeklyCostLimit
		case "month":
			cap = policy.MonthlyCostLimit
		}
	}
	if cap == nil {
		return Reply{}, Invalid("window", "This boundary has no permanent cap for the selected window.")
	}
	var now, end time.Time
	var window int64
	if err = tx.QueryRow(r.Context(), `SELECT now(),window_id,end_at FROM olp.budget_window($1,now())`, in.Window).Scan(&now, &window, &end); err != nil {
		return Reply{}, err
	}
	expires := end
	if in.ExpiresAt != nil && in.ExpiresAt.Before(expires) {
		expires = *in.ExpiresAt
	}
	expires = expires.Truncate(time.Millisecond)
	if !expires.After(now) {
		return Reply{}, Invalid("expires_at", "Expiry must be in the future, within the current budget window.")
	}
	var count int
	if err = tx.QueryRow(r.Context(), `SELECT count(*) FROM olp.budget_increases WHERE owner_id=$1 AND window_kind=$2 AND revoked_at IS NULL AND expires_at>now()`, owner, in.Window).Scan(&count); err != nil {
		return Reply{}, err
	}
	if count >= 8 {
		return Reply{}, Fail(409, "budget_increase_limit", "At most eight active increases may apply to one budget window.")
	}
	id, etag := NewID(), NewID()
	target, _ := json.Marshal(in.Target)
	_, err = tx.Exec(r.Context(), `INSERT INTO olp.budget_increases(id,owner_id,project_id,organization_id,target,window_kind,window_id,amount,reason,starts_at,expires_at,window_ends_at,created_by,etag) VALUES($1,$2,$3,CASE WHEN $4::jsonb->>'kind'='organization' THEN ($4::jsonb->>'id')::uuid ELSE NULL END,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, id, owner, project, target, in.Window, window, in.Amount, in.Reason, now, expires, end, p.UserID(), etag)
	if err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "budget.increase.create", "budget_increase", id, "success"); err != nil {
		return Reply{}, err
	}
	var data json.RawMessage
	if err = tx.QueryRow(r.Context(), "SELECT "+increaseJSON+" FROM olp.budget_increases i WHERE id=$1", id).Scan(&data); err != nil {
		return Reply{}, err
	}
	result := Reply{Status: 201, Body: data, ETag: etag, Location: "/api/v1/budget-increases/" + id}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}

func (s *Server) revokeBudgetIncrease(r *http.Request, _ Principal) (Reply, error) {
	id, err := IDParam(r, "increase_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	var target BudgetTarget
	var project *string
	var etag string
	if err = tx.QueryRow(r.Context(), "SELECT target,project_id::text,etag::text FROM olp.budget_increases WHERE id=$1 FOR UPDATE", id).Scan(&target, &project, &etag); err != nil {
		return Reply{}, err
	}
	if err = budgetIncreaseScope(p, target, project, Change); err != nil {
		return Reply{}, err
	}
	op := Keys
	if target.Kind == "installation" {
		op = Settings
	}
	if target.Kind == "organization" {
		op = ManageOrganization
	}
	if err = p.Authorize(op); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), `UPDATE olp.budget_increases SET revoked_at=COALESCE(revoked_at,now()),revoked_by=COALESCE(revoked_by,$2),etag=$3 WHERE id=$1`, id, p.UserID(), NewID()); err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "budget.increase.revoke", "budget_increase", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Reply{Status: http.StatusNoContent})
}

func (s *Server) budgetIncrease(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "increase_id")
	if err != nil {
		return Reply{}, err
	}
	var data json.RawMessage
	var project *string
	var target BudgetTarget
	var etag string
	if err = s.Pool.QueryRow(r.Context(), "SELECT "+increaseJSON+",project_id::text,etag::text,target FROM olp.budget_increases i WHERE id=$1", id).Scan(&data, &project, &etag, &target); err != nil {
		return Reply{}, err
	}
	if err = budgetIncreaseScope(p, target, project, View); err != nil {
		return Reply{}, err
	}
	return Detail(data, etag), nil
}

func budgetIncreaseScope(p Principal, target BudgetTarget, project *string, need Need) error {
	if target.Kind == "organization" {
		return p.Organization(target.ID, need)
	}
	return p.Project(project, need)
}
