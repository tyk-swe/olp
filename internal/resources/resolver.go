package resources

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

var (
	ErrUnavailable = errors.New("provider resource credential unavailable")
	ErrNoRows      = errors.New("provider resource revision unavailable")
)

type ResolvedPin struct {
	Invocation providerinvoke.Target
	Route      routing.Route
	Target     routing.Target
}

type Resolver struct {
	pool *pgxpool.Pool
}

func NewResolver(pool *pgxpool.Pool) *Resolver {
	return &Resolver{pool: pool}
}

func (r *Resolver) ResolveCurrent(ctx context.Context, res *Resource, operation string) (ResolvedPin, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return ResolvedPin{}, err
	}
	defer conn.Release()
	return resolvePin(ctx, conn, res, operation)
}

func resolvePin(ctx context.Context, query secrets.RowQuerier, res *Resource, operation string) (ResolvedPin, error) {
	provider, selected, err := loadPinnedProvider(ctx, query, res)
	if err != nil {
		return ResolvedPin{}, err
	}
	route, target, err := loadPinnedRoute(ctx, query, res, provider.ID, operation)
	if err != nil {
		return ResolvedPin{}, err
	}
	if err := verifyCredentialActive(ctx, query, res.CredentialID); err != nil {
		return ResolvedPin{}, err
	}
	return ResolvedPin{
		Invocation: providerinvoke.Target{Provider: provider, Slot: selected, Model: target.ProviderModel},
		Route:      route,
		Target:     target,
	}, nil
}

func loadPinnedProvider(ctx context.Context, query secrets.RowQuerier, res *Resource) (routing.Provider, routing.Slot, error) {
	providerRevision := routing.ProviderRevision{RevisionID: res.ProviderRevisionID}
	err := query.QueryRow(ctx,
		"SELECT r.provider_id::text,r.configuration,r.models,r.slots,r.name,p.state,p.project_id::text,"+release.PluginColumn("r")+" FROM olp.provider_revisions r JOIN olp.providers p ON p.id=r.provider_id WHERE r.id=$1",
		res.ProviderRevisionID).Scan(&providerRevision.ID, &providerRevision.Configuration, &providerRevision.Models, &providerRevision.Slots, &providerRevision.Name, &providerRevision.State, &providerRevision.ProjectID, &providerRevision.InstalledPlugin)
	if errors.Is(err, pgx.ErrNoRows) {
		return routing.Provider{}, routing.Slot{}, fmt.Errorf("provider revision %s: %w", res.ProviderRevisionID, ErrNoRows)
	}
	if err != nil {
		return routing.Provider{}, routing.Slot{}, err
	}
	if providerRevision.ID != res.ProviderID {
		return routing.Provider{}, routing.Slot{}, fmt.Errorf("provider revision %s belongs to %s, not %s: %w",
			res.ProviderRevisionID, providerRevision.ID, res.ProviderID, ErrNoRows)
	}
	provider, err := routing.DecodeProviderRevision(providerRevision)
	if err != nil {
		return routing.Provider{}, routing.Slot{}, err
	}
	selected, ok := routing.PinnedSlot(&provider, res.SlotID, res.CredentialID)
	if !ok {
		return routing.Provider{}, routing.Slot{}, fmt.Errorf("slot %s or credential absent from provider revision %s: %w", res.SlotID, res.ProviderRevisionID, ErrNoRows)
	}
	return provider, selected, nil
}

func loadPinnedRoute(ctx context.Context, query secrets.RowQuerier, res *Resource, providerID, operation string) (routing.Route, routing.Target, error) {
	routeRevision := routing.RouteRevision{RevisionID: res.RouteRevisionID}
	err := query.QueryRow(ctx,
		`SELECT v.route_id::text,v.slug,v.revision,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.activated_at,v.routing_policy,r.project_id::text,v.fidelity,v.behavior
         FROM olp.route_revisions v JOIN olp.routes r ON r.id=v.route_id WHERE v.id=$1`,
		res.RouteRevisionID).Scan(&routeRevision.ID, &routeRevision.Slug, &routeRevision.Revision, &routeRevision.Operations,
		&routeRevision.OverallTimeout, &routeRevision.MaxAttempts, &routeRevision.Targets, &routeRevision.PublishedAt, &routeRevision.Policy, &routeRevision.ProjectID, &routeRevision.Fidelity, &routeRevision.Behavior)
	if errors.Is(err, pgx.ErrNoRows) {
		return routing.Route{}, routing.Target{}, fmt.Errorf("route revision %s: %w", res.RouteRevisionID, ErrNoRows)
	}
	if err != nil {
		return routing.Route{}, routing.Target{}, err
	}
	if routeRevision.Slug != res.RouteSlug {
		return routing.Route{}, routing.Target{}, fmt.Errorf("route revision %s is slug %s, not %s: %w",
			res.RouteRevisionID, routeRevision.Slug, res.RouteSlug, ErrNoRows)
	}
	route, err := routing.DecodeRouteRevision(routeRevision)
	if err != nil {
		return routing.Route{}, routing.Target{}, err
	}
	if !slices.Contains(route.Operations, operation) {
		return routing.Route{}, routing.Target{}, fmt.Errorf("route %s revision does not allow %s: %w", route.Slug, operation, ErrNoRows)
	}
	target, ok := matchPinnedTarget(route.Targets, res.TargetID, providerID)
	if !ok {
		return routing.Route{}, routing.Target{}, fmt.Errorf("target %s is not proved by route revision %s: %w", res.TargetID, res.RouteRevisionID, ErrNoRows)
	}
	return route, target, nil
}

func matchPinnedTarget(targets []routing.Target, targetID, providerID string) (routing.Target, bool) {
	if targetID == "" {
		return routing.Target{}, false
	}
	matched := 0
	var found routing.Target
	for _, target := range targets {
		if target.ID == targetID && target.ProviderID == providerID && target.ProviderModel != "" {
			matched++
			found = target
		}
	}
	return found, matched == 1
}

func verifyCredentialActive(ctx context.Context, query secrets.RowQuerier, credentialID *string) error {
	if credentialID == nil {
		return nil
	}
	var revoked bool
	err := query.QueryRow(ctx,
		"SELECT revoked_at IS NOT NULL FROM olp.provider_credentials WHERE id=$1",
		*credentialID).Scan(&revoked)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && revoked) {
		return fmt.Errorf("credential %s: %w", *credentialID, ErrUnavailable)
	}
	return err
}
