package kernel

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

// Retained resources are provider objects a key created earlier and may use
// again: stored responses, files, batches, continuations, Gemini
// interactions, and video jobs. Every use admits the key again, whatever the
// resource. A resource the key may no longer reach answers exactly like one
// that does not exist, and a resource whose serving identity is gone answers
// that it is unavailable.

func PinUnavailable() *fault.Error {
	return fault.ServerError(http.StatusConflict, "provider_resource_credential_unavailable",
		"The provider revision, slot, or credential that owns this object is no longer available.")
}

func (s *Server) AuthorizeListedResource(ctx context.Context, authority keyauthority.Authority, res *resources.Resource) *fault.Error {
	if s.Deps.Resolver == nil {
		return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Provider state is unavailable.")
	}
	operation := "batch"
	if res.Kind == resources.KindFile {
		operation = RetainedFileOperation(res)
	}
	resolved, err := s.Deps.Resolver.ResolveCurrent(ctx, res, operation)
	if errors.Is(err, resources.ErrUnavailable) || errors.Is(err, resources.ErrNoRows) {
		return PinUnavailable()
	}
	if err != nil {
		return fault.ServerError(http.StatusInternalServerError, "internal_error", "The stored target could not be rebuilt.")
	}
	if !authority.Allows("inference", resolved.Route.Slug, resolved.Route.ProjectID, s.Now()) {
		return fault.NotFoundError("not_found", "No "+res.Kind+" with this identifier exists for this key.")
	}
	return nil
}

const RetainedListCursorPrefix = "cursor_"

func (s *Server) retainedListCursorCipher(owner string) (cipher.AEAD, error) {
	if s.Deps.Digester == nil {
		return nil, errors.New("retained cursor key is unavailable")
	}
	block, err := aes.NewCipher(s.Deps.Digester.Digest(secrets.RetainedListCursorDigest, owner))
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func (s *Server) ListedResourcePage(owner, param string, limit int, token string, fetch func(resources.ListCursor, int) ([]*resources.Resource, *fault.Error), authorize func(*resources.Resource) *fault.Error) ([]*resources.Resource, string, *fault.Error) {
	anchor := resources.ListCursor{ID: token}
	if encoded, ok := strings.CutPrefix(token, RetainedListCursorPrefix); ok {
		if len(encoded) > 160 {
			return nil, "", fault.InvalidRequest("invalid_value", "The continuation cursor is invalid.", &param)
		}
		codec, err := s.retainedListCursorCipher(owner)
		if err != nil {
			return nil, "", fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The continuation cursor is unavailable.")
		}
		raw, err := base64.RawURLEncoding.DecodeString(encoded)
		if err != nil {
			return nil, "", fault.InvalidRequest("invalid_value", "The continuation cursor is invalid.", &param)
		}
		decoded, err := codec.Open(nil, nil, raw, nil)
		if err != nil {
			return nil, "", fault.InvalidRequest("invalid_value", "The continuation cursor is invalid.", &param)
		}
		stamp, id, ok := strings.Cut(string(decoded), "|")
		if !ok {
			anchor = resources.ListCursor{ID: stamp}
		} else {
			createdAt, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil || id == "" {
				return nil, "", fault.InvalidRequest("invalid_value", "The continuation cursor is invalid.", &param)
			}
			anchor = resources.ListCursor{ID: id, CreatedAt: &createdAt}
		}
	}
	page, next, pageErr := listedResourcePage(limit, anchor, fetch, authorize)
	if pageErr != nil || next.ID == "" || len(page) > 0 && page[len(page)-1].ID == next.ID {
		return page, next.ID, pageErr
	}
	codec, err := s.retainedListCursorCipher(owner)
	if err != nil {
		return nil, "", fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The continuation cursor is unavailable.")
	}
	payload := next.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + next.ID
	return page, RetainedListCursorPrefix + base64.RawURLEncoding.EncodeToString(codec.Seal(nil, nil, []byte(payload), nil)), nil
}

func listedResourcePage(limit int, anchor resources.ListCursor, fetch func(cursor resources.ListCursor, count int) ([]*resources.Resource, *fault.Error), authorize func(*resources.Resource) *fault.Error) ([]*resources.Resource, resources.ListCursor, *fault.Error) {
	page := make([]*resources.Resource, 0, limit)
	cursor := anchor
	for remaining := limit + 400; remaining > 0; {
		count := min(remaining, max(limit+1-len(page), 101))
		rows, fetchErr := fetch(cursor, count+1)
		if fetchErr != nil {
			return nil, resources.ListCursor{}, fetchErr
		}
		for _, row := range rows[:min(len(rows), count)] {
			cursor = resources.ListCursor{ID: row.ID, CreatedAt: &row.CreatedAt}
			if authErr := authorize(row); authErr != nil {
				if authErr.Status != http.StatusNotFound && authErr.Status != http.StatusConflict {
					return nil, resources.ListCursor{}, authErr
				}
				continue
			}
			if len(page) == limit {
				last := page[len(page)-1]
				return page, resources.ListCursor{ID: last.ID, CreatedAt: &last.CreatedAt}, nil
			}
			page = append(page, row)
		}
		if len(rows) <= count {
			return page, resources.ListCursor{}, nil
		}
		remaining -= count
	}
	return page, cursor, nil
}

// retainedUse is what a request does with a retained resource.
type RetainedUse int

const (
	// retainedHousekeeping lists, deletes or cancels a retained resource.
	RetainedHousekeeping RetainedUse = iota
	// retainedRetrieval reads a retained resource or its content.
	RetainedRetrieval
	// retainedNewWork starts provider work from a retained resource.
	RetainedNewWork
)

// RetainedContract serves a retained resource only under the contract it was
// created with. A strict resource is refused once its route is transformed,
// and a transformed one cannot start new work once its route is strict. The
// owner can always list, delete or cancel, so provider-held data can be
// removed and running upstream work stopped whatever the route promises now.
func RetainedContract(strict bool, current routing.RouteFidelity, use RetainedUse) *fault.Error {
	switch {
	case use == RetainedHousekeeping:
		return nil
	case strict && !current.Strict():
		return fault.ServerError(http.StatusConflict, "provider_resource_unavailable",
			"This strict resource is unavailable because its route is now transformed.")
	case !strict && current.Strict() && use == RetainedNewWork:
		return fault.ServerError(http.StatusConflict, "provider_resource_unavailable",
			"This resource was created under a transformed route and cannot start work now that the route is strict.")
	}
	return nil
}

// strictResourceKind reports whether a resource kind holds a strict contract.
// Gemini Interactions keep one kind under either fidelity, so their kind says
// nothing about the contract (known is false).
func strictResourceKind(kind string) (strict, known bool) {
	switch kind {
	case resources.KindStrictResponse, resources.KindStrictFile, resources.KindStrictBatch, resources.KindContinuation, "agent_resource":
		return true, true
	case resources.KindResponse, resources.KindFile, resources.KindBatch,
		resources.KindAnthropicFile, resources.KindAnthropicBatch, resources.KindGeminiUpload,
		resources.KindGeminiFile, resources.KindGeminiBatch, resources.KindCloudBatch:
		return false, true
	}
	return false, false
}

func retainedResourceContract(res *resources.Resource, original, current routing.RouteFidelity, use RetainedUse) *fault.Error {
	if res.Kind == resources.KindFile && RetainedFileOperation(res) != "batch" {
		return RetainedContract(original.Strict(), current, use)
	}
	if strict, known := strictResourceKind(res.Kind); known {
		return RetainedContract(strict, current, use)
	}
	switch res.Kind {
	case resources.KindInteraction, "conversation", "vector_store", "context_cache":
		// These kinds share a name under either fidelity. Their creation
		// route supplies the contract before a historical pin is restored.
		return RetainedContract(original.Strict(), current, use)
	}
	return nil
}

func (x *Call) retainedContentGate(resource *resources.Resource, route *routing.Route, use RetainedUse) *fault.Error {
	if use == RetainedRetrieval {
		switch resource.Kind {
		case resources.KindAnthropicFile, resources.KindAnthropicBatch, resources.KindGeminiFile, resources.KindGeminiBatch:
			return x.GuardrailSurfaceGate(route)
		case resources.KindFile:
			if adapter, err := ResourceAdapter(resource); err != nil || RetainedFileOperation(resource) != "batch" || adapter.Managed() {
				return x.GuardrailSurfaceGate(route)
			}
		case resources.KindBatch:
			if adapter, err := ResourceAdapter(resource); err != nil || adapter.Managed() {
				return x.GuardrailSurfaceGate(route)
			}
		}
	}
	return nil
}

// resolveResource rebuilds the provider pin that owns a retained resource and
// refuses a use its route no longer promises.
func (s *Server) ResolveResource(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, operation string, use RetainedUse) (*Pin, *routing.Route, *fault.Error) {
	if s.Deps.Resolver == nil || s.Deps.Resources == nil {
		return nil, nil, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Provider state is not configured on this installation.")
	}
	resolved, err := s.Deps.Resolver.ResolveCurrent(ctx, res, operation)
	if errors.Is(err, resources.ErrUnavailable) || errors.Is(err, resources.ErrNoRows) {
		return nil, nil, PinUnavailable()
	}
	if err != nil {
		return nil, nil, fault.ServerError(http.StatusInternalServerError, "internal_error", "The stored target could not be rebuilt.")
	}
	provider, route, slot := &resolved.Invocation.Provider, &resolved.Route, &resolved.Invocation.Slot
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) {
		return nil, nil, fault.NotFoundError("not_found", "No "+res.Kind+" with this identifier exists for this key.")
	}
	if current, published := x.request.release.Snapshot().FindRoute(res.RouteSlug); published {
		if use != RetainedHousekeeping {
			if e := s.CheckRouteFidelity(ctx, &current); e != nil {
				return nil, nil, e
			}
		}
		if e := retainedResourceContract(res, route.Fidelity, current.Fidelity, use); e != nil {
			return nil, nil, e
		}
	}
	if e := x.retainedContentGate(res, route, use); e != nil {
		return nil, nil, e
	}
	if e := x.CheckBody(route); e != nil {
		return nil, nil, e
	}
	target := &resolved.Target
	if target.ProviderModel != ResourceModel(res) {
		return nil, nil, PinUnavailable()
	}
	if use == RetainedNewWork {
		if e := restrictRetainedWork(x.request.release.Snapshot(), authority.ID, provider, route, slot, target, operation); e != nil {
			return nil, nil, e
		}
		if e := x.CheckBody(route); e != nil {
			return nil, nil, e
		}
	}
	attempt := routing.Attempt{
		TargetID:           target.ID,
		ProviderID:         provider.ID,
		ProviderRevisionID: provider.RevisionID,
		ProviderKind:       string(provider.Kind),
		UpstreamModel:      target.ProviderModel,
		Timeout:            time.Duration(target.Timeout) * time.Millisecond,
		VendorID:           provider.VendorID,
	}
	p := &Pin{Target: *target, Provider: *provider, attempt: attempt, Slot: *slot, Model: target.ProviderModel}
	return p, route, nil
}

// AdmitStoredResponse admits a key to a retained response and returns the pin
// that serves it. A strict response also needs provider state on the key and
// the serving identity its contract recorded.
func (s *Server) AdmitStoredResponse(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, contract *StoredResponseContract, use RetainedUse) (*Pin, *routing.Route, *fault.Error) {
	if contract != nil {
		current, ok := x.request.release.Snapshot().FindRoute(res.RouteSlug)
		if !ok || !authority.Policy.AllowProviderState || !authority.Allows("inference", current.Slug, current.ProjectID, s.Now()) {
			return nil, nil, fault.NotFoundError("not_found", "The stored response is unavailable to this key.")
		}
	}
	p, route, e := s.ResolveResource(ctx, x, authority, res, operationGeneration, use)
	if e != nil || contract == nil {
		return p, route, e
	}
	if !p.Provider.Enabled || !p.Slot.Allows(p.Model, res.RouteSlug, authority.ID) || p.Model != contract.Binding || p.Provider.RevisionID != contract.Receipt.Serving.RevisionID || p.Provider.ProfileID != contract.Receipt.ProfileID || p.Provider.ProfileRevision != contract.Receipt.ProfileRevision {
		return nil, nil, PinUnavailable()
	}
	if p.Provider.Network != nil && p.Provider.Network.CredentialID != "" {
		if _, err := s.providerNetworkSecret(ctx, x.request.release, &p.Provider); err != nil {
			return nil, nil, PinUnavailable()
		}
	}
	return p, route, nil
}

// authorizeResponseContract checks a key may use a strict retained response
// whose pin is resolved later.
func (s *Server) authorizeResponseContract(ctx context.Context, x *Call, authority keyauthority.Authority, res *resources.Resource, contract *StoredResponseContract, use RetainedUse) *fault.Error {
	_, _, e := s.AdmitStoredResponse(ctx, x, authority, res, contract, use)
	return e
}

// AdmitDurable admits a key to a retained file or batch and returns the pin
// that serves it. A strict one also needs provider state on the key and the
// serving identity its document recorded.
func (s *Server) AdmitDurable(ctx context.Context, x *Call, authority keyauthority.Authority, r *resources.Resource, doc *DurableDocument, operation string, use RetainedUse) (*Pin, *routing.Route, *fault.Error) {
	if doc != nil {
		current, ok := x.Snapshot().FindRoute(r.RouteSlug)
		if !ok || !authority.Policy.AllowProviderState || !authority.Allows("inference", current.Slug, current.ProjectID, s.Now()) {
			return nil, nil, fault.NotFoundError("not_found", "The stored provider resource is unavailable to this key.")
		}
	}
	p, route, e := s.ResolveResource(ctx, x, authority, r, operation, use)
	if e != nil || doc == nil {
		return p, route, e
	}
	profile, profileErr := p.Provider.Connector().Profile()
	if profileErr != nil || !p.Provider.Enabled || !p.Slot.Allows(p.Model, r.RouteSlug, authority.ID) || p.Model != doc.Binding || p.Provider.RevisionID != doc.Serving.RevisionID || profile.ID != doc.Serving.ProfileID || profile.Revision != doc.Serving.ProfileRevision {
		return nil, nil, PinUnavailable()
	}
	if p.Provider.Network != nil && p.Provider.Network.CredentialID != "" {
		if _, err := s.providerNetworkSecret(ctx, x.request.release, &p.Provider); err != nil {
			return nil, nil, PinUnavailable()
		}
	}
	return p, route, nil
}

// AuthorizeDurable checks a key may use a strict retained file or batch; a
// transformed one (doc is nil) is checked when its pin is resolved.
func (s *Server) AuthorizeDurable(ctx context.Context, x *Call, authority keyauthority.Authority, r *resources.Resource, doc *DurableDocument, operation string, use RetainedUse) *fault.Error {
	if doc == nil {
		return nil
	}
	_, _, e := s.AdmitDurable(ctx, x, authority, r, doc, operation, use)
	return e
}

// Historical protocol and credential identity do not grant historical authority
// to start fresh inference. Intersect that contract with the installed release.
func restrictRetainedWork(current *routing.View, keyID string, provider *routing.Provider, route *routing.Route, slot *routing.Slot, target *routing.Target, operation string) *fault.Error {
	live, ok := current.FindProvider(provider.ID)
	if !ok || !live.Enabled {
		return PinUnavailable()
	}
	liveRoute, ok := current.FindRoute(route.Slug)
	if !ok || liveRoute.ID != route.ID || !slices.Contains(liveRoute.Operations, operation) {
		return PinUnavailable()
	}
	foundTarget := false
	for i := range liveRoute.Targets {
		candidate := &liveRoute.Targets[i]
		if candidate.ProviderID == target.ProviderID && candidate.ProviderModel == target.ProviderModel && candidate.Shadow == nil {
			// The current target's timeout bounds the retained attempt; the
			// pinned contract can only tighten it further.
			if candidate.Timeout > 0 && (target.Timeout <= 0 || candidate.Timeout < target.Timeout) {
				target.Timeout = candidate.Timeout
			}
			foundTarget = true
			break
		}
	}
	if !foundTarget {
		return PinUnavailable()
	}
	var liveSlot *routing.Slot
	for _, candidate := range live.Slots {
		if candidate.ID == slot.ID {
			liveSlot = &candidate
			break
		}
	}
	if liveSlot == nil || !liveSlot.Allows(target.ProviderModel, route.Slug, keyID) || !slot.Allows(target.ProviderModel, route.Slug, keyID) {
		return PinUnavailable()
	}
	slot.RequestsPerMinute = tighterRetainedLimit(slot.RequestsPerMinute, liveSlot.RequestsPerMinute)
	slot.TokensPerMinute = tighterRetainedLimit(slot.TokensPerMinute, liveSlot.TokensPerMinute)
	slot.InputTokensPerMinute = tighterRetainedLimit(slot.InputTokensPerMinute, liveSlot.InputTokensPerMinute)
	slot.OutputTokensPerMinute = tighterRetainedLimit(slot.OutputTokensPerMinute, liveSlot.OutputTokensPerMinute)
	slot.MaxConcurrency = tighterRetainedLimit(slot.MaxConcurrency, liveSlot.MaxConcurrency)
	slot.Supply = tighterRetainedSupply(slot.Supply, liveSlot.Supply)
	if live.Limits != nil {
		prior := routing.Limits{}
		if provider.Limits != nil {
			prior = *provider.Limits
		}
		provider.Limits = &routing.Limits{
			RequestsPerMinute:     tighterRetainedLimit(prior.RequestsPerMinute, live.Limits.RequestsPerMinute),
			TokensPerMinute:       tighterRetainedLimit(prior.TokensPerMinute, live.Limits.TokensPerMinute),
			InputTokensPerMinute:  tighterRetainedLimit(prior.InputTokensPerMinute, live.Limits.InputTokensPerMinute),
			OutputTokensPerMinute: tighterRetainedLimit(prior.OutputTokensPerMinute, live.Limits.OutputTokensPerMinute),
			MaxConcurrency:        tighterRetainedLimit(prior.MaxConcurrency, live.Limits.MaxConcurrency),
			Supply:                tighterRetainedSupply(prior.Supply, live.Limits.Supply),
		}
	}
	route.ContentPolicy = liveRoute.ContentPolicy
	route.Policy = liveRoute.Policy
	route.Budget = tighterRetainedCosts(route.Budget, liveRoute.Budget)
	route.MaxBodyBytes = tighterRetainedLimit(route.MaxBodyBytes, liveRoute.MaxBodyBytes)
	route.CallerCostExempt = route.CallerCostExempt && liveRoute.CallerCostExempt
	route.OverallTimeout = min(route.OverallTimeout, liveRoute.OverallTimeout)
	route.MaxAttempts = min(route.MaxAttempts, liveRoute.MaxAttempts)
	return nil
}

func tighterRetainedLimit(old, current *int64) *int64 {
	if old == nil || current != nil && *current < *old {
		return current
	}
	return old
}

// Current priority shares govern current capacity allocation. Spend caps stay
// bounded by both the historical and current contracts, like token windows.
func tighterRetainedSupply(old, current routing.Supply) routing.Supply {
	current.DailyCostLimit = tighterRetainedCost(old.DailyCostLimit, current.DailyCostLimit)
	current.MonthlyCostLimit = tighterRetainedCost(old.MonthlyCostLimit, current.MonthlyCostLimit)
	return current
}

func tighterRetainedCosts(old, current *routing.CostLimits) *routing.CostLimits {
	if old == nil {
		return current
	}
	if current == nil {
		return old
	}
	return &routing.CostLimits{
		DailyCostLimit:   tighterRetainedCost(old.DailyCostLimit, current.DailyCostLimit),
		MonthlyCostLimit: tighterRetainedCost(old.MonthlyCostLimit, current.MonthlyCostLimit),
	}
}

func tighterRetainedCost(old, current *string) *string {
	if old == nil {
		return current
	}
	if current == nil {
		return old
	}
	before, beforeOK := new(big.Rat).SetString(*old)
	after, afterOK := new(big.Rat).SetString(*current)
	if !afterOK || beforeOK && before.Cmp(after) <= 0 {
		return old
	}
	return current
}
