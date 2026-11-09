package routes

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
)

type TemplateOwner struct {
	ID        string
	ProjectID *string
}

func ReadTemplateInputs(ctx context.Context, q access.Queryer) ([]TemplateInput, error) {
	stored, err := loadTemplates(ctx, q)
	if err != nil {
		return nil, err
	}
	return slices.Collect(func(yield func(TemplateInput) bool) {
		for _, t := range stored {
			if !yield(t.TemplateInput) {
				return
			}
		}
	}), nil
}

func LockTemplateNamed(ctx context.Context, tx pgx.Tx, name string) (TemplateOwner, bool, error) {
	var owner TemplateOwner
	err := tx.QueryRow(ctx, "SELECT id::text,project_id::text FROM olp.route_templates WHERE name=$1 FOR UPDATE", name).Scan(&owner.ID, &owner.ProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return TemplateOwner{}, false, nil
	}
	return owner, err == nil, err
}
