package destination

import (
	"context"
	"encoding/json"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Filter struct {
	Project *string                                        `json:"project,omitempty"`
	Route   *string                                        `json:"route,omitempty"`
	Outcome *contract.CreateExportSinkRequestFilterOutcome `json:"outcome,omitempty"`
}

type Gap struct {
	ID          string `json:"id"`
	Stream      string `json:"stream"`
	RecordCount int64  `json:"record_count"`
	FirstAt     string `json:"first_occurred_at"`
	LastAt      string `json:"last_occurred_at"`
	Reason      string `json:"reason"`
}

type stored struct {
	ID           string
	Name         string
	ProjectID    *string
	Type         Type
	Destination  string
	Streams      []Stream
	Format       Format
	Filter       Filter
	Settings     json.RawMessage
	Enabled      bool
	ETag         string
	CredentialID *string
}

const statusJSON = `(SELECT COALESCE(jsonb_agg(CASE WHEN st.stream='focus' THEN
  jsonb_build_object('stream','focus','delivered_days',COALESCE(focusd.delivered_days,0),
   'latest_delivered_date',focusd.latest_delivered_date,'last_success_at',focusd.last_success_at,
   'pending_error_codes',COALESCE(focusd.error_codes,'{}'::text[]))
 ELSE jsonb_build_object('stream',st.stream,'cursor',c.cursor,'delivered_total',COALESCE(c.delivered_total,0),
 'failures_total',COALESCE(c.failed_total,0),'gaps_total',COALESCE(c.gaps_total,0),'last_success_at',c.last_success_at,
 'last_attempt_at',pending.last_attempt_at,'pending',COALESCE(pending.pending,0),'oldest_pending_at',pending.oldest_pending_at,
 'pending_error_codes',COALESCE(pending.error_codes,'{}'::text[])) END ORDER BY st.stream),'[]'::jsonb)
 FROM unnest(s.streams) st(stream)
 LEFT JOIN olp.export_cursors c ON c.sink_id=s.id AND c.stream=st.stream
 LEFT JOIN LATERAL (SELECT count(*) FILTER(WHERE p.delivered_at IS NULL AND p.rejected_at IS NULL) AS pending,
 min(r.queued_at) FILTER(WHERE p.delivered_at IS NULL AND p.rejected_at IS NULL) AS oldest_pending_at,max(p.last_attempt_at) AS last_attempt_at,
 array_agg(DISTINCT p.last_error_code) FILTER(WHERE p.delivered_at IS NULL AND p.rejected_at IS NULL AND p.last_error_code IS NOT NULL) AS error_codes
 FROM olp.export_pending p JOIN olp.export_records r ON r.id=p.record_id WHERE p.sink_id=s.id AND r.stream=st.stream) pending ON true
 LEFT JOIN LATERAL (SELECT count(*) FILTER(WHERE fd.delivered_at IS NOT NULL) AS delivered_days,
 max(fd.charge_date) FILTER(WHERE fd.delivered_at IS NOT NULL) AS latest_delivered_date,
 array_agg(DISTINCT fd.last_error_code) FILTER(WHERE fd.last_error_code IS NOT NULL) AS error_codes,
 max(fd.delivered_at) AS last_success_at
 FROM olp.focus_datasets fd WHERE fd.sink_id=s.id AND st.stream='focus') focusd ON true)`

const sinkJSON = `jsonb_build_object('export_sink_id',s.id,'name',s.name,'project_id',s.project_id,'type',s.type,
 'destination',s.destination,'streams',s.streams,'format',s.format,'filter',s.filter,'settings',s.settings,'enabled',s.enabled,
 'credential_configured',s.credential_id IS NOT NULL,'etag',s.etag,'status',` + statusJSON + `)`

const (
	listSinksSQL = `SELECT s.id::text,` + sinkJSON + ` FROM olp.export_sinks s
 WHERE s.id<$1::uuid AND ($2 OR s.project_id=ANY($3::uuid[])) ORDER BY s.id DESC LIMIT $4`
	sinkProjectSQL = `SELECT project_id::text FROM olp.export_sinks WHERE id=$1`
	getSinkSQL     = `SELECT ` + sinkJSON + `,s.etag::text,s.project_id::text FROM olp.export_sinks s WHERE s.id=$1`
	lockSinkSQL    = `SELECT id::text,name,project_id::text,type,destination,streams,format,filter,settings,enabled,etag::text,credential_id::text
 FROM olp.export_sinks WHERE id=$1 FOR UPDATE`
	createSinkSQL = `INSERT INTO olp.export_sinks(id,name,project_id,type,destination,streams,format,filter,settings,enabled,etag,created_by)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	updateSinkSQL       = `UPDATE olp.export_sinks SET name=$2,destination=$3,filter=$4,enabled=$5,etag=$6,settings=$7,updated_at=now() WHERE id=$1`
	setCredentialSQL    = `UPDATE olp.export_sinks SET credential_id=$2 WHERE id=$1`
	deleteCredentialSQL = `DELETE FROM olp.secrets WHERE id=$1 AND purpose=$2`
	deleteSinkSQL       = `DELETE FROM olp.export_sinks WHERE id=$1`
	countSinksSQL       = `SELECT count(*) FROM olp.export_sinks`
	missingProjectSQL   = `SELECT k::text FROM unnest($1::uuid[]) k WHERE NOT EXISTS(SELECT 1 FROM olp.projects p WHERE p.id=k) ORDER BY k LIMIT 1`
	listGapsSQL         = `SELECT to_jsonb(g) FROM olp.export_gaps g JOIN olp.export_sinks s ON s.id=g.sink_id
 WHERE g.sink_id=$1 AND g.id<$2::uuid AND ($3 OR s.project_id=ANY($4::uuid[])) ORDER BY g.id DESC LIMIT $5`
)

type listed struct {
	id   string
	body json.RawMessage
}

func listSinks(ctx context.Context, q access.Queryer, page access.Pagination, p access.Principal) ([]listed, error) {
	rows, err := q.Query(ctx, listSinksSQL, page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []listed
	for rows.Next() {
		var item listed
		if err := rows.Scan(&item.id, &item.body); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func readSink(ctx context.Context, q access.Queryer, id string) (json.RawMessage, string, *string, error) {
	var body json.RawMessage
	var etag string
	var projectID *string
	if err := q.QueryRow(ctx, getSinkSQL, id).Scan(&body, &etag, &projectID); err != nil {
		return nil, "", nil, access.NotFoundOr(err)
	}
	return body, etag, projectID, nil
}

func SinkProject(ctx context.Context, q access.Queryer, id string) (*string, error) {
	var projectID *string
	err := q.QueryRow(ctx, sinkProjectSQL, id).Scan(&projectID)
	return projectID, access.NotFoundOr(err)
}

func lockSink(ctx context.Context, q access.Queryer, id string) (stored, error) {
	var sink stored
	err := q.QueryRow(ctx, lockSinkSQL, id).Scan(&sink.ID, &sink.Name, &sink.ProjectID, &sink.Type, &sink.Destination,
		&sink.Streams, &sink.Format, &sink.Filter, &sink.Settings, &sink.Enabled, &sink.ETag, &sink.CredentialID)
	return sink, access.NotFoundOr(err)
}

func listGaps(ctx context.Context, q access.Queryer, sinkID string, page access.Pagination, p access.Principal) ([]Gap, error) {
	rows, err := q.Query(ctx, listGapsSQL, sinkID, page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var gaps []Gap
	for rows.Next() {
		var gap Gap
		if err := rows.Scan(&gap); err != nil {
			return nil, err
		}
		gaps = append(gaps, gap)
	}
	return gaps, rows.Err()
}

func ReadCredential(ctx context.Context, q secrets.RowQuerier, keys *secrets.KeyRing, installation string, id *string) ([]byte, error) {
	if id == nil {
		return nil, nil
	}
	return keys.Read(ctx, q, installation, *id, secrets.SinkCredential)
}

func ProjectExists(ctx context.Context, q access.Queryer, id string) (bool, error) {
	var exists bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.projects WHERE id=$1)`, id).Scan(&exists)
	return exists, err
}

func RouteProject(ctx context.Context, q access.Queryer, slug string) (*string, error) {
	var project *string
	err := q.QueryRow(ctx, `SELECT project_id::text FROM olp.routes WHERE slug=$1 FOR SHARE`, slug).Scan(&project)
	return project, err
}
