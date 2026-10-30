package gateway

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"reflect"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/codexwire"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/upstream"
)

const codeGenerationTimeout = 10 * time.Minute

type codeAttempt struct {
	server        *Server
	permit        resources.CodePermit
	config        routing.Configuration
	auth          codemode.Authorization
	lease         *limits.Lease
	providerLease *limits.Lease
	observer      codewire.Observer
	endpoint      codeadapter.Endpoint
	dispatched    bool
	usage         codemode.Usage
	terminal      bool
	conflicting   bool
	prewarm       bool
	finished      bool
	model         string
	publicModel   string
}

func (s *Server) prepareCode(r *http.Request, release *runtime.Release, route codemode.Route, observation codemode.Request, ingress codeIngress, transport codeadapter.Transport, recovery *resources.CodeRecovery) (*codeAttempt, error) {
	authority, failure := s.authenticate(r, "inference")
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_authentication_refused")
	}
	if !authority.Allows("inference", route.Slug, &route.ProjectID, s.now()) {
		return nil, codemode.Refuse(403, "code_permission_denied")
	}
	operation, err := route.ResolveModel(observation.Operation)
	if err != nil {
		return nil, err
	}
	providers := release.Snapshot.CodeModelProviders(route, ingress.path, operation.Model, transport)
	if len(providers) == 0 && (!route.ModelRewriting || len(release.Snapshot.CodeProviders(route, ingress.path, transport)) == 0) {
		return nil, codemode.Refuse(400, "code_operation_unsupported")
	}
	modelProviders := func() map[string][]string {
		if !route.ModelRewriting {
			return nil
		}
		return maps.Collect(func(yield func(string, []string) bool) {
			for _, model := range route.Models {
				if !yield(model, release.Snapshot.CodeModelProviders(route, ingress.path, model, transport)) {
					return
				}
			}
		})
	}()
	labels, failure := s.parseAttribution(r, authority)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_"+failure.Code)
	}
	lease, failure := s.Admission.ReserveCodeRate(r.Context(), authority, observation.Estimate, codeGenerationTimeout+time.Minute, route.Slug)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_rate_limited")
	}
	permit, err := s.CodeLedger.Admit(r.Context(), resources.CodeAdmission{
		Attribution: labels, Route: route, APIKeyID: authority.ID, EndUserDigest: authority.EndUserDigest,
		InitialRecovery: observation.Recoverable, Recovery: recovery, Operation: operation, Providers: providers, ModelProviders: modelProviders, PreviousResponse: observation.PreviousResponse,
	})
	if err != nil {
		settleKey(r.Context(), lease, false, nil, s.log)
		return nil, err
	}
	model := func() string {
		if permit.Pin.TargetModel != "" {
			return permit.Pin.TargetModel
		}
		return operation.Model
	}()
	a := &codeAttempt{model: model, publicModel: operation.PublicModel(), server: s, permit: permit, lease: lease, prewarm: observation.Operation.Name == "prewarm"}
	ok := false
	defer func() {
		if !ok {
			a.finish(r.Context())
		}
	}()
	if err := bindCodeWorkload(&permit.Authority, authority); err != nil {
		return nil, err
	}
	a.permit.Authority = permit.Authority
	if !permit.Authority.AllowsAttribution(labels) {
		return nil, codemode.Refuse(409, "code_attribution_changed")
	}
	if !reflect.DeepEqual(authority.Policy, permit.Authority.Policy) || !reflect.DeepEqual(authority.ProjectEndUserPolicy, permit.Authority.ProjectEndUserPolicy) || !reflect.DeepEqual(authority.BudgetGroupID, permit.Authority.BudgetGroupID) || !reflect.DeepEqual(authority.GroupLimits(), permit.Authority.GroupLimits()) {
		settleKey(r.Context(), a.lease, false, nil, s.log)
		a.lease = nil
		fresh := permit.Authority
		if failure = s.checkKeyAddress(r, fresh); failure != nil {
			return nil, codemode.Refuse(failure.Status, "code_ip_not_allowed")
		}
		fresh, failure = s.identifyEndUser(r, fresh)
		if failure != nil {
			return nil, codemode.Refuse(failure.Status, "code_end_user_invalid")
		}
		if fresh.EndUserDigest != authority.EndUserDigest {
			return nil, codemode.Refuse(409, "code_end_user_changed")
		}
		a.lease, failure = s.Admission.ReserveCodeRate(r.Context(), fresh, observation.Estimate, codeGenerationTimeout+time.Minute, route.Slug)
		if failure != nil {
			return nil, codemode.Refuse(failure.Status, "code_rate_limited")
		}
	}
	if permit.Pin.AccountID != permit.Account.ID || permit.Pin.Principal != permit.Account.Principal || permit.Authority.ID != authority.ID {
		return nil, codemode.Refuse(503, "code_binding_invalid")
	}
	target, err := resolveCodeTarget(release.Snapshot, route, permit.Account.ProviderID, ingress, transport)
	if err != nil {
		return nil, err
	}
	if !target.adapter.ServesModel(target.endpoint.Protocol, model) {
		return nil, codemode.Refuse(503, "code_configuration_unsupported")
	}
	a.config, a.endpoint, a.observer = target.config, target.endpoint, codeObserver(target.endpoint)
	a.providerLease, failure = s.Admission.reserveCodeProvider(r.Context(), permit.Account.ProviderID, target.config, observation.Estimate, codeGenerationTimeout+time.Minute)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_provider_rate_limited")
	}
	a.auth, err = s.CodeAuthorizer.AuthorizeCode(r.Context(), target.config, permit.Account, target.dispatchDescriptor())
	if err != nil {
		return nil, codemode.Refuse(503, "code_account_unavailable")
	}
	if err := target.adapter.ValidateAuthorization(target.endpoint, a.auth, permit.Pin.Principal); err != nil {
		return nil, err
	}
	if recovery != nil && recovery.AccountID != "" && (a.auth.CredentialID != recovery.CredentialID || a.auth.GrantGeneration <= recovery.GrantGeneration) {
		return nil, codemode.Refuse(409, "code_recovery_unsafe")
	}
	ok = true
	return a, nil
}

func (a *codeAttempt) dispatch(ctx context.Context) error {
	if err := a.server.CodeLedger.MarkDispatched(ctx, a.permit.Attempt.ID); err != nil {
		return codemode.Refuse(503, "code_dispatch_unavailable")
	}
	a.dispatched = true
	return nil
}

func (a *codeAttempt) observe(o codemode.Observation) {
	if o.ResponseID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = a.server.CodeLedger.ObserveReference(ctx, a.permit.Attempt.ID, o.ResponseID)
		cancel()
	}
	if o.Status != 0 {
		a.response(context.Background(), o.Status, http.Header{"Retry-After": o.RetryAfter})
	}
	if o.Allowance != nil {
		a.observeAllowance(context.Background(), *o.Allowance)
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
		err = a.server.CodeLedger.Settle(ctx, a.permit.Attempt.ID, a.usage)
	} else {
		err = a.server.CodeLedger.Abort(ctx, a.permit.Attempt.ID)
		if err != nil {
			err = a.server.CodeLedger.Settle(ctx, a.permit.Attempt.ID, codemode.Usage{})
		}
	}
	if err != nil {
		a.server.log.Warn("code accounting unavailable", "attempt_id", a.permit.Attempt.ID)
	}
	settleKey(ctx, a.lease, a.dispatched, a.usage.Total, a.server.log)
	settleKey(ctx, a.providerLease, a.dispatched, a.usage.Total, a.server.log)
}

func (a *codeAttempt) outcome(ctx context.Context, outcome codemode.Outcome) {
	if a.permit.Attempt.ID == "" {
		return
	}
	if outcome.ObservedAt.IsZero() {
		outcome.ObservedAt = a.server.now().UTC()
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := a.server.CodeLedger.ObserveOutcome(ctx, a.permit.Attempt.ID, outcome); err != nil {
		a.server.log.Warn("code outcome unavailable", "attempt_id", a.permit.Attempt.ID)
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

func (a *codeAttempt) observeAllowance(ctx context.Context, allowance codemode.Allowance) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = a.server.CodeLedger.ObserveAllowance(ctx, a.permit.Account.ID, allowance)
}

func (a *codeAttempt) health(ctx context.Context, status string) {
	a.observeHealth(ctx, codemode.HealthObservation{Status: status, ObservedAt: a.server.now().UTC()})
}

func (a *codeAttempt) observeHealth(ctx context.Context, observation codemode.HealthObservation) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	_ = a.server.CodeLedger.ObserveHealth(ctx, a.permit.Account.ID, observation)
}

func (a *codeAttempt) response(ctx context.Context, status int, headers http.Header) {
	outcome := codemode.Outcome{Origin: "upstream", Kind: "headers", UpstreamStatus: &status}
	if status >= 400 {
		outcome.Kind = "rejected"
	}
	a.outcome(ctx, outcome)
	if status == http.StatusUnauthorized {
		a.server.Runtime.CredentialRefused(a.auth.CredentialID, a.auth.GrantGeneration)
	}
	if status == 429 || status == 401 || status == 403 || status >= 500 {
		health := func() string {
			if status == 429 {
				return "quota_limited"
			}
			return "unavailable"
		}()
		now := a.server.now().UTC()
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
		a.observeHealth(ctx, codemode.HealthObservation{Status: health, ObservedAt: now, RetryAt: retryAt})
	}
	if a.endpoint.Observation != codeadapter.ObserveCodex {
		return
	}
	if allowance := codexwire.Allowance(headers, a.server.now()); allowance != nil {
		a.observeAllowance(ctx, *allowance)
	}
}

// A durable permit reads current issuer state. It may retain the verified claim
// digest only while the same declared issuer revision still owns the principal.
func bindCodeWorkload(fresh *access.Authority, verified access.Authority) error {
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
