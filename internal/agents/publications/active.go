package publications

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/agentcontract"
)

type ActivePublication struct {
	ID        string
	ProjectID string
	Kind      string
	Draft     agentcontract.Draft
	Published *agentcontract.Draft
}

type PromptLabel struct {
	PublicationID string
	Label         string
	Definition    agentcontract.Draft
}

func ReadActive(ctx context.Context, q access.Queryer) ([]ActivePublication, error) {
	rows, err := q.Query(ctx, `SELECT p.id::text,p.project_id::text,p.kind,p.draft,v.definition FROM olp.agent_publications p LEFT JOIN olp.agent_publication_revisions v ON v.id=p.latest_revision_id WHERE NOT p.retired`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ActivePublication])
}

func ReadActivePromptLabels(ctx context.Context, q access.Queryer) ([]PromptLabel, error) {
	rows, err := q.Query(ctx, `SELECT l.publication_id::text,l.label,v.definition FROM olp.prompt_labels l JOIN olp.agent_publication_revisions v ON v.id=l.revision_id JOIN olp.agent_publications p ON p.id=l.publication_id WHERE NOT p.retired AND p.kind='prompt'`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[PromptLabel])
}

func ActiveID(ctx context.Context, q access.Queryer, projectID, kind, name string) (string, bool, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT id::text FROM olp.agent_publications WHERE NOT retired AND project_id=$1 AND kind=$2 AND lower(name)=lower($3)`, projectID, kind, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}
