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

type Resolver struct {
	pool         *pgxpool.Pool
	installation string
	keys         *secrets.KeyRing
}

func NewResolver(pool *pgxpool.Pool, installation string, keys *secrets.KeyRing) *Resolver {
	return &Resolver{pool: pool, installation: installation, keys: keys}
}

// ResolveCurrent reads historical revisions and current authority through one
// connection. READ COMMITTED already gives each SELECT a fresh snapshot, so
// this read-only path does not need an explicit transaction round trip. A
// secret expiring during resolution is now rejected at the secret SELECT's
// statement time rather than remaining valid from the old BEGIN time.
func (r *Resolver) ResolveCurrent(ctx context.Context, res *Resource, operation string) (*runtime.Provider, *runtime.Route, *runtime.Slot, []byte, error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	defer conn.Release()
	return r.resolve(ctx, conn, res, operation)
}

func (r *Resolver) Resolve(ctx context.Context, tx pgx.Tx, res *Resource, operation string) (*runtime.Provider, *runtime.Route, *runtime.Slot, []byte, error) {
	return r.resolve(ctx, tx, res, operation)
}

func (r *Resolver) resolve(ctx context.Context, query secrets.RowQuerier, res *Resource, operation string) (*runtime.Provider, *runtime.Route, *runtime.Slot, []byte, error) {
	providerRevision := runtime.ProviderRevision{RevisionID: res.ProviderRevisionID}
	err := query.QueryRow(ctx,
		"SELECT r.provider_id::text,r.configuration,r.models,r.slots,r.name,p.state,p.project_id::text FROM olp.provider_revisions r JOIN olp.providers p ON p.id=r.provider_id WHERE r.id=$1",
		res.ProviderRevisionID).Scan(&providerRevision.ID, &providerRevision.Configuration, &providerRevision.Models, &providerRevision.Slots, &providerRevision.Name, &providerRevision.State, &providerRevision.ProjectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, nil, fmt.Errorf("provider revision %s: %w", res.ProviderRevisionID, ErrNoRows)
	}
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if providerRevision.ID != res.ProviderID {
		return nil, nil, nil, nil, fmt.Errorf("provider revision %s belongs to %s, not %s: %w",
			res.ProviderRevisionID, providerRevision.ID, res.ProviderID, ErrNoRows)
	}

	provider, err := runtime.DecodeProviderRevision(providerRevision)
	if err != nil {
		return nil, nil, nil, nil, err
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
		return nil, nil, nil, nil, fmt.Errorf("slot %s absent from provider revision %s: %w", res.SlotID, res.ProviderRevisionID, ErrNoRows)
	}
	if (slot.CredentialID == nil) != (res.CredentialID == nil) ||
		(slot.CredentialID != nil && *slot.CredentialID != *res.CredentialID) {
		return nil, nil, nil, nil, fmt.Errorf("slot %s credential drifted: %w", res.SlotID, ErrNoRows)
	}

	routeRevision := runtime.RouteRevision{RevisionID: res.RouteRevisionID}
	err = query.QueryRow(ctx,
		`SELECT v.route_id::text,v.slug,v.revision,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.activated_at,v.routing_policy,r.project_id::text,v.fidelity,v.content_policy
         FROM olp.route_revisions v JOIN olp.routes r ON r.id=v.route_id WHERE v.id=$1`,
		res.RouteRevisionID).Scan(&routeRevision.ID, &routeRevision.Slug, &routeRevision.Revision, &routeRevision.Operations,
		&routeRevision.OverallTimeout, &routeRevision.MaxAttempts, &routeRevision.Targets, &routeRevision.PublishedAt, &routeRevision.Policy, &routeRevision.ProjectID, &routeRevision.Fidelity, &routeRevision.ContentPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, nil, fmt.Errorf("route revision %s: %w", res.RouteRevisionID, ErrNoRows)
	}
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if routeRevision.Slug != res.RouteSlug {
		return nil, nil, nil, nil, fmt.Errorf("route revision %s is slug %s, not %s: %w",
			res.RouteRevisionID, routeRevision.Slug, res.RouteSlug, ErrNoRows)
	}
	route, err := runtime.DecodeRouteRevision(routeRevision)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if !slices.Contains(route.Operations, operation) {
		return nil, nil, nil, nil, fmt.Errorf("route %s revision does not allow %s: %w", route.Slug, operation, ErrNoRows)
	}

	var secret []byte
	if res.CredentialID != nil {
		var revoked bool
		err = query.QueryRow(ctx,
			"SELECT revoked_at IS NOT NULL FROM olp.provider_credentials WHERE id=$1",
			*res.CredentialID).Scan(&revoked)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && revoked) {
			return nil, nil, nil, nil, fmt.Errorf("credential %s: %w", *res.CredentialID, ErrUnavailable)
		}
		if err != nil {
			return nil, nil, nil, nil, err
		}
		secret, err = r.keys.Read(ctx, query, r.installation, *res.CredentialID, "provider_credential")
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("credential %s: %w", *res.CredentialID, ErrUnavailable)
		}
	}
	return &provider, &route, slot, secret, nil
}

// NetworkCredential resolves a retained provider revision's network identity
// under current ownership and revocation; retained revisions are not authority.
func (r *Resolver) NetworkCredential(ctx context.Context, providerID, credentialID string) ([]byte, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var valid bool
	if err := tx.QueryRow(ctx, "SELECT revoked_at IS NULL FROM olp.provider_network_credentials WHERE id=$1 AND provider_id=$2", credentialID, providerID).Scan(&valid); err != nil || !valid {
		return nil, ErrUnavailable
	}
	return r.keys.Read(ctx, tx, r.installation, credentialID, "provider_credential")
}
