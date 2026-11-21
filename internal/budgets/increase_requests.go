package budgets

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/limits"
)

const increaseRequestJSON = `to_jsonb(q)||jsonb_build_object('amount',q.amount::text,'base_cap',q.base_cap::text,'status',CASE WHEN q.status='pending' AND q.window_ends_at<=now() THEN 'expired' ELSE q.status END)`

type increaseRequest struct {
	id           string
	projectID    string
	ownerID      string
	windowKind   string
	windowID     int64
	windowEndsAt time.Time
	baseCap      string
	amount       string
	reason       string
	status       string
	requestedBy  string
	etag         string
}

type increaseRequestDecision struct {
	request increaseRequest
	policy  *keyauthority.BudgetPolicy
}

func (a *API) increaseRequests(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), "SELECT "+increaseRequestJSON+" FROM olp.budget_increase_requests q WHERE q.id<$1 AND ($2 OR q.project_id=ANY($3::uuid[])) ORDER BY q.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	items, err := httpkit.JSONRows(rows)
	return httpkit.ListReply(items, page), err
}

func (a *API) increaseRequestByID(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "request_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var data json.RawMessage
	var project, etag string
	if err = a.server.Pool.QueryRow(r.Context(), "SELECT "+increaseRequestJSON+",q.project_id::text,q.etag::text FROM olp.budget_increase_requests q WHERE q.id=$1", id).Scan(&data, &project, &etag); err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&project, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(data, etag), nil
}

func (a *API) createIncreaseRequest(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var in struct {
		ProjectID string `json:"project_id"`
		Window    string `json:"window"`
		Amount    string `json:"amount"`
		Reason    string `json:"reason"`
	}
	if err := httpkit.Decode(r, &in); err != nil {
		return httpkit.Reply{}, err
	}
	project, err := uuid.Parse(strings.TrimSpace(in.ProjectID))
	if err != nil {
		return httpkit.Reply{}, httpkit.Invalid("project_id", "A project UUID is required.")
	}
	in.ProjectID = project.String()
	in.Reason = strings.TrimSpace(in.Reason)
	in.Amount = strings.TrimSpace(in.Amount)
	if err := validateIncreaseTerms(in.Window, in.Amount, in.Reason); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p authz.Principal) (*keyauthority.BudgetPolicy, error) {
		if p.Kind != "user" {
			return nil, httpkit.Forbidden()
		}
		if err := p.Project(&in.ProjectID, authz.View); err != nil {
			return nil, err
		}
		var policy *keyauthority.BudgetPolicy
		err := tx.QueryRow(r.Context(), "SELECT budget_policy FROM olp.projects WHERE id=$1 FOR UPDATE", in.ProjectID).Scan(&policy)
		return policy, err
	}, func(tx pgx.Tx, p authz.Principal, policy *keyauthority.BudgetPolicy) (httpkit.Reply, access.Event, error) {
		cap := capForWindow(policy, in.Window)
		if cap == nil {
			return httpkit.Reply{}, access.Event{}, httpkit.Invalid("window", "This boundary has no permanent cap for the selected window.")
		}
		var now, end time.Time
		var window int64
		if err := tx.QueryRow(r.Context(), `SELECT now(),window_id,end_at FROM olp.budget_window($1,now())`, in.Window).Scan(&now, &window, &end); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if !end.After(now) {
			return httpkit.Reply{}, access.Event{}, staleIncreaseRequest("The budget window for this request has ended, so a new request is needed.")
		}
		id, etag := httpkit.NewID(), httpkit.NewID()
		owner := limits.AggregateBudgetID("project", in.ProjectID)
		if _, err := tx.Exec(r.Context(), `INSERT INTO olp.budget_increase_requests(id,project_id,owner_id,window_kind,window_id,window_ends_at,base_cap,amount,reason,requested_by,etag) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, id, in.ProjectID, owner, in.Window, window, end, *cap, in.Amount, in.Reason, p.UserID(), etag); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		data, err := readIncreaseRequest(r.Context(), tx, id)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		reply := httpkit.Reply{Status: 201, Body: data, ETag: etag, Location: "/api/v1/budget-increase-requests/" + id}
		return reply, access.Event{Action: "budget.increase_request.create", Resource: "budget_increase_request", ID: id, Project: &in.ProjectID}, nil
	})
}

func (a *API) approveIncreaseRequest(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return a.decideIncreaseRequest(r, true)
}

func (a *API) declineIncreaseRequest(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	return a.decideIncreaseRequest(r, false)
}

func (a *API) decideIncreaseRequest(r *http.Request, approve bool) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "request_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.server, r, id, func(tx pgx.Tx, p authz.Principal) (increaseRequestDecision, error) {
		return lockIncreaseRequestDecision(r, tx, p, id, approve)
	}, func(tx pgx.Tx, p authz.Principal, state increaseRequestDecision) (httpkit.Reply, access.Event, error) {
		return applyIncreaseRequestDecision(r, tx, p, state, approve)
	})
}

func lockIncreaseRequestDecision(r *http.Request, tx pgx.Tx, p authz.Principal, id string, approve bool) (increaseRequestDecision, error) {
	ctx := r.Context()
	var state increaseRequestDecision
	err := tx.QueryRow(ctx, "SELECT project_id::text,owner_id::text,window_kind,requested_by::text FROM olp.budget_increase_requests WHERE id=$1", id).Scan(&state.request.projectID, &state.request.ownerID, &state.request.windowKind, &state.request.requestedBy)
	if err != nil {
		return state, err
	}
	if err := authorizeIncreaseRequestReviewer(p, state.request, approve); err != nil {
		return state, err
	}
	if err := lockBudgetWindow(ctx, tx, state.request.ownerID, state.request.windowKind); err != nil {
		return state, err
	}
	err = tx.QueryRow(ctx, "SELECT id::text,project_id::text,owner_id::text,window_kind,window_id,window_ends_at,base_cap::text,amount::text,reason,status,requested_by::text,etag::text FROM olp.budget_increase_requests WHERE id=$1 FOR UPDATE", id).Scan(&state.request.id, &state.request.projectID, &state.request.ownerID, &state.request.windowKind, &state.request.windowID, &state.request.windowEndsAt, &state.request.baseCap, &state.request.amount, &state.request.reason, &state.request.status, &state.request.requestedBy, &state.request.etag)
	if err != nil {
		return state, err
	}
	err = tx.QueryRow(ctx, "SELECT budget_policy FROM olp.projects WHERE id=$1 FOR UPDATE", state.request.projectID).Scan(&state.policy)
	return state, err
}

func authorizeIncreaseRequestReviewer(p authz.Principal, request increaseRequest, approve bool) error {
	if err := p.Authorize(authz.Keys); err != nil {
		return err
	}
	if err := p.Project(&request.projectID, authz.Change); err != nil {
		return err
	}
	if approve && request.requestedBy == p.UserID() {
		return httpkit.Fail(403, "budget_increase_request_self_approval", "You cannot approve your own budget increase request.")
	}
	return nil
}

func applyIncreaseRequestDecision(r *http.Request, tx pgx.Tx, p authz.Principal, state increaseRequestDecision, approve bool) (httpkit.Reply, access.Event, error) {
	if state.request.status != "pending" {
		if state.request.status == "approved" && approve || state.request.status == "declined" && !approve {
			data, err := readIncreaseRequest(r.Context(), tx, state.request.id)
			if err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			return httpkit.Reply{Status: http.StatusOK, Body: data, ETag: state.request.etag}, access.Event{}, nil
		}
		return httpkit.Reply{}, access.Event{}, httpkit.Fail(409, "budget_increase_request_decided", "This budget increase request has already been decided.")
	}
	if !approve {
		return declinePendingIncreaseRequest(r, tx, p, state.request)
	}
	return grantPendingIncreaseRequest(r, tx, p, state)
}

func declinePendingIncreaseRequest(r *http.Request, tx pgx.Tx, p authz.Principal, request increaseRequest) (httpkit.Reply, access.Event, error) {
	ctx := r.Context()
	etag := httpkit.NewID()
	if _, err := tx.Exec(ctx, `UPDATE olp.budget_increase_requests SET status='declined',decided_by=$2,decided_at=now(),etag=$3 WHERE id=$1 AND status='pending'`, request.id, p.UserID(), etag); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	data, err := readIncreaseRequest(ctx, tx, request.id)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	reply := httpkit.Reply{Status: http.StatusOK, Body: data, ETag: etag}
	return reply, access.Event{Action: "budget.increase_request.decline", Resource: "budget_increase_request", ID: request.id, Project: &request.projectID}, nil
}

func grantPendingIncreaseRequest(r *http.Request, tx pgx.Tx, p authz.Principal, state increaseRequestDecision) (httpkit.Reply, access.Event, error) {
	ctx := r.Context()
	request := state.request
	if err := checkIncreaseRequestCurrent(ctx, tx, state); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if err := checkActiveIncreaseLimit(ctx, tx, request.ownerID, request.windowKind); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&now); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	expires := request.windowEndsAt.Truncate(time.Millisecond)
	if !expires.After(now) {
		return httpkit.Reply{}, access.Event{}, staleIncreaseRequest("The budget window for this request has ended, so a new request is needed.")
	}
	increaseID, increaseETag, requestETag := httpkit.NewID(), httpkit.NewID(), httpkit.NewID()
	encoded, _ := json.Marshal(BudgetTarget{Kind: "project", ID: request.projectID})
	if _, err := tx.Exec(ctx, `INSERT INTO olp.budget_increases(id,owner_id,project_id,target,window_kind,window_id,amount,reason,starts_at,expires_at,window_ends_at,created_by,etag) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, increaseID, request.ownerID, request.projectID, encoded, request.windowKind, request.windowID, request.amount, request.reason, now, expires, request.windowEndsAt, p.UserID(), increaseETag); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.budget_increase_requests SET status='approved',decided_by=$2,decided_at=$3,increase_id=$4,etag=$5 WHERE id=$1 AND status='pending'`, request.id, p.UserID(), now, increaseID, requestETag); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if _, err := access.AdvanceAuthority(ctx, tx); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if err := access.Audit(ctx, tx, r, p.Actor(), "budget.increase.create", "budget_increase", increaseID, "success"); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	data, err := readIncreaseRequest(ctx, tx, request.id)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	reply := httpkit.Reply{Status: http.StatusOK, Body: data, ETag: requestETag}
	return reply, access.Event{Action: "budget.increase_request.approve", Resource: "budget_increase_request", ID: request.id, Project: &request.projectID}, nil
}

func checkIncreaseRequestCurrent(ctx context.Context, tx pgx.Tx, state increaseRequestDecision) error {
	request := state.request
	var now, end time.Time
	var window int64
	if err := tx.QueryRow(ctx, `SELECT now(),window_id,end_at FROM olp.budget_window($1,now())`, request.windowKind).Scan(&now, &window, &end); err != nil {
		return err
	}
	if window != request.windowID || !end.Equal(request.windowEndsAt) || !request.windowEndsAt.After(now) {
		return staleIncreaseRequest("The budget window for this request has ended, so a new request is needed.")
	}
	cap := capForWindow(state.policy, request.windowKind)
	if cap == nil {
		return staleIncreaseRequest("The base cap for this budget changed after the request was made, so a new request is needed.")
	}
	var same bool
	if err := tx.QueryRow(ctx, `SELECT $1::numeric=$2::numeric`, request.baseCap, *cap).Scan(&same); err != nil {
		return err
	}
	if !same {
		return staleIncreaseRequest("The base cap for this budget changed after the request was made, so a new request is needed.")
	}
	return nil
}

func staleIncreaseRequest(detail string) error {
	return httpkit.Fail(409, "budget_increase_request_stale", detail)
}

func readIncreaseRequest(ctx context.Context, tx pgx.Tx, id string) (json.RawMessage, error) {
	var data json.RawMessage
	err := tx.QueryRow(ctx, "SELECT "+increaseRequestJSON+" FROM olp.budget_increase_requests q WHERE q.id=$1", id).Scan(&data)
	return data, err
}
