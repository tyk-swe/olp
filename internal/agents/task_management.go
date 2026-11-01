package agents

import (
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type TaskView struct {
	ID                string    `db:"id" json:"id"`
	ProjectID         string    `db:"project_id" json:"project_id"`
	APIKeyID          string    `db:"api_key_id" json:"api_key_id"`
	PublicationID     string    `db:"publication_id" json:"publication_id"`
	RevisionID        string    `db:"revision_id" json:"revision_id"`
	AgentPrincipalID  *string   `db:"agent_principal_id" json:"agent_principal_id"`
	MemberPrincipalID *string   `db:"member_principal_id" json:"member_principal_id"`
	DelegationPairID  *string   `db:"delegation_pair_id" json:"delegation_pair_id"`
	State             string    `db:"state" json:"state"`
	CallbackAttempts  *int      `db:"callback_attempts" json:"callback_attempts"`
	CreatedAt         time.Time `db:"created_at" json:"created_at"`
	DeadlineAt        time.Time `db:"deadline_at" json:"deadline_at"`
}

type TaskRemoteFee struct {
	DispatchID      string    `db:"dispatch_id" json:"dispatch_id"`
	RunID           string    `db:"run_id" json:"run_id"`
	State           string    `db:"state" json:"state"`
	Currency        string    `db:"currency" json:"currency"`
	ObservedCost    string    `db:"observed_cost" json:"observed_cost"`
	ObservedCharge  string    `db:"observed_charge" json:"observed_charge"`
	RemainingCost   string    `db:"remaining_cost" json:"remaining_cost"`
	RemainingCharge string    `db:"remaining_charge" json:"remaining_charge"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
}

type TaskDetail struct {
	TaskView
	RemoteFees     []TaskRemoteFee     `json:"remote_fees"`
	CallbackModels []TaskModelCallback `json:"callback_models"`
	CallbackTools  []TaskToolCallback  `json:"callback_tools"`
}

type TaskModelCallback struct {
	RequestID       string  `db:"request_id" json:"request_id"`
	AttemptID       *string `db:"attempt_id" json:"attempt_id"`
	State           string  `db:"state" json:"state"`
	Currency        *string `db:"currency" json:"currency"`
	ObservedCost    *string `db:"observed_cost" json:"observed_cost"`
	ObservedCharge  *string `db:"observed_charge" json:"observed_charge"`
	RemainingCost   *string `db:"remaining_cost" json:"remaining_cost"`
	RemainingCharge *string `db:"remaining_charge" json:"remaining_charge"`
}

type TaskToolCallback struct {
	DispatchID      string  `db:"dispatch_id" json:"dispatch_id"`
	RunID           string  `db:"run_id" json:"run_id"`
	State           string  `db:"state" json:"state"`
	PricingState    string  `db:"pricing_state" json:"pricing_state"`
	Currency        string  `db:"currency" json:"currency"`
	ObservedCost    *string `db:"observed_cost" json:"observed_cost"`
	ObservedCharge  *string `db:"observed_charge" json:"observed_charge"`
	RemainingCost   *string `db:"remaining_cost" json:"remaining_cost"`
	RemainingCharge *string `db:"remaining_charge" json:"remaining_charge"`
}

const taskColumns = `id::text AS id, project_id::text AS project_id, api_key_id::text AS api_key_id,
 publication_id::text AS publication_id, revision_id::text AS revision_id,
 delegation_agent_id::text AS agent_principal_id, delegation_member_id::text AS member_principal_id,
 NULLIF(delegation_pair_digest,'') AS delegation_pair_id, state, callback_attempts, created_at, deadline_at`

func projectFilter(r *http.Request, p access.Principal) (string, error) {
	project := r.URL.Query().Get("project_id")
	if project == "" {
		return "", nil
	}
	id, err := uuid.Parse(project)
	if err != nil || id == uuid.Nil || id.String() != project {
		return "", access.Invalid("project_id", "Use a canonical project UUID.")
	}
	if err := p.Project(&project, access.View); err != nil {
		return "", err
	}
	return project, nil
}

func (s *Server) listTasks(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	project, err := projectFilter(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT `+taskColumns+` FROM olp.a2a_tasks
 WHERE id<$1 AND ($2 OR project_id=ANY($3::uuid[])) AND ($4='' OR project_id=NULLIF($4,'')::uuid)
 ORDER BY id DESC LIMIT $5`, page.Before, p.AllProjects, p.ProjectIDs(), project, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := pgx.CollectRows(rows, pgx.RowToStructByName[TaskView])
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item TaskView) string { return item.ID }), nil
}

func (s *Server) getTask(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "task_id")
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := s.Access.Pool.Query(r.Context(), `SELECT `+taskColumns+` FROM olp.a2a_tasks WHERE id=$1`, id)
	if err != nil {
		return access.Reply{}, err
	}
	task, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[TaskView])
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(&task.ProjectID, access.View); err != nil {
		return access.Reply{}, err
	}
	feeRows, err := s.Access.Pool.Query(r.Context(), `SELECT d.id::text AS dispatch_id,d.run_id::text AS run_id,d.state,d.currency,
 d.observed_cost::text AS observed_cost,d.observed_charge::text AS observed_charge,
 (CASE WHEN d.closed_at IS NOT NULL THEN 0 ELSE GREATEST(0,d.cost_bound-d.observed_cost) END)::text AS remaining_cost,
 (CASE WHEN d.closed_at IS NOT NULL THEN 0 ELSE GREATEST(0,d.charge_bound-d.observed_charge) END)::text AS remaining_charge,d.created_at
 FROM olp.execution_debits d WHERE d.kind='agent' AND (d.id=(SELECT dispatch_id FROM olp.a2a_tasks WHERE id=$1)
 OR EXISTS(SELECT 1 FROM olp.a2a_task_capabilities c WHERE c.dispatch_id=d.id AND c.task_id=$1)) ORDER BY d.created_at,d.id`, id)
	if err != nil {
		return access.Reply{}, err
	}
	fees, err := pgx.CollectRows(feeRows, pgx.RowToStructByName[TaskRemoteFee])
	if err != nil {
		return access.Reply{}, err
	}
	modelRows, err := s.Access.Pool.Query(r.Context(), `SELECT c.accounting_id::text AS request_id,f.attempt_id::text AS attempt_id,
 CASE WHEN f.attempt_id IS NULL THEN 'pending' WHEN f.charge_status='not_billable' THEN 'not_billable'
 WHEN NOT f.usage_complete OR f.charge_status='billing_uncertain' THEN 'outcome_unknown'
 WHEN f.unpriced OR f.estimated_cost IS NULL THEN 'unpriced' WHEN f.charge IS NULL THEN 'uncharged'
 WHEN f.estimated_cost=0 AND f.charge=0 THEN 'free' ELSE 'priced' END AS state,
 btrim(f.currency)::text AS currency,f.estimated_cost::text AS observed_cost,f.charge::text AS observed_charge,
 CASE WHEN f.attempt_id IS NOT NULL AND f.usage_complete AND f.charge_status<>'billing_uncertain' AND (f.charge_status='not_billable' OR NOT f.unpriced) THEN '0' END AS remaining_cost,
 CASE WHEN f.attempt_id IS NOT NULL AND f.usage_complete AND f.charge_status<>'billing_uncertain' AND (f.charge_status='not_billable' OR f.charge IS NOT NULL) THEN '0' END AS remaining_charge
 FROM olp.a2a_task_callback_accounting c LEFT JOIN olp.attempt_usage_facts f ON f.request_id=c.accounting_id
 WHERE c.kind='inference' AND (c.dispatch_id=(SELECT dispatch_id FROM olp.a2a_tasks WHERE id=$1)
 OR EXISTS(SELECT 1 FROM olp.a2a_task_capabilities cap WHERE cap.dispatch_id=c.dispatch_id AND cap.task_id=$1))
 ORDER BY c.created_at,c.accounting_id,f.attempt_ordinal`, id)
	if err != nil {
		return access.Reply{}, err
	}
	models, err := pgx.CollectRows(modelRows, pgx.RowToStructByName[TaskModelCallback])
	if err != nil {
		return access.Reply{}, err
	}
	toolRows, err := s.Access.Pool.Query(r.Context(), `SELECT d.id::text AS dispatch_id,d.run_id::text AS run_id,d.state,
 COALESCE(m.pricing_state,'unknown') AS pricing_state,d.currency,
 CASE WHEN m.pricing_state IN ('priced','free') THEN d.observed_cost::text END AS observed_cost,
 CASE WHEN m.pricing_state IN ('priced','free') THEN d.observed_charge::text END AS observed_charge,
 (CASE WHEN d.closed_at IS NOT NULL THEN 0 ELSE GREATEST(0,d.cost_bound-d.observed_cost) END)::text AS remaining_cost,
 (CASE WHEN d.closed_at IS NOT NULL THEN 0 ELSE GREATEST(0,d.charge_bound-d.observed_charge) END)::text AS remaining_charge
 FROM olp.a2a_task_callback_accounting c JOIN olp.execution_debits d ON d.id=c.accounting_id
 LEFT JOIN olp.mcp_tool_calls m ON m.dispatch_id=d.id
 WHERE c.kind='tools' AND (c.dispatch_id=(SELECT dispatch_id FROM olp.a2a_tasks WHERE id=$1)
 OR EXISTS(SELECT 1 FROM olp.a2a_task_capabilities cap WHERE cap.dispatch_id=c.dispatch_id AND cap.task_id=$1))
 ORDER BY c.created_at,c.accounting_id`, id)
	if err != nil {
		return access.Reply{}, err
	}
	tools, err := pgx.CollectRows(toolRows, pgx.RowToStructByName[TaskToolCallback])
	if err != nil {
		return access.Reply{}, err
	}
	return access.OK(TaskDetail{TaskView: task, RemoteFees: append([]TaskRemoteFee{}, fees...), CallbackModels: append([]TaskModelCallback{}, models...), CallbackTools: append([]TaskToolCallback{}, tools...)}), nil
}
