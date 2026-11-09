package access

import (
	"context"
)

// Organization membership widens project scope, never installation roles or
// token scopes. Explicitly project-scoped tokens cannot administer an organization.
func (p Principal) Organization(id string, need Need) error {
	if p.AllProjects && (need == View || p.Authorize(Access) == nil) {
		return nil
	}
	role, ok := p.Organizations[id]
	if !ok {
		return NotFound
	}
	if need == Change && role != "manager" {
		return Forbidden()
	}
	return nil
}

func (p Principal) OrganizationIDs() []string {
	ids := make([]string, 0, len(p.Organizations))
	for id := range p.Organizations {
		ids = append(ids, id)
	}
	return ids
}

func (p *Principal) loadOrganizations(ctx context.Context, q Queryer, user string) error {
	rows, err := q.Query(ctx, "SELECT organization_id::text,role FROM olp.organization_members WHERE user_id=$1", user)
	if err != nil {
		return err
	}
	defer rows.Close()
	p.Organizations = map[string]string{}
	for rows.Next() {
		var id, role string
		if err = rows.Scan(&id, &role); err != nil {
			return err
		}
		p.Organizations[id] = role
	}
	return rows.Err()
}
