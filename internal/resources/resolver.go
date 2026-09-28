package resources

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
)

var (
	ErrUnavailable = errors.New("provider resource credential unavailable")

	ErrNoRows = errors.New("provider resource revision unavailable")
)

// Resolver rebuilds the historical provider pin that owns a retained
// resource. Its credential's secret comes from the credential source.
type Resolver struct {
	pool *pgxpool.Pool
}

func NewResolver(pool *pgxpool.Pool) *Resolver {
	return &Resolver{pool: pool}
}

// ResolveCurrent reads historical revisions and current revocation through
// one connection. READ COMMITTED already gives each SELECT a fresh snapshot,
// so this read-only path does not need an explicit transaction round trip.
func (r *Resolver) ResolveCurrent(ctx context.Context, res *Resource, operation string) (*runtime.Provider, *runtime.Route, *runtime.Slot, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	defer conn.Release()
	return r.resolve(ctx, conn, res, operation)
}

func (r *Resolver) Resolve(ctx context.Context, tx pgx.Tx, res *Resource, operation string) (*runtime.Provider, *runtime.Route, *runtime.Slot, error) {
	return r.resolve(ctx, tx, res, operation)
}

func (r *Resolver) resolve(ctx context.Context, query secrets.RowQuerier, res *Resource, operation string) (*runtime.Provider, *runtime.Route, *runtime.Slot, error) {
	providerRevision := runtime.ProviderRevision{RevisionID: res.ProviderRevisionID}
	err := query.QueryRow(ctx,
		"SELECT r.provider_id::text,r.configuration,r.models,r.slots,r.name,p.state,p.project_id::text,"+runtime.PluginColumn+" FROM olp.provider_revisions r JOIN olp.providers p ON p.id=r.provider_id WHERE r.id=$1",
		res.ProviderRevisionID).Scan(&providerRevision.ID, &providerRevision.Configuration, &providerRevision.Models, &providerRevision.Slots, &providerRevision.Name, &providerRevision.State, &providerRevision.ProjectID, &providerRevision.Plugin)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, fmt.Errorf("provider revision %s: %w", res.ProviderRevisionID, ErrNoRows)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if providerRevision.ID != res.ProviderID {
		return nil, nil, nil, fmt.Errorf("provider revision %s belongs to %s, not %s: %w",
			res.ProviderRevisionID, providerRevision.ID, res.ProviderID, ErrNoRows)
	}

	provider, err := runtime.DecodeProviderRevision(providerRevision)
	if err != nil {
		return nil, nil, nil, err
	}

	var slot *runtime.Slot
	for i := range provider.Slots {
		if provider.Slots[i].ID == res.SlotID {
			copied := provider.Slots[i]
			slot = &copied
			break
		}
	}
	if slot == nil {
		return nil, nil, nil, fmt.Errorf("slot %s absent from provider revision %s: %w", res.SlotID, res.ProviderRevisionID, ErrNoRows)
	}
	if (slot.CredentialID == nil) != (res.CredentialID == nil) ||
		(slot.CredentialID != nil && *slot.CredentialID != *res.CredentialID) {
		return nil, nil, nil, fmt.Errorf("slot %s credential drifted: %w", res.SlotID, ErrNoRows)
	}

	routeRevision := runtime.RouteRevision{RevisionID: res.RouteRevisionID}
	err = query.QueryRow(ctx,
		`SELECT v.route_id::text,v.slug,v.revision,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.activated_at,v.routing_policy,r.project_id::text,v.fidelity,v.content_policy
         FROM olp.route_revisions v JOIN olp.routes r ON r.id=v.route_id WHERE v.id=$1`,
		res.RouteRevisionID).Scan(&routeRevision.ID, &routeRevision.Slug, &routeRevision.Revision, &routeRevision.Operations,
		&routeRevision.OverallTimeout, &routeRevision.MaxAttempts, &routeRevision.Targets, &routeRevision.PublishedAt, &routeRevision.Policy, &routeRevision.ProjectID, &routeRevision.Fidelity, &routeRevision.ContentPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, fmt.Errorf("route revision %s: %w", res.RouteRevisionID, ErrNoRows)
	}
	if err != nil {
		return nil, nil, nil, err
	}
	if routeRevision.Slug != res.RouteSlug {
		return nil, nil, nil, fmt.Errorf("route revision %s is slug %s, not %s: %w",
			res.RouteRevisionID, routeRevision.Slug, res.RouteSlug, ErrNoRows)
	}
	route, err := runtime.DecodeRouteRevision(routeRevision)
	if err != nil {
		return nil, nil, nil, err
	}
	if !slices.Contains(route.Operations, operation) {
		return nil, nil, nil, fmt.Errorf("route %s revision does not allow %s: %w", route.Slug, operation, ErrNoRows)
	}

	if res.CredentialID != nil {
		var revoked bool
		err = query.QueryRow(ctx,
			"SELECT revoked_at IS NOT NULL FROM olp.provider_credentials WHERE id=$1",
			*res.CredentialID).Scan(&revoked)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && revoked) {
			return nil, nil, nil, fmt.Errorf("credential %s: %w", *res.CredentialID, ErrUnavailable)
		}
		if err != nil {
			return nil, nil, nil, err
		}
	}
	return &provider, &route, slot, nil
}
