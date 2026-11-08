package project

import (
	"context"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/projects"
	"github.com/tyk-swe/olp/internal/promotion"
)

func Applier(desired []Project, costs Costs) promotion.Applier {
	return func(ctx context.Context, tx pgx.Tx, p access.Principal) error {
		current, err := Read(ctx, tx)
		if err != nil {
			return err
		}
		for _, project := range desired {
			if err := ensure(ctx, tx, p, costs, project, current); err != nil {
				return err
			}
		}
		directory, err := ReadDirectory(ctx, tx)
		if err != nil {
			return err
		}
		changed, err := projects.ApplyPolicies(ctx, tx, p, imports(desired, directory))
		if err != nil || !changed {
			return err
		}
		if _, err := access.AdvanceAuthority(ctx, tx); err != nil {
			return err
		}
		return nil
	}
}

func imports(desired []Project, directory Directory) []projects.PolicyImport {
	return slices.Collect(func(yield func(projects.PolicyImport) bool) {
		for _, project := range desired {
			id, _ := directory.ID(project.Name)
			if !yield(project.imported(id)) {
				return
			}
		}
	})
}

func ensure(ctx context.Context, tx pgx.Tx, p access.Principal, costs Costs, project Project, state State) error {
	current, ok := state.Project(project.Name)
	if !ok {
		return create(ctx, tx, p, project)
	}
	if project.Organization == nil {
		return nil
	}
	return assign(ctx, tx, p, costs, current.ID, *project.Organization)
}

func create(ctx context.Context, tx pgx.Tx, p access.Principal, project Project) error {
	organization, err := createIn(ctx, tx, p, project.Organization)
	if err != nil {
		return err
	}
	if _, _, err := projects.CreateProjectInOrganization(ctx, tx, project.Name, p.UserID(), organization); err != nil {
		return err
	}
	return nil
}

func createIn(ctx context.Context, tx pgx.Tx, p access.Principal, name *string) (*string, error) {
	if name == nil {
		return nil, nil
	}
	id, err := organizationID(ctx, tx, *name)
	if err != nil {
		return nil, err
	}
	if err := p.Authorize(access.Access); err != nil {
		return nil, err
	}
	return &id, nil
}

func assign(ctx context.Context, tx pgx.Tx, p access.Principal, costs Costs, project, organizationName string) error {
	organization, err := organizationID(ctx, tx, organizationName)
	if err != nil {
		return err
	}
	var current *string
	if err := tx.QueryRow(ctx, "SELECT organization_id::text FROM olp.projects WHERE id=$1 FOR UPDATE", project).Scan(&current); err != nil {
		return err
	}
	if current != nil && *current == organization {
		return nil
	}
	if current != nil {
		return organizationImmutable()
	}
	if err := p.Authorize(access.Access); err != nil {
		return err
	}
	snapshots, err := projects.AssignOrganization(ctx, tx, project, organization)
	if err != nil {
		return err
	}
	return costs.Install(ctx, snapshots)
}

func organizationID(ctx context.Context, q access.Queryer, name string) (string, error) {
	var id string
	err := q.QueryRow(ctx, "SELECT id::text FROM olp.organizations WHERE lower(name)=lower($1)", name).Scan(&id)
	return id, err
}
