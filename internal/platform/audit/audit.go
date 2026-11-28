package audit

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/domain/ids"
)

type Entry struct {
	UserID          string
	TokenID         string
	Action          string
	ResourceType    string
	ResourceID      string
	Outcome         string
	SourceIP        string
	UserAgentFamily string
	ProjectID       *string
}

func Insert(ctx context.Context, tx pgx.Tx, entry Entry) error {
	_, err := tx.Exec(ctx, insertSQL, entry.values()...)
	return err
}

func InsertInResourceProject(ctx context.Context, tx pgx.Tx, entry Entry) error {
	_, err := tx.Exec(ctx, insertInResourceProjectSQL, entry.values()...)
	return err
}

func Purge(ctx context.Context, tx pgx.Tx, before time.Time, limit int) (int64, error) {
	tag, err := tx.Exec(ctx, purgeSQL, before, limit)
	return tag.RowsAffected(), err
}

func (e Entry) values() []any {
	return []any{ids.New(), optional(e.UserID), optional(e.TokenID), e.Action, e.ResourceType, e.ResourceID, e.Outcome, optional(e.SourceIP), e.UserAgentFamily, e.ProjectID}
}

func optional(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
