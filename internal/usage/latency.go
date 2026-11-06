package usage

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

const routeLatencySQL = `SELECT rt.slug,rt.project_id::text,
 count(r.id) FILTER(WHERE CASE WHEN $3 THEN first_output.milliseconds ELSE r.total_latency_ms END IS NOT NULL),
 percentile_cont(0.95) WITHIN GROUP(ORDER BY CASE WHEN $3 THEN first_output.milliseconds ELSE r.total_latency_ms END)::text
 FROM olp.routes rt LEFT JOIN olp.requests r ON r.route_slug=rt.slug AND r.completed_at>=now()-make_interval(secs=>$1)
 AND r.origin='caller' AND ($2::uuid IS NULL OR ` + requestProjectExpression + `=$2)
 LEFT JOIN LATERAL (
 SELECT min(EXTRACT(epoch FROM a.started_at-r.started_at)*1000+(a.routing->>'first_output_ms')::numeric) AS milliseconds
 FROM olp.attempts a WHERE a.request_id=r.id AND a.request_started_at=r.started_at AND a.committed
 AND a.routing->>'first_output_ms' IS NOT NULL
 ) first_output ON true
 WHERE rt.state='active' AND ($2::uuid IS NULL OR rt.project_id=$2 OR rt.project_id IS NULL)
 GROUP BY rt.slug,rt.project_id ORDER BY rt.slug LIMIT 10001`

type RouteLatencyObservation struct {
	Route        string
	ProjectID    *string
	Samples      int64
	Milliseconds *string
}

func ReadRouteLatency(ctx context.Context, q access.Queryer, windowSeconds int, projectID *string, timeToFirstToken bool) ([]RouteLatencyObservation, error) {
	rows, err := q.Query(ctx, routeLatencySQL, windowSeconds, projectID, timeToFirstToken)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (RouteLatencyObservation, error) {
		var observation RouteLatencyObservation
		err := row.Scan(&observation.Route, &observation.ProjectID, &observation.Samples, &observation.Milliseconds)
		return observation, err
	})
}
