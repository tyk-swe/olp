package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/proxy"
	"github.com/tyk-swe/olp/internal/code/research"
	codewire "github.com/tyk-swe/olp/internal/code/wire"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type CodeDependencies struct {
	Ledger     proxy.Ledger
	Authorizer interface {
		proxy.Authorizer
		research.Authorizer
	}
	Research research.Ledger
}

func (s *Server) registerCode(mux *http.ServeMux) {
	if s.deps.Code == nil || s.deps.Code.Ledger == nil || s.deps.Code.Authorizer == nil {
		return
	}
	host := &codeHost{server: s}
	var researchHandler http.Handler
	if s.deps.Code.Research != nil {
		researchHandler = &research.Handler{
			Host:       host,
			Ledger:     s.deps.Code.Research,
			Authorizer: s.deps.Code.Authorizer,
		}
	}
	(&proxy.Server{
		Host:       host,
		Ledger:     s.deps.Code.Ledger,
		Authorizer: s.deps.Code.Authorizer,
		Research:   researchHandler,
	}).Register(mux)
}

type codeHost struct {
	server *Server
}

func (h *codeHost) AdmitInFlight(ctx context.Context) bool { return h.server.admit(ctx) }
func (h *codeHost) ReleaseInFlight(ctx context.Context)    { h.server.release(ctx) }

func (h *codeHost) OriginAllowed(r *http.Request) bool {
	values := r.Header.Values("Origin")
	return len(values) == 0 || len(values) == 1 && sameOrigin(r, values[0])
}

func (h *codeHost) Authenticate(r *http.Request) (keyauthority.Authority, int, bool) {
	a, failure := h.server.authenticate(r, "inference")
	if failure != nil {
		return keyauthority.Authority{}, failure.Status, false
	}
	return a, 0, true
}

func (h *codeHost) AuthenticatePresentedKey(r *http.Request) (keyauthority.Authority, int, bool) {
	token, failure := presentedKey(r)
	if failure != nil {
		return keyauthority.Authority{}, failure.Status, false
	}
	a, failure := h.server.authenticateKey(r, token, "inference")
	if failure != nil {
		return keyauthority.Authority{}, failure.Status, false
	}
	return a, 0, true
}

func (h *codeHost) ParseAttribution(r *http.Request, authority keyauthority.Authority) (map[string]string, int, string, bool) {
	labels, failure := h.server.parseAttribution(r, authority)
	if failure != nil {
		return nil, failure.Status, failure.Code, false
	}
	return labels, 0, "", true
}

func (h *codeHost) CheckKeyAddress(r *http.Request, authority keyauthority.Authority) (int, bool) {
	if failure := h.server.checkKeyAddress(r, authority); failure != nil {
		return failure.Status, false
	}
	return 0, true
}

func (h *codeHost) IdentifyEndUser(r *http.Request, authority keyauthority.Authority) (keyauthority.Authority, int, bool) {
	identified, failure := h.server.identifyEndUser(r, authority)
	if failure != nil {
		return keyauthority.Authority{}, failure.Status, false
	}
	return identified, 0, true
}

func (h *codeHost) CheckResearchCaller(r *http.Request, fresh *keyauthority.Authority, verified keyauthority.Authority) error {
	if err := proxy.BindWorkload(fresh, verified); err != nil {
		return err
	}
	failure := h.server.checkKeyAddress(r, *fresh)
	if failure == nil {
		*fresh, failure = h.server.identifyEndUser(r, *fresh)
	}
	if failure != nil {
		return code.Refuse(failure.Status, "code_permission_denied")
	}
	if !fresh.AllowsEndUser(fresh.EndUserDigest) {
		return code.Refuse(403, "code_permission_denied")
	}
	if _, failure = h.server.parseAttribution(r, *fresh); failure != nil {
		return code.Refuse(failure.Status, "code_invalid_attribution")
	}
	return nil
}

func (h *codeHost) Release() *release.Release           { return h.server.deps.Runtime.Release() }
func (h *codeHost) RoutingInputs() *usage.RoutingInputs { return h.server.deps.Runtime.RoutingInputs() }
func (h *codeHost) IsCredentialEligible(id string) bool {
	return h.server.deps.Runtime.Eligibility(id) == routing.Eligible
}
func (h *codeHost) Secret(ctx context.Context, rel *release.Release, id string) ([]byte, int64, error) {
	return h.server.deps.Runtime.Secret(ctx, rel, id)
}
func (h *codeHost) CredentialRefused(id string, generation int64) {
	h.server.deps.Runtime.CredentialRefused(id, generation)
}
func (h *codeHost) ApplyConnectorAuth(ctx context.Context, r *http.Request, connector connectors.Config, secret, body []byte) (egress.Sensitive, error) {
	return h.server.auth.Apply(ctx, r, connector, secret, body)
}
func (h *codeHost) RedactFailureDocument(doc oif.Document, sensitive egress.Sensitive) (oif.Document, error) {
	return redactNativeFailureDocument(doc, sensitive)
}

func (h *codeHost) ReserveRate(ctx context.Context, authority keyauthority.Authority, estimate int64, ttl time.Duration, slug string) (*limitadmission.Lease, int, bool) {
	lease, failure := h.server.deps.Admission.ReserveCodeRate(ctx, authority, estimate, ttl, slug)
	if failure != nil {
		return nil, failure.Status, false
	}
	return lease, 0, true
}

func (h *codeHost) ReserveProvider(ctx context.Context, providerID string, cfg routing.Configuration, estimate int64, ttl time.Duration) (*limitadmission.Lease, int, bool) {
	lease, failure := h.server.deps.Admission.reserveCodeProvider(ctx, providerID, cfg, estimate, ttl)
	if failure != nil {
		return nil, failure.Status, false
	}
	return lease, 0, true
}

func (h *codeHost) ReserveStream(ctx context.Context, authority keyauthority.Authority, slug, service, providerID string, limits *routing.Limits) (*limitadmission.Lease, int, bool) {
	lease, failure := h.server.deps.Admission.reserveResearchStream(ctx, authority, slug, service, providerID, limits)
	if failure != nil {
		return nil, failure.Status, false
	}
	return lease, 0, true
}

type targetReservationLease struct{ inner *targetReservation }

func (t targetReservationLease) Settle(ctx context.Context, dispatched bool, tokens *int64) {
	t.inner.settle(ctx, dispatched, tokens)
}

func (h *codeHost) ReservePaid(ctx context.Context, authority *keyauthority.Authority, route code.Route, provider routing.Provider, slot routing.Slot, tokens int64, ttl time.Duration, requestID, cost, charge string) (*limitadmission.Lease, proxy.PaidTargetLease, []*limitadmission.Lease, error) {
	hold := costReservation{amount: cost, charge: charge, requestID: requestID}
	lease, failure := h.server.deps.Admission.reserveKeyCosted(ctx, authority, "code", tokens, ttl, hold, route.Slug)
	if failure != nil {
		return nil, nil, nil, code.Refuse(failure.Status, "code_"+failure.Code)
	}
	targetLease, rejection, skip := h.server.deps.Admission.reserveTarget(ctx, &provider, &slot, tokens, ttl, "code")
	if rejection != nil {
		return lease, nil, nil, code.Refuse(429, "code_paid_target_rate_limited")
	}
	if skip {
		return lease, nil, nil, code.Refuse(503, "code_limits_unavailable")
	}
	owners := slices.Collect(func(yield func(cappedOwner) bool) {
		for index, owner := range attemptCaps(&routing.Route{}, &provider, &slot) {
			if index > 0 && !yield(owner) {
				return
			}
		}
	})
	supply, err := h.reservePaidCaps(ctx, owners, hold, ttl)
	return lease, targetReservationLease{inner: targetLease}, supply, err
}

func (h *codeHost) reservePaidCaps(ctx context.Context, owners []cappedOwner, hold costReservation, ttl time.Duration) ([]*limitadmission.Lease, error) {
	if len(owners) == 0 {
		return nil, nil
	}
	owner := owners[0]
	if owner.limits == nil {
		return h.reservePaidCaps(ctx, owners[1:], hold, ttl)
	}
	if failure := h.server.deps.Admission.checkCostAccounting(ctx); failure != nil {
		return nil, code.Refuse(failure.Status, "code_"+failure.Code)
	}
	if failure := h.server.deps.Admission.checkPendingCostAccounting(ctx, owner.id); failure != nil {
		return nil, code.Refuse(failure.Status, "code_"+failure.Code)
	}
	if !h.server.deps.Admission.ready() {
		return nil, code.Refuse(503, "code_limits_unavailable")
	}
	lease, err := h.server.deps.Admission.limiter.Reserve(ctx, hold.on(capRequest(owner, ttl)))
	if _, ok := errors.AsType[*limits.ExceededError](err); ok {
		return nil, code.Refuse(429, "code_budget_exhausted")
	}
	if err != nil {
		return nil, code.Refuse(503, "code_limits_unavailable")
	}
	tail, tailErr := h.reservePaidCaps(ctx, owners[1:], hold, ttl)
	return append([]*limitadmission.Lease{lease}, tail...), tailErr
}

func (h *codeHost) SettleLease(ctx context.Context, lease *limitadmission.Lease, dispatched bool, tokens *int64) {
	settleKey(ctx, lease, dispatched, tokens, h.server.log)
}

func (h *codeHost) RecordPaidTerminal(t proxy.PaidTerminal) {
	if h.server.deps.Sink == nil {
		return
	}
	var cardID *string
	if t.Card != nil {
		cardID = &t.Card.ID
	}
	mode := "unary"
	if t.Stream {
		mode = "streaming"
	}
	outcome := "failure"
	if t.Status >= 200 && t.Status < 300 {
		outcome = "success"
	}
	duration := t.CompletedAt.Sub(t.StartedAt)
	fact := AttemptFact{
		VendorID: t.Provider.VendorID, Class: outcome, TargetID: t.Slot.ID, ProviderID: t.Provider.ID,
		ProviderRevisionID: t.Provider.RevisionID, UpstreamModel: t.Model, SlotID: t.Slot.ID,
		CredentialID: *t.Slot.CredentialID, CredentialVersion: *t.Slot.CredentialVersion,
		Price: t.Price, PricingPinned: true, Discount: t.Discount, CardPinned: true,
		cardRev: t.Card, CardRevisionID: cardID, Budgets: []string{t.Route.ID, t.Provider.ID, t.Slot.ID},
		Ordinal: 1, Mode: mode, StartedAt: t.StartedAt, Duration: duration, Status: t.Status,
		Usage: t.Usage, UsageObserved: t.UsageObserved, UsageComplete: t.UsageComplete, BillingUncertain: t.BillingUncertain,
	}
	h.server.deps.Sink.Terminal(Envelope{
		RequestID: t.AttemptID, AccountingID: t.AttemptID, KeyID: t.Authority.ID,
		EndUserDigest: t.Authority.EndUserDigest, BudgetGroupID: t.Authority.BudgetGroupID,
		Attribution: t.Authority.Attribution, Route: t.Route.Slug, RouteRevisionID: t.Route.RevisionID,
		RuntimeGenerationID: t.RuntimeGenerationID, ReleaseSequence: t.ReleaseSequence,
		Family: string(t.Family), Operation: "generation", Surface: t.Family.Surface(), Mode: mode,
		Outcome: outcome, Status: t.Status, Committed: true, StartedAt: t.StartedAt,
		CompletedAt: t.CompletedAt, Duration: duration, Usage: t.Usage, Attempts: []AttemptFact{fact},
	})
}

func (h *codeHost) ValidateEndpoint(target string) error {
	_, err := h.server.egress.ValidateEndpoint(target)
	return err
}

func (h *codeHost) ConnectionClient(ctx context.Context, rel *release.Release, cfg routing.Configuration, providerID string) (*http.Client, error) {
	var secret []byte
	if network := cfg.Options.Network; network != nil && network.CredentialID != "" {
		var err error
		secret, err = h.server.deps.Runtime.NetworkSecret(ctx, rel, providerID, network.CredentialID)
		if err != nil {
			return nil, code.Refuse(503, "code_network_credential_unavailable")
		}
	}
	client, err := h.server.egress.RawConnectionClient(cfg.Options.Network, secret, UpstreamHeaderTimeout)
	if err != nil {
		return nil, code.Refuse(502, "code_connection_invalid")
	}
	return client, nil
}

type codeStreamWriter struct{ inner *streamWriter }

func (w codeStreamWriter) Emit(chunk []byte) error { return w.inner.emit(chunk) }
func (w codeStreamWriter) Fail(status int, errCode string, cancelled bool) {
	w.inner.finish(&outcome{err: &Error{Status: status, Code: errCode, Type: "api_error", Message: errCode}, cancelled: cancelled})
}

func (h *codeHost) NewStreamWriter(w http.ResponseWriter, family wire.Format) proxy.StreamWriter {
	return codeStreamWriter{inner: &streamWriter{w: w, family: family}}
}

func (h *codeHost) WriteError(w http.ResponseWriter, r *http.Request, err error) {
	h.WriteRefusal(w, r, proxy.RefusalFromError(err))
}

func (h *codeHost) WriteRefusal(w http.ResponseWriter, r *http.Request, refusal *code.Refusal) {
	writeSurfaceError(w, h.surfaceError(r, refusal), requestSurface(r))
}

func (h *codeHost) WriteTranslatedRejection(w http.ResponseWriter, r *http.Request, refusal *code.Refusal) {
	surface := requestSurface(r)
	surfaceErr := h.surfaceError(r, refusal)
	w.Header().Set("Content-Length", strconv.Itoa(len(surfaceErr.surfaceBody(surface))))
	writeSurfaceError(w, surfaceErr, surface)
}

func (h *codeHost) surfaceError(r *http.Request, refusal *code.Refusal) *Error {
	surface, kind := requestSurface(r), "code_mode_error"
	if surface == "anthropic" {
		switch refusal.Status {
		case 400, 409, 413, 415, 422:
			kind = "invalid_request_error"
		case 401:
			kind = "authentication_error"
		case 403:
			kind = "permission_error"
		}
	}
	return &Error{Status: refusal.Status, Code: refusal.Code, Type: kind, Message: proxy.RefusalMessage(refusal.Code), RetryAfter: refusal.RetryAfter(h.Now())}
}

func (h *codeHost) BodyLimit() int64 {
	if h.server.cfg.MaxBodyBytes > 0 {
		return min(codewire.MaxBody, h.server.cfg.MaxBodyBytes)
	}
	return codewire.MaxBody
}

func (h *codeHost) Now() time.Time    { return h.server.now() }
func (h *codeHost) Log() *slog.Logger { return h.server.log }

func withoutCostBudgets(authority keyauthority.Authority) keyauthority.Authority {
	return research.WithoutCostBudgets(authority)
}

func researchRateAuthority(authority keyauthority.Authority, control bool) keyauthority.Authority {
	return research.RateAuthority(authority, control)
}

func (a *Admission) ReserveCodeRate(ctx context.Context, authority keyauthority.Authority, estimate int64, ttl time.Duration, route ...string) (*limitadmission.Lease, *Error) {
	return a.reserveKey(ctx, research.WithoutCostBudgets(authority), "code", estimate, ttl, route...)
}

func (a *Admission) reserveCodeProvider(ctx context.Context, providerID string, configuration routing.Configuration, estimate int64, ttl time.Duration) (*limitadmission.Lease, *Error) {
	request := connectionRequest(&routing.Provider{ID: providerID, Limits: configuration.Options.Limits}, estimate, ttl, "")
	if !request.HasHardLimits() {
		return nil, nil
	}
	if !a.ready() {
		return nil, limitsUnavailable()
	}
	step, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	lease, err := a.limiter.Reserve(step, request)
	if err == nil {
		return lease, nil
	}
	if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
		return nil, rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate)
	}
	return nil, limitsUnavailable()
}

func (a *Admission) reserveResearchStream(ctx context.Context, authority keyauthority.Authority, route, service, provider string, providerLimits *routing.Limits) (*limitadmission.Lease, *Error) {
	type boundary struct {
		scope    string
		max      *int64
		provider bool
	}
	boundaries := []boundary{
		{scope: "key:" + authority.LimitsLookup(), max: authority.Policy.MaxConcurrency},
		{scope: "route:" + authority.ID + ":" + route, max: authority.Policy.RouteLimits[route].MaxConcurrency},
		{scope: "key-end-user:" + authority.ID + ":" + authority.EndUserDigest, max: authority.Policy.EndUserPolicy.Limits(authority.EndUserDigest).MaxConcurrency},
	}
	if authority.BudgetGroupID != nil {
		boundaries = append(boundaries, boundary{scope: "group:" + *authority.BudgetGroupID, max: authority.BudgetGroupConcurrency})
	}
	if authority.ProjectID != nil {
		boundaries = append(boundaries, boundary{scope: "project-end-user:" + *authority.ProjectID + ":" + authority.EndUserDigest, max: authority.ProjectEndUserPolicy.Limits(authority.EndUserDigest).MaxConcurrency})
	}
	if providerLimits != nil {
		boundaries = append(boundaries, boundary{scope: "provider:" + provider, max: providerLimits.MaxConcurrency, provider: true})
	}
	var chain *limitadmission.Lease
	for _, boundary := range boundaries {
		if boundary.max == nil {
			continue
		}
		refuse := func(failure *Error) (*limitadmission.Lease, *Error) {
			settleKey(ctx, chain, false, nil, a.logger())
			return nil, failure
		}
		if !a.ready() {
			return refuse(limitsUnavailable())
		}
		digest := sha256.Sum256([]byte(service + "\x00" + boundary.scope))
		request := limitadmission.Request{Lookup: "rs_" + hex.EncodeToString(digest[:16]), LeaseTTL: research.Timeout + reserveTimeout, Rate: limitadmission.RateLimits{MaxConcurrency: boundary.max}}
		decision, cancel := context.WithTimeout(ctx, reserveTimeout)
		lease, err := a.limiter.Reserve(decision, request)
		cancel()
		if err != nil {
			if exceeded, ok := errors.AsType[*limits.ExceededError](err); ok {
				return refuse(rateLimited(exceeded.Dimension, exceeded.RetryAfter, exceeded.Estimate))
			}
			if boundary.provider {
				return refuse(limitsUnavailable())
			}
			if failure := a.outage(authority.ID, false, err); failure != nil {
				return refuse(failure)
			}
			continue
		}
		lease.Join(chain)
		chain = lease
	}
	return chain, nil
}
