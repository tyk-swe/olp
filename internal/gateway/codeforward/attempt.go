package codeforward

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"time"

	"github.com/tyk-swe/olp/internal/codesessions"

	"github.com/coder/websocket"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode/codewire"
	"github.com/tyk-swe/olp/internal/gateway/codeforward/codexwire"
	"github.com/tyk-swe/olp/internal/limits/admission"

	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
)

const codeGenerationTimeout = codemode.GenerationTimeout

type codeAttempt struct {
	identityOriginator string
	adapter            codemode.Adapter
	paid               *paidCodeAttempt
	forwarder          *Forwarder
	permit             codesessions.Permit
	config             routing.Configuration
	auth               codemode.Authorization
	lease              *admission.Lease
	providerLease      *admission.Lease
	observer           codewire.Observer
	endpoint           codeadapter.Endpoint
	dispatched         bool
	usage              codemode.Usage
	terminal           bool
	conflicting        bool
	prewarm            bool
	finished           bool
	model              string
	publicModel        string
	status             int
	responseHeaders    http.Header
	pendingHealth      *codemode.HealthObservation
}

func (f *Forwarder) prepareCode(req codeRequest, recovery *codesessions.Recovery) (*codeAttempt, error) {
	authority, operation, paidAttempt, err := f.resolveCodeIngress(req, recovery)
	if err != nil || paidAttempt != nil {
		return paidAttempt, err
	}
	providers := req.release.Snapshot().CodeModelProviders(req.route, req.inbound.path, operation.Model, req.transport)
	if len(providers) == 0 && len(req.route.PaidFallbackTargets) == 0 && (!req.route.ModelRewriting || len(req.release.Snapshot().CodeProviders(req.route, req.inbound.path, req.transport)) == 0) {
		return nil, codemode.Refuse(400, "code_operation_unsupported")
	}
	labels, failure := ingress.ParseAttribution(req.r, authority)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_"+failure.Code)
	}
	lease, failure := f.admission.ReserveCodeRate(req.r.Context(), authority, req.observation.Estimate, codeGenerationTimeout+time.Minute, req.route.Slug)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_rate_limited")
	}
	permit, fallbackAttempt, err := f.admitCodePermit(req, codePermitInput{
		operation: operation,
		authority: authority,
		labels:    labels,
		providers: providers,
		lease:     lease,
		recovery:  recovery,
	})
	if err != nil || fallbackAttempt != nil {
		return fallbackAttempt, err
	}
	model := operation.Model
	if permit.Pin.TargetModel != "" {
		model = permit.Pin.TargetModel
	}
	a := &codeAttempt{model: model, publicModel: operation.PublicModel(), forwarder: f, permit: permit, lease: lease, prewarm: req.observation.Operation.Name == "prewarm"}
	ok := false
	defer func() {
		if !ok {
			a.finish(req.r.Context())
		}
	}()
	if err := f.reconcileCodePermit(req.r, a, authority, labels, req.observation, req.route.Slug); err != nil {
		return nil, err
	}
	if err := f.bindCodeTarget(req, a, recovery, model); err != nil {
		return nil, err
	}
	ok = true
	return a, nil
}

func (a *codeAttempt) dispatch(ctx context.Context) error {
	if err := a.forwarder.ledger.MarkDispatched(ctx, a.permit.Attempt.ID); err != nil {
		return codemode.Refuse(503, "code_dispatch_unavailable")
	}
	a.dispatched = true
	return nil
}

func (a *codeAttempt) observeUnary(body []byte) {
	observation := a.observer.Unary(body)
	if a.status >= 400 && len(observation.RejectionBody) == 0 {
		a.observe(codemode.Observation{RejectionBody: codewire.RejectionBody(body)})
	}
	a.observe(observation)
	if a.status >= 400 {
		a.terminal = true
	}
}

func (a *codeAttempt) observeEncodedUnary(body []byte, encoding string) {
	decoded, err := codewire.Decode(body, encoding, codewire.MaxBody)
	if err == nil {
		a.observeUnary(decoded)
	}
}

func (a *codeAttempt) observe(o codemode.Observation) {
	if o.ResponseID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = a.forwarder.ledger.ObserveReference(ctx, a.permit.Attempt.ID, o.ResponseID)
		cancel()
	}
	classification, classified := a.observeCooldownClass(o)
	switch {
	case classified && classification.Scope == codeadapter.ScopeModel:
		a.pendingHealth = nil
	case len(o.RejectionBody) > 0:
		a.flushPendingHealth(context.Background())
	}
	if o.Status != 0 && !classified {
		a.response(context.Background(), o.Status, http.Header{"Retry-After": o.RetryAfter})
		a.flushPendingHealth(context.Background())
	}
	if o.Allowance != nil {
		a.observeAllowance(context.Background(), *o.Allowance)
	}
	if o.AllowanceCharge != nil {
		a.observeAllowanceCharge(context.Background(), o.AllowanceCharge)
	}
	if o.Outcome != nil {
		a.outcome(context.Background(), *o.Outcome)
	}
	if o.Successful && !a.prewarm {
		a.health(context.Background(), "healthy")
	}
	if !o.Terminal {
		return
	}
	if a.terminal && !reflect.DeepEqual(a.usage, o.Usage) {
		a.conflicting = true
	}
	a.terminal = true
	a.usage = o.Usage
	if a.conflicting {
		a.usage = codemode.Usage{}
	}
}

func (a *codeAttempt) finish(ctx context.Context) {
	if a.finished {
		return
	}
	a.finished = true
	a.flushPendingHealth(ctx)
	if !a.dispatched {
		a.outcome(ctx, codemode.Outcome{Origin: "gateway", Kind: "rejected"})
	} else if !a.terminal {
		outcome := codemode.Outcome{Origin: "gateway", Kind: "interrupted"}
		if errors.Is(ctx.Err(), context.Canceled) {
			outcome.Origin, outcome.Kind = "client", "canceled"
		}
		a.outcome(ctx, outcome)
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	var err error
	if a.dispatched {
		err = a.forwarder.ledger.Settle(ctx, a.permit.Attempt.ID, a.usage)
	} else {
		err = a.forwarder.ledger.Abort(ctx, a.permit.Attempt.ID)
		if err != nil {
			err = a.forwarder.ledger.Settle(ctx, a.permit.Attempt.ID, codemode.Usage{})
		}
	}
	if err != nil {
		a.forwarder.log.Warn("code accounting unavailable", "attempt_id", a.permit.Attempt.ID)
	}
	if a.paid != nil {
		a.finishPaid(ctx)
	}
	quota.SettleKey(ctx, a.lease, a.dispatched, a.usage.Total, a.forwarder.log)
	quota.SettleLease(ctx, a.providerLease, a.dispatched, settledCodeUsage(a.usage), a.forwarder.log)
}

func codeProviderDemand(request codemode.Request) quota.TokenDemand {
	return quota.GenerationDemand(request.Estimate-request.OutputCap, request.OutputCap, request.ChatChoices)
}

func settledCodeUsage(usage codemode.Usage) *admission.TokenUsage {
	if usage.Total == nil {
		return nil
	}
	if usage.Input == nil || usage.Output == nil {
		return &admission.TokenUsage{Total: *usage.Total}
	}
	cached := int64(0)
	if usage.Cached != nil && *usage.Cached <= *usage.Input {
		cached = *usage.Cached
	}
	return &admission.TokenUsage{Input: *usage.Input - cached, Output: *usage.Output, Total: *usage.Total, Credit: true}
}

func (a *codeAttempt) outcome(ctx context.Context, outcome codemode.Outcome) {
	if a.permit.Attempt.ID == "" {
		return
	}
	if outcome.ObservedAt.IsZero() {
		outcome.ObservedAt = a.forwarder.now().UTC()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := a.forwarder.ledger.ObserveOutcome(ctx, a.permit.Attempt.ID, outcome); err != nil {
		a.forwarder.log.Warn("code outcome unavailable", "attempt_id", a.permit.Attempt.ID)
	}
}

// transportFailed records a gateway transport failure. Only an upstream close
// that signals the peer's own failure or unavailability cools the account;
// transport noise, like a canceled or expired request, is not the account's
// failure and cannot prove unavailability.
func (a *codeAttempt) transportFailed(ctx context.Context, err error) {
	if ctx.Err() != nil {
		return
	}
	a.outcome(ctx, codemode.Outcome{Origin: "gateway", Kind: "transport_error"})
	if codeUnavailableClose(err) {
		a.health(ctx, "unavailable")
	}
}

// codeUnavailableClose reports whether err is an upstream WebSocket close whose
// code signals the peer's own failure (internal error, restart, try-again or
// bad gateway) rather than transport noise.
func codeUnavailableClose(err error) bool {
	code := websocket.CloseStatus(err)
	return code >= websocket.StatusInternalError && code <= websocket.StatusBadGateway
}

func (a *codeAttempt) observeAllowanceCharge(ctx context.Context, charge json.RawMessage) {
	if a.paid != nil || a.adapter != codemode.AdapterOpenCodeGo || a.permit.Attempt.ID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = a.forwarder.ledger.ObserveAllowanceCharge(ctx, a.permit.Attempt.ID, charge)
}

func (a *codeAttempt) observeAllowance(ctx context.Context, allowance codemode.Allowance) {
	if a.paid != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = a.forwarder.ledger.ObserveAllowance(ctx, a.permit.Account.ID, allowance)
}

func (a *codeAttempt) health(ctx context.Context, status string) {
	a.observeHealth(ctx, codemode.HealthObservation{Status: status, ObservedAt: a.forwarder.now().UTC()})
}

func (a *codeAttempt) observeHealth(ctx context.Context, observation codemode.HealthObservation) {
	if a.paid != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = a.forwarder.ledger.ObserveHealth(ctx, a.permit.Account.ID, observation)
}

func (a *codeAttempt) flushPendingHealth(ctx context.Context) {
	if a.pendingHealth == nil {
		return
	}
	pending := *a.pendingHealth
	a.pendingHealth = nil
	a.observeHealth(ctx, pending)
}

func (a *codeAttempt) observeCooldownClass(o codemode.Observation) (codeadapter.Classification, bool) {
	if a.paid != nil || len(o.RejectionBody) == 0 || a.permit.Account.ID == "" {
		return codeadapter.Classification{}, false
	}
	adapter, ok := codeadapter.ForConnection(a.config.Kind, a.config.AuthMode, a.config.ProfileID)
	if !ok {
		return codeadapter.Classification{}, false
	}
	status := o.Status
	if status == 0 {
		status = a.status
	}
	headers := http.Header{"Retry-After": o.RetryAfter}
	if len(o.RetryAfter) == 0 && a.responseHeaders != nil {
		headers = a.responseHeaders
	}
	now := a.forwarder.now().UTC()
	classification, ok := adapter.Classify(a.endpoint, status, o.RejectionBody, headers, now)
	if !ok {
		return codeadapter.Classification{}, false
	}
	if classification.SuppressFallback {
		a.pendingHealth = nil
		return classification, true
	}
	classification = codeadapter.WithCycleReset(classification, a.permit.Account.CycleAnchor, now)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := a.forwarder.ledger.ObserveCooldownClass(ctx, a.permit.Account.ID, a.model, classification, now)
	return classification, err == nil
}

func (a *codeAttempt) response(ctx context.Context, status int, headers http.Header) {
	a.status = status
	if headers != nil {
		a.responseHeaders = headers.Clone()
	}
	if a.paid != nil {
		a.paid.status = status
	}
	outcome := codemode.Outcome{Origin: "upstream", Kind: "headers", UpstreamStatus: &status}
	if status >= 400 {
		outcome.Kind = "rejected"
	}
	a.outcome(ctx, outcome)
	if status == http.StatusUnauthorized {
		a.forwarder.runtime.CredentialRefused(a.auth.CredentialID, a.auth.GrantGeneration)
	}
	if status == 429 || status == 401 || status == 403 || status >= 500 {
		health := func() string {
			if status == 429 {
				return "quota_limited"
			}
			return "unavailable"
		}()
		now := a.forwarder.now().UTC()
		retryAt := func() *time.Time {
			values := headers.Values("Retry-After")
			if len(values) != 1 {
				return nil
			}
			delay, valid := upstream.ParseRetryAfter(values[0], now)
			if !valid {
				return nil
			}
			at := now.Add(delay)
			return &at
		}()
		a.flushPendingHealth(ctx)
		a.pendingHealth = &codemode.HealthObservation{Status: health, ObservedAt: now, RetryAt: retryAt}
	}
	if a.endpoint.Observation != codeadapter.ObserveCodex {
		return
	}
	if allowance := codexwire.Allowance(headers, a.forwarder.now()); allowance != nil {
		a.observeAllowance(ctx, *allowance)
	}
}

// A durable permit reads current issuer state. It may retain the verified claim
// digest only while the same declared issuer revision still owns the principal.
func bindCodeWorkload(fresh *keyauthority.Authority, verified keyauthority.Authority) error {
	if fresh.WorkloadIssuerID == nil && verified.WorkloadIssuerID == nil {
		return nil
	}
	if fresh.WorkloadIssuerID == nil || verified.WorkloadIssuerID == nil || *fresh.WorkloadIssuerID != *verified.WorkloadIssuerID || fresh.WorkloadDigest != verified.WorkloadDigest || fresh.WorkloadMapping != verified.WorkloadMapping || fresh.WorkloadRevision != verified.WorkloadRevision {
		return codemode.Refuse(409, "code_workload_changed")
	}
	fresh.EndUserDigest = verified.EndUserDigest
	fresh.ExpiresAt = verified.ExpiresAt
	return nil
}
