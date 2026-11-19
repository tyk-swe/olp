package budgets

import (
	"net/http"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
)

// executionDebits reports financial exposure, not invoice charges. Accounting
// facts and all existing spend/budget endpoints keep their original meaning.
func (a *API) debits(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	key := r.URL.Query().Get("api_key_id")
	if key != "" && !debitOwnerUUID(key) {
		return httpkit.Reply{}, httpkit.Invalid("api_key_id", "Use a canonical API key UUID.")
	}
	rows, err := a.server.Pool.Query(r.Context(), `SELECT id::text,api_key_id::text,project_id::text,run_id::text,kind,state,currency,cost_bound::text,charge_bound::text,observed_cost::text,observed_charge::text,CASE WHEN closed_at IS NOT NULL THEN 0 ELSE GREATEST(0,cost_bound-observed_cost) END::text,CASE WHEN closed_at IS NOT NULL THEN 0 ELSE GREATEST(0,charge_bound-observed_charge) END::text,bound_violation,to_char(created_at AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM olp.execution_debits WHERE id<$1 AND ($2 OR project_id=ANY($3::uuid[])) AND ($4='' OR api_key_id=NULLIF($4,'')::uuid) ORDER BY id DESC LIMIT $5`, page.Before, p.AllProjects, p.ProjectIDs(), key, page.Limit+1)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, key, kind, state, currency, cost, charge, observedCost, observedCharge, remainingCost, remainingCharge, created string
		var run, project *string
		var violation bool
		if err = rows.Scan(&id, &key, &project, &run, &kind, &state, &currency, &cost, &charge, &observedCost, &observedCharge, &remainingCost, &remainingCharge, &violation, &created); err != nil {
			return httpkit.Reply{}, err
		}
		items = append(items, map[string]any{"id": id, "api_key_id": key, "project_id": project, "run_id": run, "kind": kind, "state": state, "currency": currency, "cost_bound": cost, "charge_bound": charge, "observed_cost": observedCost, "observed_charge": observedCharge, "remaining_cost": remainingCost, "remaining_charge": remainingCharge, "bound_violation": violation, "created_at": created})
	}
	return httpkit.ListReply(items, page), rows.Err()
}

func debitOwnerUUID(raw string) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw
}
