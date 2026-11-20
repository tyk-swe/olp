package routes

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type StoredTemplate struct {
	TemplateInput
	ID string
}

func ReadTemplateInputs(ctx context.Context, q database.Queryer) ([]TemplateInput, error) {
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

func LockTemplateNamed(ctx context.Context, tx pgx.Tx, name string) (StoredTemplate, bool, error) {
	stored, err := scanTemplate(tx.QueryRow(ctx, "SELECT "+templateColumns+" FROM olp.route_templates WHERE name=$1 FOR UPDATE", name))
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredTemplate{}, false, nil
	}
	if err != nil {
		return StoredTemplate{}, false, err
	}
	return StoredTemplate{TemplateInput: stored.TemplateInput, ID: stored.ID}, true, nil
}
