package evidence

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/database"
)

type stream string

const (
	runs       stream = "runs"
	traceSpans stream = "trace_spans"
)

type creatorAccess struct {
	Scope       string  `db:"access_scope"`
	ProjectRole *string `db:"project_role"`
	SourceLive  bool    `db:"source_live"`
}

func sourceQuery(source stream) string {
	if source == traceSpans {
		return `SELECT EXISTS (SELECT 1 FROM olp.trace_spans WHERE id=$2 AND project_id=$3 AND received_at >= clock_timestamp() - make_interval(days => COALESCE((SELECT value::integer FROM olp.settings WHERE key='retention.requests_days'), 30)))`
	}
	return `SELECT EXISTS (SELECT 1 FROM olp.agent_runs WHERE id=$2 AND project_id=$3 AND created_at >= clock_timestamp() - make_interval(days => COALESCE((SELECT value::integer FROM olp.settings WHERE key='retention.requests_days'), 30)))`
}

func projectMembership(projectID string, role *string) map[string]string {
	if role == nil {
		return nil
	}
	return map[string]string{projectID: *role}
}

func eligible(ctx context.Context, q database.Queryer, source stream, sourceID, projectID, creatorID string) (bool, error) {
	rows, err := q.Query(ctx, `SELECT u.access_scope, m.role AS project_role, (`+sourceQuery(source)+`) AS source_live
		FROM olp.users u LEFT JOIN olp.effective_project_members m ON m.user_id=u.id AND m.project_id=$3
		WHERE u.id=$1 AND u.active AND u.oidc_authorized`, creatorID, sourceID, projectID)
	if err != nil {
		return false, err
	}
	current, err := pgx.CollectOneRow(rows, pgx.RowToStructByName[creatorAccess])
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	principal := access.Principal{AllProjects: current.Scope == "global", Projects: projectMembership(projectID, current.ProjectRole)}
	return current.SourceLive && principal.Project(&projectID, access.View) == nil, nil
}

func Admit(ctx context.Context, q database.Queryer, name string, sourceID, projectID *string, creatorID string) (bool, error) {
	if name != string(runs) && name != string(traceSpans) {
		return true, nil
	}
	if sourceID == nil || projectID == nil {
		return false, nil
	}
	return eligible(ctx, q, stream(name), *sourceID, *projectID, creatorID)
}
