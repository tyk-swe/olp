package access

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/limits"
)

// AttributionBudgets bounds allocated spend for up to 64 project label/value pairs.
// Callers may choose unpinned labels; such caps are allocation controls, not identity.
type AttributionBudgets map[string]map[string]BudgetPolicy

func (b AttributionBudgets) Validate() error {
	count := 0
	for key, values := range b {
		if !attribution.KeyPattern.MatchString(key) || len(values) == 0 {
			return Invalid("budgets", "Use valid label keys with at least one budgeted value.")
		}
		for value, policy := range values {
			count++
			if !attribution.ValuePattern.MatchString(value) || !policy.Limited() {
				return Invalid("budgets", "Use machine-token values with at least one positive cost cap.")
			}
			if err := policy.Validate(); err != nil {
				return err
			}
		}
	}
	if count > 64 {
		return Invalid("budgets", "Configure at most 64 label/value budgets per project.")
	}
	return nil
}

func (b AttributionBudgets) JSON() []byte {
	if b == nil {
		return []byte("{}")
	}
	data, _ := json.Marshal(b)
	return data
}

func (b AttributionBudgets) EnsureAccounts(ctx context.Context, tx pgx.Tx, project string) error {
	for key, values := range b {
		for value := range values {
			if err := limits.EnsureAttributionBudget(ctx, tx, project, key, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) projectAttributionBudgets(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	if err = p.Project(&id, View); err != nil {
		return Reply{}, err
	}
	return readAttributionBudgets(r, s.Pool, id)
}

func readAttributionBudgets(r *http.Request, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) (Reply, error) {
	var budgets, usage json.RawMessage
	var etag string
	if err := q.QueryRow(r.Context(), `SELECT attribution_budgets,etag::text,`+limits.AttributionBudgetsSQL+` FROM olp.projects p WHERE id=$1`, id).Scan(&budgets, &etag, &usage); err != nil {
		return Reply{}, err
	}
	return Detail(map[string]any{"budgets": budgets, "usage": usage, "etag": etag}, etag), nil
}

func (s *Server) putProjectAttributionBudgets(r *http.Request, _ Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	var input struct {
		Budgets AttributionBudgets `json:"budgets"`
	}
	if err = Decode(r, &input); err != nil {
		return Reply{}, err
	}
	if input.Budgets == nil {
		return Reply{}, Invalid("budgets", "Supply a budget map; an empty object clears caps without resetting spend.")
	}
	if err = input.Budgets.Validate(); err != nil {
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
	if err = p.Project(&id, Change); err != nil {
		return Reply{}, err
	}
	etag, err := loadProject(r, tx, id)
	if err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	if err = input.Budgets.EnsureAccounts(r.Context(), tx, id); err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), `UPDATE olp.projects SET attribution_budgets=$2::jsonb,etag=$3,updated_at=now() WHERE id=$1`, id, input.Budgets.JSON(), NewID()); err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "project.attribution_budgets.update", "project", id, "success"); err != nil {
		return Reply{}, err
	}
	reply, err := readAttributionBudgets(r, tx, id)
	if err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, reply)
}

func (b AttributionBudgets) Equal(other AttributionBudgets) bool {
	return bytes.Equal(b.JSON(), other.JSON())
}

// Matches avoids cost estimation for a request outside every allocation.
func (b AttributionBudgets) Matches(labels map[string]string) bool {
	if len(b) == 0 {
		return false
	}
	for key, value := range labels {
		if policy, ok := b[key][value]; ok && policy.Limited() {
			return true
		}
	}
	return false
}
