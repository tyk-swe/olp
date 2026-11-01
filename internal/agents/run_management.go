package agents

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type RunView struct {
	ID              string    `db:"id" json:"id"`
	APIKeyID        string    `db:"api_key_id" json:"api_key_id"`
	ProjectID       string    `db:"project_id" json:"project_id"`
	RevisionID      *string   `db:"revision_id" json:"revision_id"`
	RouteRevisionID *string   `db:"route_revision_id" json:"route_revision_id"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
}

type RunStepView struct {
	ID              string    `db:"id" json:"id"`
	DispatchID      string    `db:"dispatch_id" json:"dispatch_id"`
	Ordinal         int       `db:"ordinal" json:"ordinal"`
	Kind            string    `db:"kind" json:"kind"`
	State           string    `db:"state" json:"state"`
	Currency        string    `db:"currency" json:"currency"`
	CostBound       string    `db:"cost_bound" json:"cost_bound"`
	ChargeBound     string    `db:"charge_bound" json:"charge_bound"`
	ObservedCost    string    `db:"observed_cost" json:"observed_cost"`
	ObservedCharge  string    `db:"observed_charge" json:"observed_charge"`
	RemainingCost   string    `db:"remaining_cost" json:"remaining_cost"`
	RemainingCharge string    `db:"remaining_charge" json:"remaining_charge"`
	BoundViolation  bool      `db:"bound_violation" json:"bound_violation"`
	ToolAlias       *string   `db:"tool_alias" json:"tool_alias"`
	ToolStatus      *string   `db:"tool_status" json:"tool_status"`
	ToolIsError     *bool     `db:"tool_is_error" json:"tool_is_error"`
	PricingState    *string   `db:"pricing_state" json:"pricing_state"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
}

type RunApprovalView struct {
	ID         string     `db:"id" json:"id"`
	ExpiresAt  time.Time  `db:"expires_at" json:"expires_at"`
	ConsumedAt *time.Time `db:"consumed_at" json:"consumed_at"`
	DispatchID *string    `db:"dispatch_id" json:"dispatch_id"`
}

type RunSpanView struct {
	ID           string    `db:"id" json:"id"`
	TraceID      string    `db:"trace_id" json:"trace_id"`
	SpanID       string    `db:"span_id" json:"span_id"`
	ParentSpanID *string   `db:"parent_span_id" json:"parent_span_id"`
	AttemptID    *string   `db:"attempt_id" json:"attempt_id"`
	StartedNanos string    `db:"started_nanos" json:"started_nanos"`
	EndedNanos   string    `db:"ended_nanos" json:"ended_nanos"`
	Category     string    `db:"category" json:"category"`
	Status       int16     `db:"status" json:"status"`
	ReceivedAt   time.Time `db:"received_at" json:"received_at"`
}

type RunDetailView struct {
	RunView
	Steps     []RunStepView     `json:"steps"`
	Approvals []RunApprovalView `json:"approvals"`
}

const runColumns = `id::text AS id, api_key_id::text AS api_key_id, project_id::text AS project_id,
 revision_id::text AS revision_id, route_revision_id::text AS route_revision_id, created_at`

func (s *Server) listRuns(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	project := r.URL.Query().Get("project_id")
	if project != "" {
		id, err := uuid.Parse(project)
		if err != nil || id == uuid.Nil || id.String() != project {
			return access.Reply{}, access.Invalid("project_id", "Use a canonical project UUID.")
		}
		if err := p.Project(&project, access.View); err != nil {
			return access.Reply{}, err
		}
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT `+runColumns+` FROM olp.agent_runs
 WHERE id<$1 AND ($2 OR project_id=ANY($3::uuid[])) AND ($4='' OR project_id=NULLIF($4,'')::uuid)
 ORDER BY id DESC LIMIT $5`, page.Before, p.AllProjects, p.ProjectIDs(), project, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByName[RunView])
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item RunView) string { return item.ID }), nil
}

func (s *Server) visibleRun(r *http.Request, p access.Principal) (RunView, error) {
	id, err := access.IDParam(r, "run_id")
	if err != nil {
		return RunView{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT `+runColumns+` FROM olp.agent_runs WHERE id=$1`, id)
	if err != nil {
		return RunView{}, err
	}
	run, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[RunView])
	if err != nil {
		return RunView{}, err
	}
	if err := p.Project(&run.ProjectID, access.View); err != nil {
		return RunView{}, err
	}
	return run, nil
}

func (s *Server) getRun(r *http.Request, p access.Principal) (access.Reply, error) {
	run, err := s.visibleRun(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	stepRows, err := s.Access.Pool.Query(r.Context(), `SELECT s.id::text AS id, s.dispatch_id::text AS dispatch_id, s.ordinal,
 d.kind,d.state,d.currency,d.cost_bound::text AS cost_bound,d.charge_bound::text AS charge_bound,
 d.observed_cost::text AS observed_cost,d.observed_charge::text AS observed_charge,
 (CASE WHEN d.closed_at IS NOT NULL THEN 0 ELSE GREATEST(0,d.cost_bound-d.observed_cost) END)::text AS remaining_cost,
 (CASE WHEN d.closed_at IS NOT NULL THEN 0 ELSE GREATEST(0,d.charge_bound-d.observed_charge) END)::text AS remaining_charge,
 d.bound_violation,c.tool_alias,c.status AS tool_status,c.is_error AS tool_is_error,c.pricing_state,d.created_at
 FROM olp.agent_steps s JOIN olp.execution_debits d ON d.id=s.dispatch_id
 LEFT JOIN olp.mcp_tool_calls c ON c.dispatch_id=s.dispatch_id
 WHERE s.run_id=$1 ORDER BY s.ordinal`, run.ID)
	if err != nil {
		return access.Reply{}, err
	}
	steps, err := pgx.CollectRows(stepRows, pgx.RowToStructByName[RunStepView])
	if err != nil {
		return access.Reply{}, err
	}
	approvalRows, err := s.Access.Pool.Query(r.Context(), `SELECT id::text AS id,expires_at,consumed_at,dispatch_id::text AS dispatch_id
 FROM olp.agent_approvals WHERE run_id=$1 ORDER BY expires_at,id`, run.ID)
	if err != nil {
		return access.Reply{}, err
	}
	approvals, err := pgx.CollectRows(approvalRows, pgx.RowToStructByName[RunApprovalView])
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(RunDetailView{RunView: run, Steps: append([]RunStepView{}, steps...), Approvals: append([]RunApprovalView{}, approvals...)}), nil
}

func (s *Server) listRunSpans(r *http.Request, p access.Principal) (access.Reply, error) {
	run, err := s.visibleRun(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	spanID := r.URL.Query().Get("span_id")
	if spanID != "" {
		id, err := uuid.Parse(spanID)
		if err != nil || id == uuid.Nil || id.String() != spanID {
			return access.Reply{}, access.Invalid("span_id", "Use a canonical span UUID.")
		}
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 200 {
			return access.Reply{}, access.Invalid("limit", "Use a page size from 1 to 200.")
		}
	}
	var beforeTime, beforeID any
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(raw)
		timestamp, id, found := strings.Cut(string(decoded), "|")
		_, timeErr := strconv.ParseUint(timestamp, 10, 64)
		parsedID, idErr := uuid.Parse(id)
		if decodeErr != nil || !found || timeErr != nil || idErr != nil {
			return access.Reply{}, access.Invalid("cursor", "Invalid pagination cursor.")
		}
		beforeTime, beforeID = timestamp, parsedID.String()
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT id::text AS id,encode(trace_id,'hex') AS trace_id,
 encode(span_id,'hex') AS span_id,encode(parent_span_id,'hex') AS parent_span_id,attempt_id::text AS attempt_id,
 started_nanos::text AS started_nanos,ended_nanos::text AS ended_nanos,category,status,received_at
 FROM olp.trace_spans WHERE run_id=$1 AND ($2::numeric IS NULL OR (started_nanos,id)<($2::numeric,$3::uuid))
 AND ($5='' OR id=NULLIF($5,'')::uuid)
 ORDER BY started_nanos DESC,id DESC LIMIT $4`, run.ID, beforeTime, beforeID, limit+1, spanID)
	if err != nil {
		return access.Reply{}, err
	}
	spans, err := pgx.CollectRows(rows, pgx.RowToStructByName[RunSpanView])
	if err != nil {
		return access.Reply{}, err
	}
	var next any
	if len(spans) > limit {
		spans = spans[:limit]
		last := spans[len(spans)-1]
		next = base64.RawURLEncoding.EncodeToString([]byte(last.StartedNanos + "|" + last.ID))
	}
	return access.OK(map[string]any{"items": append([]RunSpanView{}, spans...), "next_cursor": next}), nil
}
