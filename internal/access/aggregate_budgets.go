package access

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/limits"
)

// BudgetPolicy caps aggregate invoiced inference spend. Nil dimensions are unlimited.
type BudgetPolicy struct {
	DailyCostLimit   *string `json:"daily_cost_limit"`
	MonthlyCostLimit *string `json:"monthly_cost_limit"`
	WeeklyCostLimit  *string `json:"weekly_cost_limit"`
}

func (p *BudgetPolicy) Limited() bool {
	return p != nil && (p.DailyCostLimit != nil || p.MonthlyCostLimit != nil || p.WeeklyCostLimit != nil)
}
func (p *BudgetPolicy) Validate() error {
	if p == nil {
		return nil
	}
	return (AdmissionLimits{DailyCostLimit: p.DailyCostLimit, MonthlyCostLimit: p.MonthlyCostLimit, WeeklyCostLimit: p.WeeklyCostLimit}).Validate()
}

func (s *Server) projectBudget(r *http.Request, p Principal) (Reply, error) {
	return s.readAggregateBudget(r, p, true)
}
func (s *Server) installationBudget(r *http.Request, p Principal) (Reply, error) {
	return s.readAggregateBudget(r, p, false)
}
func (s *Server) putProjectBudget(r *http.Request, p Principal) (Reply, error) {
	return s.writeAggregateBudget(r, p, true)
}
func (s *Server) putInstallationBudget(r *http.Request, p Principal) (Reply, error) {
	return s.writeAggregateBudget(r, p, false)
}

// Table names are selected internally, never supplied by the request.
func (s *Server) budgetBoundary(r *http.Request, p Principal, project, change bool) (id, table, etagColumn, level string, err error) {
	id, table, etagColumn, level = s.Installation, "olp.installation", "budget_etag", "installation"
	if r.PathValue("organization_id") != "" {
		table, etagColumn, level = "olp.organizations", "etag", "organization"
		id, err = IDParam(r, "organization_id")
		if err != nil {
			return
		}
		need := View
		if change {
			need = Change
		}
		err = p.Organization(id, need)
		return
	}
	if !project {
		return
	}
	table, etagColumn, level = "olp.projects", "etag", "project"
	id, err = IDParam(r, "project_id")
	if err != nil {
		return
	}
	need := View
	if change {
		need = Change
	}
	err = p.Project(&id, need)
	return
}
func (s *Server) readAggregateBudget(r *http.Request, p Principal, project bool) (Reply, error) {
	id, table, column, level, err := s.budgetBoundary(r, p, project, false)
	if err != nil {
		return Reply{}, err
	}
	var policy, usage json.RawMessage
	var etag string
	err = s.Pool.QueryRow(r.Context(), "SELECT COALESCE(budget_policy,'null'::jsonb),"+column+"::text,"+aggregateReportSQL(level)+" FROM "+table+" p WHERE id=$1", id).Scan(&policy, &etag, &usage)
	return Detail(map[string]any{"policy": policy, "etag": etag, "usage": usage}, etag), err
}
func (s *Server) writeAggregateBudget(r *http.Request, _ Principal, project bool) (Reply, error) {
	var body struct {
		Policy json.RawMessage `json:"policy"`
	}
	if err := Decode(r, &body); err != nil {
		return Reply{}, err
	}
	if len(body.Policy) == 0 {
		return Reply{}, Invalid("policy", "A policy or explicit null is required.")
	}
	var policy *BudgetPolicy
	d := json.NewDecoder(bytes.NewReader(body.Policy))
	d.DisallowUnknownFields()
	if err := d.Decode(&policy); err != nil {
		return Reply{}, Invalid("policy", "The budget policy is invalid.")
	}
	if err := policy.Validate(); err != nil {
		return Reply{}, err
	}
	if !policy.Limited() {
		policy = nil
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
	id, table, column, level, err := s.budgetBoundary(r, p, project, true)
	if err != nil {
		return Reply{}, err
	}
	var etag string
	if err = tx.QueryRow(r.Context(), "SELECT "+column+"::text FROM "+table+" WHERE id=$1 FOR UPDATE", id).Scan(&etag); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	if policy.Limited() {
		if err = limits.EnsureAggregateBudget(r.Context(), tx, level, id); err != nil {
			return Reply{}, err
		}
	}
	encoded, err := json.Marshal(policy)
	if err != nil {
		return Reply{}, err
	}
	etag = NewID()
	extra := ""
	if level != "installation" {
		extra = ",updated_at=now()"
	}
	if _, err = tx.Exec(r.Context(), "UPDATE "+table+" SET budget_policy=NULLIF($2::jsonb,'null'::jsonb),"+column+"=$3"+extra+" WHERE id=$1", id, encoded, etag); err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), level+".budget.update", level, id, "success"); err != nil {
		return Reply{}, err
	}
	var usage json.RawMessage
	if err = tx.QueryRow(r.Context(), "SELECT "+aggregateReportSQL(level)+" FROM "+table+" p WHERE id=$1", id).Scan(&usage); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(map[string]any{"policy": policy, "etag": etag, "usage": usage}, etag))
}

func aggregateReportSQL(level string) string {
	if level == "organization" {
		return limits.OrganizationBudgetSQL
	}
	return limits.AggregateBudgetSQL(level == "project")
}
