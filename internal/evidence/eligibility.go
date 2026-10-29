package evidence

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type Stream string

const (
	Runs       Stream = "runs"
	TraceSpans Stream = "trace_spans"
)

type creatorAccess struct {
	Scope       string  `db:"access_scope"`
	ProjectRole *string `db:"project_role"`
	SourceLive  bool    `db:"source_live"`
}

func sourceQuery(stream Stream) (string, error) {
	switch stream {
	case Runs:
		return `SELECT EXISTS (SELECT 1 FROM olp.agent_runs WHERE id=$2 AND project_id=$3 AND created_at >= clock_timestamp() - make_interval(days => COALESCE((SELECT value::integer FROM olp.settings WHERE key='retention.requests_days'), 30)))`, nil
	case TraceSpans:
		return `SELECT EXISTS (SELECT 1 FROM olp.trace_spans WHERE id=$2 AND project_id=$3 AND received_at >= clock_timestamp() - make_interval(days => COALESCE((SELECT value::integer FROM olp.settings WHERE key='retention.requests_days'), 30)))`, nil
	default:
		return "", errors.New("unsupported evidence stream")
	}
}

func projectMembership(projectID string, role *string) map[string]string {
	if role == nil {
		return nil
	}
	return map[string]string{projectID: *role}
}

func Eligible(ctx context.Context, q access.Queryer, stream Stream, sourceID, projectID, creatorID string) (bool, error) {
	source, err := sourceQuery(stream)
	if err != nil {
		return false, err
	}
	rows, err := q.Query(ctx, `SELECT u.access_scope, m.role AS project_role, (`+source+`) AS source_live
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
