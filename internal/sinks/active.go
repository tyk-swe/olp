package sinks

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type ActiveSink struct {
	ID           string
	CredentialID *string
	Name         string
	Project      *string
	Destination  string
	Streams      []string
	Enabled      bool
}

func ReadActiveSinks(ctx context.Context, q access.Queryer) ([]ActiveSink, error) {
	rows, err := q.Query(ctx, "SELECT s.id::text,s.credential_id::text,s.name,p.name,s.destination,s.streams,s.enabled FROM olp.managed_export_sinks s LEFT JOIN olp.projects p ON p.id=s.project_id WHERE s.retired_at IS NULL")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[ActiveSink])
}
