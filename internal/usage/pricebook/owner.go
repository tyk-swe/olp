package pricebook

import (
	"context"
	"fmt"

	"github.com/tyk-swe/olp/internal/access"
)

type ownerKind string

const (
	ownerInstallation ownerKind = "installation"
	ownerOrganization ownerKind = "organization"
	ownerProject      ownerKind = "project"
)

// Owner is what a rate card belongs to.
type Owner struct {
	kind ownerKind
	id   string
}

func InstallationOwner() Owner          { return Owner{kind: ownerInstallation} }
func OrganizationOwner(id string) Owner { return Owner{kind: ownerOrganization, id: id} }
func ProjectOwner(id string) Owner      { return Owner{kind: ownerProject, id: id} }

func (o Owner) columns() (organizationID, projectID *string) {
	switch o.kind {
	case ownerOrganization:
		return &o.id, nil
	case ownerProject:
		return nil, &o.id
	default:
		return nil, nil
	}
}

func parseOwner(kind string, id *string) (Owner, error) {
	switch ownerKind(kind) {
	case ownerInstallation:
		if id != nil {
			return Owner{}, invalid("owner.id", "The installation card takes no owner id.")
		}
		return InstallationOwner(), nil
	case ownerOrganization, ownerProject:
		if id == nil {
			return Owner{}, invalid("owner.id", "Name the "+kind+".")
		}
		parsed, err := access.ParseUUID(*id)
		if err != nil {
			return Owner{}, invalid("owner.id", "Use a valid identifier.")
		}
		return Owner{kind: ownerKind(kind), id: parsed}, nil
	default:
		return Owner{}, invalid("owner.kind", "Use installation, organization or project.")
	}
}

func ownerExists(ctx context.Context, q access.Queryer, owner Owner) error {
	query := ""
	switch owner.kind {
	case ownerOrganization:
		query = "SELECT EXISTS(SELECT 1 FROM olp.organizations WHERE id=$1::uuid)"
	case ownerProject:
		query = "SELECT EXISTS(SELECT 1 FROM olp.projects WHERE id=$1::uuid)"
	default:
		return nil
	}
	var exists bool
	if err := q.QueryRow(ctx, query, owner.id).Scan(&exists); err != nil {
		return fmt.Errorf("read rate card owner: %w", err)
	}
	if !exists {
		return invalid("owner.id", "No such "+string(owner.kind)+".")
	}
	return nil
}
