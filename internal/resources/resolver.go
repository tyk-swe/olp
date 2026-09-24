package resources

import (
	"context"
	"encoding/json"
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
	var providerID, providerName, providerState string
	var providerProject *string
	var configuration, models, slots []byte
	err := query.QueryRow(ctx,
		"SELECT r.provider_id::text,r.configuration,r.models,r.slots,r.name,p.state,p.project_id::text FROM olp_go.provider_revisions r JOIN olp_go.providers p ON p.id=r.provider_id WHERE r.id=$1",
		res.ProviderRevisionID).Scan(&providerID, &configuration, &models, &slots, &providerName, &providerState, &providerProject)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, nil, fmt.Errorf("provider revision %s: %w", res.ProviderRevisionID, ErrNoRows)
	}
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if providerID != res.ProviderID {
		return nil, nil, nil, nil, fmt.Errorf("provider revision %s belongs to %s, not %s: %w",
			res.ProviderRevisionID, providerID, res.ProviderID, ErrNoRows)
	}

	provider := &runtime.Provider{ID: providerID, Name: providerName, Enabled: providerState == "active", ProjectID: providerProject, RevisionID: res.ProviderRevisionID, Capabilities: []runtime.Capability{}}
	var cfg runtime.Configuration
	var revisionModels []runtime.RevisionModel
	var revisionSlots []runtime.RevisionSlot
	if err = json.Unmarshal(configuration, &cfg); err == nil {
		err = json.Unmarshal(models, &revisionModels)
	}
	if err == nil {
		err = json.Unmarshal(slots, &revisionSlots)
	}
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("provider %s revision: %w", providerID, err)
	}
	provider.Network = cfg.Options.Network
	provider.ProfileID, provider.ProfileRevision = cfg.ProfileID, cfg.ProfileRevision
	provider.SemanticHeaders, provider.QuerySettings = cfg.Options.SemanticHeaders, cfg.Options.QuerySettings
	provider.OperationDefaults, provider.Bindings = cfg.Options.OperationDefaults, cfg.Options.Bindings
	provider.Kind = cfg.Kind
	provider.AuthMode = cfg.AuthMode
	provider.Endpoint = cfg.Endpoint
	provider.CloudRegion, provider.CloudProject, provider.Deployment, provider.APIVersion = cfg.CloudRegion, cfg.CloudProject, cfg.Deployment, cfg.APIVersion
	provider.Models = cfg.Options.Models
	provider.CredentialHeaders = cfg.Options.CredentialHeaders
	provider.ParameterDefaults = cfg.Options.ParameterDefaults
	provider.VendorID = cfg.Options.VendorID
	provider.Limits = cfg.Options.Limits
	for _, revisionSlot := range revisionSlots {
		provider.Slots = append(provider.Slots, revisionSlot.Slot)
		if revisionSlot.Default {
			provider.DefaultSlotID = revisionSlot.ID
			provider.ActiveCredential = revisionSlot.CredentialID
		}
	}
	for _, model := range revisionModels {
		for _, c := range model.Capabilities {
			if c.Source == "certified" {
				provider.Capabilities = append(provider.Capabilities, runtime.Capability{Model: model.UpstreamModel, Operation: c.Operation, Surface: c.Surface, Mode: c.Mode})
			}
		}
	}

	var slot *runtime.Slot
	for i := range revisionSlots {
		if revisionSlots[i].ID == res.SlotID {
			copied := revisionSlots[i].Slot
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

	route := &runtime.Route{RevisionID: res.RouteRevisionID}
	var operations, targets, policy, fidelity, contentPolicy []byte
	err = query.QueryRow(ctx,
		`SELECT v.route_id::text,v.slug,v.revision,v.operations,v.overall_timeout_ms,v.max_attempts,v.targets,v.activated_at,v.routing_policy,r.project_id::text,v.fidelity,v.content_policy
         FROM olp_go.route_revisions v JOIN olp_go.routes r ON r.id=v.route_id WHERE v.id=$1`,
		res.RouteRevisionID).Scan(&route.ID, &route.Slug, &route.Revision, &operations,
		&route.OverallTimeout, &route.MaxAttempts, &targets, &route.PublishedAt, &policy, &route.ProjectID, &fidelity, &contentPolicy)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, nil, fmt.Errorf("route revision %s: %w", res.RouteRevisionID, ErrNoRows)
	}
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if route.Slug != res.RouteSlug {
		return nil, nil, nil, nil, fmt.Errorf("route revision %s is slug %s, not %s: %w",
			res.RouteRevisionID, route.Slug, res.RouteSlug, ErrNoRows)
	}
	var published []runtime.PublishedTarget
	if err = json.Unmarshal(operations, &route.Operations); err == nil {
		err = json.Unmarshal(targets, &published)
	}
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("route %s revision: %w", route.Slug, err)
	}
	if len(policy) > 0 {
		if err = json.Unmarshal(policy, &route.Policy); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("route %s policy: %w", route.Slug, err)
		}
	}
	if len(contentPolicy) > 0 {
		if err = json.Unmarshal(contentPolicy, &route.ContentPolicy); err != nil {
			return nil, nil, nil, nil, err
		}
	}
	route.Fidelity, err = runtime.DecodeFidelity(fidelity)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("route %s fidelity: %w", route.Slug, err)
	}
	route.RoutingID = route.ID
	route.PublishedAt = route.PublishedAt.UTC()
	for _, t := range published {
		route.Targets = append(route.Targets, runtime.Target{ID: t.ID, ProviderID: t.ProviderID, ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, Timeout: t.TimeoutMS, RoutingID: t.ProviderModelID})
	}
	if !slices.Contains(route.Operations, operation) {
		return nil, nil, nil, nil, fmt.Errorf("route %s revision does not allow %s: %w", route.Slug, operation, ErrNoRows)
	}

	var secret []byte
	if res.CredentialID != nil {
		var revoked bool
		err = query.QueryRow(ctx,
			"SELECT revoked_at IS NOT NULL FROM olp_go.provider_credentials WHERE id=$1",
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
	return provider, route, slot, secret, nil
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
	if err := tx.QueryRow(ctx, "SELECT revoked_at IS NULL FROM olp_go.provider_network_credentials WHERE id=$1 AND provider_id=$2", credentialID, providerID).Scan(&valid); err != nil || !valid {
		return nil, ErrUnavailable
	}
	return r.keys.Read(ctx, tx, r.installation, credentialID, "provider_credential")
}
