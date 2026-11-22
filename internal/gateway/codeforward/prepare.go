package codeforward

import (
	"errors"
	"net/http"
	"reflect"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/release"
)

func (f *Forwarder) resolveCodeIngress(r *http.Request, release *release.Release, route codemode.Route, observation codemode.Request, inbound codeIngress, transport codeadapter.Transport, recovery *codesessions.Recovery, body []byte) (keyauthority.Authority, codemode.Operation, *codeAttempt, error) {
	authority, failure := f.gate.Authenticate(r, "inference")
	if failure != nil {
		return keyauthority.Authority{}, codemode.Operation{}, nil, codemode.Refuse(failure.Status, "code_authentication_refused")
	}
	if !authority.Allows("inference", route.Slug, &route.ProjectID, f.now()) {
		return keyauthority.Authority{}, codemode.Operation{}, nil, codemode.Refuse(403, "code_permission_denied")
	}
	resolved, err := route.ResolveModel(observation.Operation)
	if err != nil {
		return keyauthority.Authority{}, codemode.Operation{}, nil, err
	}
	operation := resolved
	if recovery == nil && len(route.PaidTargets) > 0 && resolved.PublicModel() != resolved.Model {
		operation, err = f.ledger.ResolvePaidModel(r.Context(), codesessions.Admission{Route: route, APIKeyID: authority.ID, Operation: resolved, EndUserDigest: authority.EndUserDigest, PreviousResponse: observation.PreviousResponse}, authority)
		if err != nil {
			return keyauthority.Authority{}, codemode.Operation{}, nil, err
		}
	}
	if _, paid := route.PaidTargets[operation.Model]; paid && recovery == nil {
		attempt, err := f.preparePaidCode(r, release, route, observation, operation, inbound, transport, authority, body, nil, false)
		return authority, operation, attempt, err
	}
	if recovery == nil && len(route.PaidFallbackTargets) > 0 {
		selected, err := f.ledger.ResolvePaidFallback(r.Context(), codesessions.Admission{Route: route, APIKeyID: authority.ID, Operation: operation, PreviousResponse: observation.PreviousResponse}, authority)
		if err != nil {
			return keyauthority.Authority{}, codemode.Operation{}, nil, err
		}
		if selected != nil {
			attempt, err := f.preparePaidCode(r, release, route, observation, *selected, inbound, transport, authority, body, nil, true)
			return authority, operation, attempt, err
		}
	}
	return authority, operation, nil, nil
}

func (f *Forwarder) admitCodePermit(r *http.Request, release *release.Release, route codemode.Route, observation codemode.Request, operation codemode.Operation, inbound codeIngress, transport codeadapter.Transport, authority keyauthority.Authority, labels map[string]string, providers []string, lease *admission.Lease, recovery *codesessions.Recovery, body []byte) (codesessions.Permit, *codeAttempt, error) {
	permit, err := f.ledger.Admit(r.Context(), codesessions.Admission{
		Attribution: labels, Route: route, APIKeyID: authority.ID, EndUserDigest: authority.EndUserDigest,
		InitialRecovery: observation.Recoverable, Recovery: recovery, Operation: operation, Providers: providers, ModelSource: release.Snapshot(), ModelPath: inbound.path, ModelTransport: transport, PreviousResponse: observation.PreviousResponse,
	})
	if err == nil {
		return permit, nil, nil
	}
	quota.SettleKey(r.Context(), lease, false, nil, f.log)
	if refusal, ok := errors.AsType[*codemode.Refusal](err); ok && refusal.Code == "code_account_unavailable" && (recovery == nil || recovery.AccountID == "") && transport == codeadapter.HTTP && observation.Recoverable && len(route.PaidFallbackTargets) > 0 {
		selected, selectionErr := f.ledger.ResolvePaidFallback(r.Context(), codesessions.Admission{Route: route, APIKeyID: authority.ID, Operation: operation, InitialRecovery: observation.Recoverable, Recovery: recovery, PreviousResponse: observation.PreviousResponse}, authority)
		if selectionErr != nil {
			return codesessions.Permit{}, nil, selectionErr
		}
		if selected != nil {
			attempt, paidErr := f.preparePaidCode(r, release, route, observation, *selected, inbound, transport, authority, body, recovery, true)
			return codesessions.Permit{}, attempt, paidErr
		}
	}
	return codesessions.Permit{}, nil, err
}

func (f *Forwarder) reconcileCodePermit(r *http.Request, a *codeAttempt, authority keyauthority.Authority, labels map[string]string, observation codemode.Request, slug string) error {
	if err := bindCodeWorkload(&a.permit.Authority, authority); err != nil {
		return err
	}
	if !a.permit.Authority.AllowsAttribution(labels) {
		return codemode.Refuse(409, "code_attribution_changed")
	}
	if !reflect.DeepEqual(authority.Policy, a.permit.Authority.Policy) || !reflect.DeepEqual(authority.ProjectEndUserPolicy, a.permit.Authority.ProjectEndUserPolicy) || !reflect.DeepEqual(authority.BudgetGroupID, a.permit.Authority.BudgetGroupID) || !reflect.DeepEqual(authority.GroupLimits(), a.permit.Authority.GroupLimits()) {
		quota.SettleKey(r.Context(), a.lease, false, nil, f.log)
		a.lease = nil
		fresh := a.permit.Authority
		if failure := f.gate.CheckKeyAddress(r, fresh); failure != nil {
			return codemode.Refuse(failure.Status, "code_ip_not_allowed")
		}
		fresh, failure := f.gate.IdentifyEndUser(r, fresh)
		if failure != nil {
			return codemode.Refuse(failure.Status, "code_end_user_invalid")
		}
		if fresh.EndUserDigest != authority.EndUserDigest {
			return codemode.Refuse(409, "code_end_user_changed")
		}
		a.lease, failure = f.admission.ReserveCodeRate(r.Context(), fresh, observation.Estimate, codeGenerationTimeout+time.Minute, slug)
		if failure != nil {
			return codemode.Refuse(failure.Status, "code_rate_limited")
		}
	}
	if a.permit.Pin.AccountID != a.permit.Account.ID || a.permit.Pin.Principal != a.permit.Account.Principal || a.permit.Authority.ID != authority.ID {
		return codemode.Refuse(503, "code_binding_invalid")
	}
	return nil
}

func (f *Forwarder) bindCodeTarget(r *http.Request, a *codeAttempt, release *release.Release, route codemode.Route, observation codemode.Request, inbound codeIngress, transport codeadapter.Transport, recovery *codesessions.Recovery, model string) error {
	target, err := resolveCodeTarget(release.Snapshot(), route, a.permit.Account.ProviderID, inbound, transport)
	if err != nil {
		return err
	}
	if !target.adapter.ServesModel(target.endpoint.Protocol, model) {
		return codemode.Refuse(503, "code_configuration_unsupported")
	}
	a.config, a.endpoint, a.observer = target.config, target.endpoint, codeObserver(target.endpoint, observation.ChatChoices)
	a.adapter = target.adapter.Adapter
	a.identityOriginator = target.adapter.CompatibilityOriginator(route)
	var failure error
	providerLease, gateFailure := f.admission.ReserveCodeProvider(r.Context(), a.permit.Account.ProviderID, target.config, codeProviderDemand(observation), codeGenerationTimeout+time.Minute)
	if gateFailure != nil {
		return codemode.Refuse(gateFailure.Status, "code_provider_rate_limited")
	}
	a.providerLease = providerLease
	a.auth, failure = f.authorizer.AuthorizeCode(r.Context(), target.config, a.permit.Account, target.dispatchDescriptor())
	if failure != nil {
		return codemode.Refuse(503, "code_account_unavailable")
	}
	if err := target.adapter.ValidateAuthorization(target.endpoint, a.auth, a.permit.Pin.Principal); err != nil {
		return err
	}
	if recovery != nil && recovery.AccountID != "" && (a.auth.CredentialID != recovery.CredentialID || a.auth.GrantGeneration <= recovery.GrantGeneration) {
		return codemode.Refuse(409, "code_recovery_unsafe")
	}
	return nil
}
