package gateway

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
)

// Retained resources are provider objects a key created earlier and may use
// again: stored responses, files, batches, continuations, Gemini
// interactions, and video jobs. Every use admits the key again, whatever the
// resource. A resource the key may no longer reach answers exactly like one
// that does not exist, and a resource whose serving identity is gone answers
// that it is unavailable.

func pinUnavailable() *Error {
	return serverError(http.StatusConflict, "provider_resource_credential_unavailable",
		"The provider revision, slot, or credential that owns this object is no longer available.")
}

// retainedUse is what a request does with a retained resource.
type retainedUse int

const (
	// retainedHousekeeping lists, deletes or cancels a retained resource.
	retainedHousekeeping retainedUse = iota
	// retainedRetrieval reads a retained resource or its content.
	retainedRetrieval
	// retainedNewWork starts provider work from a retained resource.
	retainedNewWork
)

// retainedContract serves a retained resource only under the contract it was
// created with. A strict resource is refused once its route is transformed,
// and a transformed one cannot start new work once its route is strict. The
// owner can always list, delete or cancel, so provider-held data can be
// removed and running upstream work stopped whatever the route promises now.
func retainedContract(strict bool, current runtime.RouteFidelity, use retainedUse) *Error {
	switch {
	case use == retainedHousekeeping:
		return nil
	case strict && !current.Strict():
		return serverError(http.StatusConflict, "provider_resource_unavailable",
			"This strict resource is unavailable because its route is now transformed.")
	case !strict && current.Strict() && use == retainedNewWork:
		return serverError(http.StatusConflict, "provider_resource_unavailable",
			"This resource was created under a transformed route and cannot start work now that the route is strict.")
	}
	return nil
}

// strictResourceKind reports whether a resource kind holds a strict contract.
// Gemini Interactions keep one kind under either fidelity, so their kind says
// nothing about the contract (known is false).
func strictResourceKind(kind string) (strict, known bool) {
	switch kind {
	case resources.KindStrictResponse, resources.KindStrictFile, resources.KindStrictBatch, resources.KindContinuation:
		return true, true
	case resources.KindResponse, resources.KindFile, resources.KindBatch:
		return false, true
	}
	return false, false
}

// resolveResource rebuilds the provider pin that owns a retained resource and
// refuses a use its route no longer promises.
func (s *Server) resolveResource(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, operation string, use retainedUse) (*pin, *runtime.Route, *Error) {
	if s.Resolver == nil || s.Resources == nil {
		return nil, nil, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "Provider state is not configured on this installation.")
	}
	provider, route, slot, err := s.Resolver.ResolveCurrent(ctx, res, operation)
	if errors.Is(err, resources.ErrUnavailable) || errors.Is(err, resources.ErrNoRows) {
		return nil, nil, pinUnavailable()
	}
	if err != nil {
		return nil, nil, serverError(http.StatusInternalServerError, "internal_error", "The stored target could not be rebuilt.")
	}
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		return nil, nil, notFoundError("not_found", "No "+res.Kind+" with this identifier exists for this key.")
	}
	if current, published := x.request.release.Snapshot.Routes[res.RouteSlug]; published {
		if strict, known := strictResourceKind(res.Kind); known {
			if e := retainedContract(strict, current.Fidelity, use); e != nil {
				return nil, nil, e
			}
		}
	}
	var target *runtime.Target
	for i := range route.Targets {
		if route.Targets[i].ProviderID != provider.ID || route.Targets[i].ProviderModel != resourceModel(res) {
			continue
		}
		if target != nil {
			return nil, nil, pinUnavailable()
		}
		target = &route.Targets[i]
	}
	if target == nil {
		return nil, nil, pinUnavailable()
	}
	if use == retainedNewWork {
		if e := restrictRetainedWork(x.request.release.Snapshot, authority.ID, provider, route, slot, *target, operation); e != nil {
			return nil, nil, e
		}
	}
	attempt := runtime.Attempt{
		TargetID:           target.ID,
		ProviderID:         provider.ID,
		ProviderRevisionID: provider.RevisionID,
		ProviderKind:       provider.Kind,
		UpstreamModel:      target.ProviderModel,
		Timeout:            time.Duration(target.Timeout) * time.Millisecond,
		VendorID:           provider.VendorID,
	}
	p := &pin{target: *target, provider: *provider, attempt: attempt, slot: *slot, model: target.ProviderModel}
	return p, route, nil
}

// admitStoredResponse admits a key to a retained response and returns the pin
// that serves it. A strict response also needs provider state on the key and
// the serving identity its contract recorded.
func (s *Server) admitStoredResponse(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, contract *storedResponseContract, use retainedUse) (*pin, *runtime.Route, *Error) {
	if contract != nil {
		current, ok := x.request.release.Snapshot.Routes[res.RouteSlug]
		if !ok || !authority.Policy.AllowProviderState || !authority.Allows("inference", current.Slug, current.ProjectID, s.now()) {
			return nil, nil, notFoundError("not_found", "The stored response is unavailable to this key.")
		}
	}
	p, route, e := s.resolveResource(ctx, x, authority, res, operationGeneration, use)
	if e != nil || contract == nil {
		return p, route, e
	}
	if !p.provider.Enabled || !p.slot.Allows(p.model, res.RouteSlug, authority.ID) || p.model != contract.Binding || p.provider.RevisionID != contract.Receipt.Serving.RevisionID || p.provider.ProfileID != contract.Receipt.ProfileID || p.provider.ProfileRevision != contract.Receipt.ProfileRevision {
		return nil, nil, pinUnavailable()
	}
	if p.provider.Network != nil && p.provider.Network.CredentialID != "" {
		if _, err := s.providerNetworkSecret(ctx, x.request.release, &p.provider); err != nil {
			return nil, nil, pinUnavailable()
		}
	}
	return p, route, nil
}

// authorizeResponseContract checks a key may use a strict retained response
// whose pin is resolved later.
func (s *Server) authorizeResponseContract(ctx context.Context, x *execution, authority access.Authority, res *resources.Resource, contract *storedResponseContract, use retainedUse) *Error {
	_, _, e := s.admitStoredResponse(ctx, x, authority, res, contract, use)
	return e
}

// admitDurable admits a key to a retained file or batch and returns the pin
// that serves it. A strict one also needs provider state on the key and the
// serving identity its document recorded.
func (s *Server) admitDurable(ctx context.Context, x *execution, authority access.Authority, r *resources.Resource, doc *durableDocument, operation string, use retainedUse) (*pin, *runtime.Route, *Error) {
	if doc != nil {
		current, ok := x.snapshot().Routes[r.RouteSlug]
		if !ok || !authority.Policy.AllowProviderState || !authority.Allows("inference", current.Slug, current.ProjectID, s.now()) {
			return nil, nil, notFoundError("not_found", "The stored provider resource is unavailable to this key.")
		}
	}
	p, route, e := s.resolveResource(ctx, x, authority, r, operation, use)
	if e != nil || doc == nil {
		return p, route, e
	}
	profile, profileErr := p.provider.Connector().Profile()
	if profileErr != nil || !p.provider.Enabled || !p.slot.Allows(p.model, r.RouteSlug, authority.ID) || p.model != doc.Binding || p.provider.RevisionID != doc.Serving.RevisionID || profile.ID != doc.Serving.ProfileID || profile.Revision != doc.Serving.ProfileRevision {
		return nil, nil, pinUnavailable()
	}
	if p.provider.Network != nil && p.provider.Network.CredentialID != "" {
		if _, err := s.providerNetworkSecret(ctx, x.request.release, &p.provider); err != nil {
			return nil, nil, pinUnavailable()
		}
	}
	return p, route, nil
}

// authorizeDurable checks a key may use a strict retained file or batch; a
// transformed one (doc is nil) is checked when its pin is resolved.
func (s *Server) authorizeDurable(ctx context.Context, x *execution, authority access.Authority, r *resources.Resource, doc *durableDocument, operation string, use retainedUse) *Error {
	if doc == nil {
		return nil
	}
	_, _, e := s.admitDurable(ctx, x, authority, r, doc, operation, use)
	return e
}

// Historical protocol and credential identity do not grant historical authority
// to start fresh inference. Intersect that contract with the installed release.
func restrictRetainedWork(current *runtime.Snapshot, keyID string, provider *runtime.Provider, route *runtime.Route, slot *runtime.Slot, target runtime.Target, operation string) *Error {
	live, ok := current.Providers[provider.ID]
	if !ok || !live.Enabled {
		return pinUnavailable()
	}
	liveRoute, ok := current.Routes[route.Slug]
	if !ok || !slices.Contains(liveRoute.Operations, operation) {
		return pinUnavailable()
	}
	foundTarget := false
	for _, candidate := range liveRoute.Targets {
		if candidate.ProviderID == target.ProviderID && candidate.ProviderModel == target.ProviderModel {
			foundTarget = true
			break
		}
	}
	if !foundTarget {
		return pinUnavailable()
	}
	var liveSlot *runtime.Slot
	for _, candidate := range live.Slots {
		if candidate.ID == slot.ID {
			liveSlot = &candidate
			break
		}
	}
	if liveSlot == nil || !liveSlot.Allows(target.ProviderModel, route.Slug, keyID) || !slot.Allows(target.ProviderModel, route.Slug, keyID) {
		return pinUnavailable()
	}
	slot.RequestsPerMinute = tighterRetainedLimit(slot.RequestsPerMinute, liveSlot.RequestsPerMinute)
	slot.TokensPerMinute = tighterRetainedLimit(slot.TokensPerMinute, liveSlot.TokensPerMinute)
	slot.MaxConcurrency = tighterRetainedLimit(slot.MaxConcurrency, liveSlot.MaxConcurrency)
	if live.Limits != nil {
		prior := runtime.Limits{}
		if provider.Limits != nil {
			prior = *provider.Limits
		}
		provider.Limits = &runtime.Limits{
			RequestsPerMinute: tighterRetainedLimit(prior.RequestsPerMinute, live.Limits.RequestsPerMinute),
			TokensPerMinute:   tighterRetainedLimit(prior.TokensPerMinute, live.Limits.TokensPerMinute),
			MaxConcurrency:    tighterRetainedLimit(prior.MaxConcurrency, live.Limits.MaxConcurrency),
		}
	}
	route.ContentPolicy = liveRoute.ContentPolicy
	return nil
}

func tighterRetainedLimit(old, current *int64) *int64 {
	if old == nil || current != nil && *current < *old {
		return current
	}
	return old
}
