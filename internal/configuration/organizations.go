package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
)

type OrganizationEntry struct {
	Name   string               `json:"name"`
	Budget *access.BudgetPolicy `json:"budget,omitempty"`
}

func validateOrganizations(ctx context.Context, q access.Queryer, doc *Document) error {
	if len(doc.Organizations) > 1000 {
		return access.Invalid("organizations", "Use at most 1000 organizations.")
	}
	names := map[string]bool{}
	for i := range doc.Organizations {
		o := &doc.Organizations[i]
		o.Name = strings.TrimSpace(o.Name)
		if err := access.ValidText("organizations", o.Name, 100); err != nil {
			return err
		}
		k := strings.ToLower(o.Name)
		if names[k] {
			return access.Invalid("organizations", "Organization names must be unique.")
		}
		names[k] = true
		if err := o.Budget.Validate(); err != nil {
			return err
		}
		o.Budget = normalizedBudget(o.Budget)
	}
	for i := range doc.Projects {
		p := &doc.Projects[i]
		if p.Organization == nil {
			continue
		}
		name := strings.TrimSpace(*p.Organization)
		p.Organization = &name
		if names[strings.ToLower(name)] {
			continue
		}
		var exists bool
		if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.organizations WHERE lower(name)=lower($1))", name).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return access.Invalid("organization", "Reference an organization in the document or destination.")
		}
	}
	return nil
}

func planOrganizations(ctx context.Context, q access.Queryer, doc *Document, result *planResult) error {
	for _, o := range doc.Organizations {
		var current *access.BudgetPolicy
		err := q.QueryRow(ctx, "SELECT budget_policy FROM olp.organizations WHERE lower(name)=lower($1)", o.Name).Scan(&current)
		action := "reuse"
		if errors.Is(err, pgx.ErrNoRows) {
			action = "create"
		} else if err != nil {
			return err
		} else if !reflect.DeepEqual(current, o.Budget) {
			action = "replace"
		}
		result.item("configuration", "organization:"+o.Name, action, "Organization budget; destination memberships are preserved.")
	}
	return nil
}

func applyOrganizations(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document) error {
	changed := false
	for _, o := range doc.Organizations {
		var id string
		var current *access.BudgetPolicy
		err := tx.QueryRow(ctx, "SELECT id::text,budget_policy FROM olp.organizations WHERE lower(name)=lower($1) FOR UPDATE", o.Name).Scan(&id, &current)
		if errors.Is(err, pgx.ErrNoRows) {
			if err = p.Authorize(access.Access); err != nil {
				return err
			}
			id, _, err = access.CreateOrganization(ctx, tx, o.Name, p.UserID())
			if err != nil {
				return err
			}
			changed = true
		} else if err != nil {
			return err
		}
		if reflect.DeepEqual(current, o.Budget) {
			continue
		}
		if err = p.Authorize(access.ManageOrganization); err != nil {
			return err
		}
		if err = p.Organization(id, access.Change); err != nil {
			return err
		}
		if o.Budget.Limited() {
			if err = limits.EnsureAggregateBudget(ctx, tx, "organization", id); err != nil {
				return err
			}
		}
		data, err := json.Marshal(o.Budget)
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "UPDATE olp.organizations SET budget_policy=NULLIF($2::jsonb,'null'::jsonb),etag=$3,updated_at=now() WHERE id=$1", id, data, access.NewID()); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		_, err := access.AdvanceAuthority(ctx, tx)
		return err
	}
	return nil
}

func (s *Server) applyProjectOrganization(ctx context.Context, tx pgx.Tx, p access.Principal, project string, name *string) error {
	if name == nil {
		return nil
	}
	var desired string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM olp.organizations WHERE lower(name)=lower($1)", *name).Scan(&desired); err != nil {
		return err
	}
	var current *string
	if err := tx.QueryRow(ctx, "SELECT organization_id::text FROM olp.projects WHERE id=$1 FOR UPDATE", project).Scan(&current); err != nil {
		return err
	}
	if current != nil {
		if *current == desired {
			return nil
		}
		return access.Fail(409, "project_organization_immutable", "A project cannot move between organizations.")
	}
	if err := p.Authorize(access.Access); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE olp.projects SET organization_id=$2,etag=$3,updated_at=now() WHERE id=$1", project, desired, access.NewID()); err != nil {
		return err
	}
	// The organization's budget now covers the project's spend so far, which its
	// balance in Valkey has never counted. Installing the recomputed balance
	// before gateways learn of the move keeps them from admitting against a cap
	// that omits it; a balance is never lowered, so a rollback errs toward refusal.
	if snapshot, ok, err := limits.ReconcileAggregateBudget(ctx, tx, "organization", desired, time.Now()); err != nil {
		return err
	} else if ok && s.Limiter != nil {
		if _, _, err := s.Limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
			return access.Fail(503, "limits_unavailable", "Budget state is unavailable; retry the apply.")
		}
	}
	_, err := access.AdvanceAuthority(ctx, tx)
	return err
}

// createProject creates a project the document declares. One that names an
// organization is created inside it, as the API creates one, so the
// organization's managers manage it rather than a direct grant to whoever
// applied the document, which would outlive their organization membership.
func createProject(ctx context.Context, tx pgx.Tx, p access.Principal, name string, organization *string) (string, error) {
	if organization == nil {
		id, _, err := access.CreateProject(ctx, tx, name, p.UserID())
		return id, err
	}
	var desired string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM olp.organizations WHERE lower(name)=lower($1)", *organization).Scan(&desired); err != nil {
		return "", err
	}
	if err := p.Authorize(access.Access); err != nil {
		return "", err
	}
	id, _, err := access.CreateProjectInOrganization(ctx, tx, name, p.UserID(), &desired)
	return id, err
}
