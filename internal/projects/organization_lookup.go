package projects

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
)

type OrganizationBudget struct {
	Name   string
	Budget *authority.BudgetPolicy
}

type LockedOrganization struct {
	ID     string
	Budget *authority.BudgetPolicy
}

func ReadOrganizationBudgets(ctx context.Context, q access.Queryer) ([]OrganizationBudget, error) {
	rows, err := q.Query(ctx, "SELECT name,budget_policy FROM olp.organizations")
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[OrganizationBudget])
}

func LockOrganizationByName(ctx context.Context, tx pgx.Tx, name string) (LockedOrganization, bool, error) {
	rows, err := tx.Query(ctx, "SELECT id::text,budget_policy FROM olp.organizations WHERE lower(name)=lower($1) FOR UPDATE", name)
	if err != nil {
		return LockedOrganization{}, false, err
	}
	locked, err := pgx.CollectOneRow(rows, pgx.RowToStructByPos[LockedOrganization])
	if errors.Is(err, pgx.ErrNoRows) {
		return LockedOrganization{}, false, nil
	}
	return locked, err == nil, err
}

func OrganizationIDByName(ctx context.Context, q access.Queryer, name string) (string, bool, error) {
	var id string
	err := q.QueryRow(ctx, "SELECT id::text FROM olp.organizations WHERE lower(name)=lower($1)", name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

func LockProjectOrganization(ctx context.Context, tx pgx.Tx, project string) (*string, error) {
	var organization *string
	err := tx.QueryRow(ctx, "SELECT organization_id::text FROM olp.projects WHERE id=$1 FOR UPDATE", project).Scan(&organization)
	return organization, err
}
