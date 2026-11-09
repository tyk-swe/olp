package focus

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/management/contract"
)

const datasetsSQL = `SELECT jsonb_build_object('charge_date',fd.charge_date::text,'revision',fd.revision,
 'generated_at',fd.generated_at,'delivered_at',fd.delivered_at,'row_count',fd.row_count,'part_count',fd.part_count,
 'billed_cost',trim_scale(fd.billed_cost)::text,'effective_cost',trim_scale(fd.effective_cost)::text,
 'commitment_totals',fd.commitment_totals,'attempts',fd.attempts,'last_error_code',fd.last_error_code,
 'current',fd.delivered_at IS NOT NULL AND fd.dirty_version=COALESCE(dd.version,0))
 FROM olp.focus_datasets fd LEFT JOIN olp.focus_dirty_days dd ON dd.day=fd.charge_date
 WHERE fd.sink_id=$1::uuid AND fd.charge_date < $2::date ORDER BY fd.charge_date DESC LIMIT $3`

type API struct {
	server *access.Server
}

func NewAPI(server *access.Server) *API {
	return &API{server: server}
}

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/observability/sinks/{export_sink_id}/focus-datasets", a.datasets)
}

func (a *API) datasets(r *http.Request, p access.Principal) (access.Reply, error) {
	sinkID := r.PathValue("export_sink_id")
	projectID, err := destination.SinkProject(r.Context(), a.server.Pool, sinkID)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(projectID, access.View); err != nil {
		return access.Reply{}, err
	}
	page, err := access.PageBy(r, "9999-12-31", func(raw string) (string, error) {
		_, err := time.Parse(time.DateOnly, raw)
		return raw, err
	})
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := a.server.Pool.Query(r.Context(), datasetsSQL, sinkID, page.Before, page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (contract.FocusDataset, error) {
		var body []byte
		var item contract.FocusDataset
		if err := row.Scan(&body); err != nil {
			return item, err
		}
		return item, json.Unmarshal(body, &item)
	})
	if err != nil {
		return access.Reply{}, err
	}
	return access.ListReplyBy(items, page, func(item contract.FocusDataset) string { return item.ChargeDate.String() }), nil
}
