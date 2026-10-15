package resources

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/tyk-swe/olp/internal/routing"

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
func (r *Resolver) ResolveCurrent(ctx context.Context, res *Resource, operation string) (*routing.Provider, *routing.Route, *routing.Slot, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	defer conn.Release()
	return r.resolve(ctx, conn, res, operation)
}

func (r *Resolver) Resolve(ctx context.Context, tx pgx.Tx, res *Resource, operation string) (*routing.Provider, *routing.Route, *routing.Slot, error) {
	return r.resolve(ctx, tx, res, operation)
}

func (r *Resolver) resolve(ctx context.Context, query secrets.RowQuerier, res *Resource, operation string) (*routing.Provider, *routing.Route, *routing.Slot, error) {
	providerRevision := routing.ProviderRevision{RevisionID: res.ProviderRevisionID}
	err := query.QueryRow(ctx,
		"SELECT r.provider_id::text,r.configuration,r.models,r.slots,r.name,p.state,p.project_id::text,"+runtime.PluginColumn+" FROM olp.provider_revisions r JOIN olp.providers p ON p.id=r.provider_id WHERE r.id=$1",
		res.ProviderRevisionID).Scan(&providerRevision.ID, &providerRevision.Configuration, &providerRevision.Models, &providerRevision.Slots, &providerRevision.Name, &providerRevision.State, &providerRevision.ProjectID, &providerRevision.InstalledPlugin)
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

	provider, err := routing.DecodeProviderRevision(providerRevision)
	if err != nil {
		return nil, nil, nil, err
	}

	selected, ok := routing.PinnedSlot(&provider, res.SlotID, res.CredentialID)
	if !ok {
		return nil, nil, nil, fmt.Errorf("slot %s or credential absent from provider revision %s: %w", res.SlotID, res.ProviderRevisionID, ErrNoRows)
	}
	slot := &selected

	routeRevision := routing.RouteRevision{RevisionID: res.RouteRevisionID}
	err = query.QueryRow(ctx,
		`SELECT v.route_id::text,v.slug,v.revision,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.activated_at,v.routing_policy,r.project_id::text,v.fidelity,v.content_policy,v.behavior
         FROM olp.route_revisions v JOIN olp.routes r ON r.id=v.route_id WHERE v.id=$1`,
		res.RouteRevisionID).Scan(&routeRevision.ID, &routeRevision.Slug, &routeRevision.Revision, &routeRevision.Operations,
		&routeRevision.OverallTimeout, &routeRevision.MaxAttempts, &routeRevision.Targets, &routeRevision.PublishedAt, &routeRevision.Policy, &routeRevision.ProjectID, &routeRevision.Fidelity, &routeRevision.ContentPolicy, &routeRevision.Behavior)
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
	route, err := routing.DecodeRouteRevision(routeRevision)
	if err != nil {
		return nil, nil, nil, err
	}
	if !slices.Contains(route.Operations, operation) {
		return nil, nil, nil, fmt.Errorf("route %s revision does not allow %s: %w", route.Slug, operation, ErrNoRows)
	}
	matched := 0
	for _, target := range route.Targets {
		if res.TargetID != "" && target.ID == res.TargetID && target.ProviderID == provider.ID && target.ProviderModel != "" {
			matched++
		}
	}
	if matched != 1 {
		return nil, nil, nil, fmt.Errorf("target %s is not proved by route revision %s: %w", res.TargetID, res.RouteRevisionID, ErrNoRows)
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
