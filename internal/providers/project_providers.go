package providers

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
)

type ProjectProvider struct {
	ID            string
	Configuration Configuration
}

type RevisionLabels struct {
	Provider string
	Project  string
	Models   []routing.RevisionModel
}

func ReadProjectProvider(ctx context.Context, q access.Queryer, name, projectID string) (*ProjectProvider, error) {
	var provider ProjectProvider
	err := q.QueryRow(ctx, `SELECT id::text,configuration FROM olp.providers WHERE lower(name)=lower($1) AND project_id=$2`, name, projectID).Scan(&provider.ID, &provider.Configuration)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &provider, nil
}

func ReadActiveRevision(ctx context.Context, q access.Queryer, name, projectID string) (*routing.ProviderRevision, error) {
	var current routing.ProviderRevision
	err := q.QueryRow(ctx, `SELECT p.id::text,v.id::text,p.name,p.state,p.project_id::text,v.configuration,v.models,v.slots FROM olp.providers p JOIN olp.provider_revisions v ON v.id=p.active_revision_id WHERE lower(p.name)=lower($1) AND p.project_id=$2 AND p.state='active'`, name, projectID).Scan(&current.ID, &current.RevisionID, &current.Name, &current.State, &current.ProjectID, &current.Configuration, &current.Models, &current.Slots)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &current, nil
}

func ReadRevisionLabels(ctx context.Context, q access.Queryer, revisionID, providerID string) (RevisionLabels, error) {
	var labels RevisionLabels
	err := q.QueryRow(ctx, `SELECT p.name,j.name,v.models FROM olp.provider_revisions v JOIN olp.providers p ON p.id=v.provider_id JOIN olp.projects j ON j.id=p.project_id WHERE v.id=$1 AND p.id=$2`, revisionID, providerID).Scan(&labels.Provider, &labels.Project, &labels.Models)
	return labels, err
}
