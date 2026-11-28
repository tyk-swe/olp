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
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/keyauthority"
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
	items, err := httpkit.QueryJSON(r.Context(), a.server.Pool, "SELECT "+increaseRequestJSON+" FROM olp.budget_increase_requests q WHERE q.id<$1 AND ($2 OR q.project_id=ANY($3::uuid[])) ORDER BY q.id DESC LIMIT $4", page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	return httpkit.ListReply(items, page), err
}

func (a *API) increaseRequestByID(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "request_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var data json.RawMessage
	var project, etag string
	if err := a.server.Pool.QueryRow(r.Context(), "SELECT "+increaseRequestJSON+",q.project_id::text,q.etag::text FROM olp.budget_increase_requests q WHERE q.id=$1", id).Scan(&data, &project, &etag); err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(&project, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(data, etag), nil
}

type increaseRequestInput struct {
	ProjectID string `json:"project_id"`
	Window    string `json:"window"`
	Amount    string `json:"amount"`
	Reason    string `json:"reason"`
}

func (a *API) createIncreaseRequest(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var raw increaseRequestInput
	if err := httpkit.Decode(r, &raw); err != nil {
		return httpkit.Reply{}, err
	}
	project, err := uuid.Parse(strings.TrimSpace(raw.ProjectID))
	if err != nil {
		return httpkit.Reply{}, httpkit.Invalid("project_id", "A project UUID is required.")
	}
	in := increaseRequestInput{ProjectID: project.String(), Window: raw.Window, Amount: strings.TrimSpace(raw.Amount), Reason: strings.TrimSpace(raw.Reason)}
	if err := validateIncreaseTerms(in.Window, in.Amount, in.Reason); err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.server, r, in, func(tx pgx.Tx, p authz.Principal) (*keyauthority.BudgetPolicy, error) {
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
		var end time.Time
		var window int64
		if err := tx.QueryRow(r.Context(), `SELECT window_id,end_at FROM olp.budget_window($1,now())`, in.Window).Scan(&window, &end); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		id, etag := ids.New(), ids.New()
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
	var scope increaseRequest
	if err := tx.QueryRow(ctx, "SELECT project_id::text,owner_id::text,window_kind,requested_by::text FROM olp.budget_increase_requests WHERE id=$1", id).Scan(&scope.projectID, &scope.ownerID, &scope.windowKind, &scope.requestedBy); err != nil {
		return increaseRequestDecision{}, err
	}
	if err := authorizeIncreaseRequestReviewer(p, scope, approve); err != nil {
		return increaseRequestDecision{}, err
	}
	if err := lockBudgetWindow(ctx, tx, scope.ownerID, scope.windowKind); err != nil {
		return increaseRequestDecision{}, err
	}
	var policy *keyauthority.BudgetPolicy
	if err := tx.QueryRow(ctx, "SELECT budget_policy FROM olp.projects WHERE id=$1 FOR UPDATE", scope.projectID).Scan(&policy); err != nil {
		return increaseRequestDecision{}, err
	}
	var request increaseRequest
	err := tx.QueryRow(ctx, "SELECT id::text,project_id::text,owner_id::text,window_kind,window_id,window_ends_at,base_cap::text,amount::text,reason,status,requested_by::text,etag::text FROM olp.budget_increase_requests WHERE id=$1 FOR UPDATE", id).Scan(&request.id, &request.projectID, &request.ownerID, &request.windowKind, &request.windowID, &request.windowEndsAt, &request.baseCap, &request.amount, &request.reason, &request.status, &request.requestedBy, &request.etag)
	return increaseRequestDecision{request: request, policy: policy}, err
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
	etag := ids.New()
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
	increaseID, increaseETag, requestETag := ids.New(), ids.New(), ids.New()
	encoded, _ := json.Marshal(BudgetTarget{Kind: "project", ID: request.projectID})
	if _, err := tx.Exec(ctx, `INSERT INTO olp.budget_increases(id,owner_id,project_id,target,window_kind,window_id,amount,reason,starts_at,expires_at,window_ends_at,created_by,etag) VALUES($1,$2,$3,$4,$5,$6,$7,$8,now(),$9,$10,$11,$12)`, increaseID, request.ownerID, request.projectID, encoded, request.windowKind, request.windowID, request.amount, request.reason, request.windowEndsAt.Truncate(time.Millisecond), request.windowEndsAt, p.UserID(), increaseETag); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE olp.budget_increase_requests SET status='approved',decided_by=$2,decided_at=now(),increase_id=$3,etag=$4 WHERE id=$1 AND status='pending'`, request.id, p.UserID(), increaseID, requestETag); err != nil {
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
	var sameWindow, sameCap bool
	if err := tx.QueryRow(ctx, `SELECT w.window_id=$2 AND w.end_at=$3,COALESCE($4::numeric=$5::numeric,false) FROM olp.budget_window($1,now()) w`, request.windowKind, request.windowID, request.windowEndsAt, request.baseCap, capForWindow(state.policy, request.windowKind)).Scan(&sameWindow, &sameCap); err != nil {
		return err
	}
	if !sameWindow {
		return staleIncreaseRequest("The budget window for this request has ended, so a new request is needed.")
	}
	if !sameCap {
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
