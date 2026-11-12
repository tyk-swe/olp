package codeforward

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type researchAttempt struct {
	log                  *slog.Logger
	account              codemode.Account
	config               routing.Configuration
	auth                 codemode.Authorization
	binding              codemode.ResearchSessionBinding
	lease, providerLease *admission.Lease
	streamLease          *admission.Lease
	dispatched           bool
}

func (f *Forwarder) prepareResearch(r *http.Request, release *release.Release, route codemode.Route, service string, authority keyauthority.Authority, notification bool) (*researchAttempt, error) {
	ctx := r.Context()
	fresh, account, err := f.researchAuthority(r, route, service, authority)
	if err != nil {
		return nil, err
	}
	a := &researchAttempt{log: f.log, account: account}
	prepared := false
	defer func() {
		if !prepared {
			a.finish(ctx)
		}
	}()
	var failure *fault.Error
	streamAuthority := fresh
	control := notification || r.Method == http.MethodGet
	fresh = quota.RequestRateAuthority(fresh, control)
	a.lease, failure = f.admission.ReserveCodeRate(ctx, fresh, 0, researchTimeout+time.Minute, route.Slug)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_rate_limited")
	}
	var found bool
	a.config, found = release.Snapshot().CodeConnection(route, account.ProviderID)
	if !found || a.config.ProfileID != codeplans.ZAIProfile {
		return nil, codemode.Refuse(503, "code_configuration_unsupported")
	}
	limited := a.config
	if limited.Options.Limits != nil {
		copy := *limited.Options.Limits
		copy.TokensPerMinute = nil
		if control {
			copy.MaxConcurrency = nil
		}
		limited.Options.Limits = &copy
	}
	a.providerLease, failure = f.admission.ReserveCodeProvider(ctx, account.ProviderID, limited, 0, researchTimeout+time.Minute)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_provider_rate_limited")
	}
	if r.Method == http.MethodGet {
		a.streamLease, failure = f.admission.ReserveResearchStream(ctx, streamAuthority, route.Slug, service, account.ProviderID, a.config.Options.Limits, researchTimeout)
		if failure != nil {
			return nil, codemode.Refuse(failure.Status, "code_research_stream_limited")
		}
	}
	a.auth, err = f.researchAuthorization(ctx, a.config, account, service)
	if err != nil {
		return nil, err
	}
	a.binding = researchBinding(fresh.ID, route, service, account, a.auth)
	prepared = true
	return a, nil
}

func (f *Forwarder) researchAuthority(r *http.Request, route codemode.Route, service string, authority keyauthority.Authority) (keyauthority.Authority, codemode.Account, error) {
	ctx := r.Context()
	fresh, account, err := f.research.AuthorizeResearch(ctx, route, authority.ID, service)
	if err != nil {
		return fresh, account, err
	}
	if fresh.ID != authority.ID || account.ID != route.Tools.AccountID || account.ProjectID != route.ProjectID {
		return fresh, account, codemode.Refuse(403, "code_research_account_unavailable")
	}
	if err = bindCodeWorkload(&fresh, authority); err != nil {
		return fresh, account, err
	}
	failure := f.gate.CheckKeyAddress(r, fresh)
	if failure == nil {
		fresh, failure = f.gate.IdentifyEndUser(r, fresh)
	}
	if failure != nil {
		return fresh, account, codemode.Refuse(failure.Status, "code_permission_denied")
	}
	if !fresh.AllowsEndUser(fresh.EndUserDigest) {
		return fresh, account, codemode.Refuse(403, "code_permission_denied")
	}
	if _, failure = ingress.ParseAttribution(r, fresh); failure != nil {
		return fresh, account, codemode.Refuse(failure.Status, "code_invalid_attribution")
	}
	return fresh, account, nil
}

func (f *Forwarder) researchAuthorization(ctx context.Context, config routing.Configuration, account codemode.Account, service string) (codemode.Authorization, error) {
	auth, err := f.researchAuth.AuthorizeResearch(ctx, config, account, service)
	if err != nil {
		return auth, err
	}
	vendor, _ := codeadapter.Lookup(codemode.AdapterZAICoding)
	endpoint, _ := vendor.Endpoint("v1/chat/completions")
	if err = vendor.ValidateAuthorization(endpoint, auth, account.Principal); err != nil || auth.CredentialID != account.CredentialID || auth.GrantGeneration < 1 {
		return auth, codemode.Refuse(503, "code_authorization_invalid")
	}
	return auth, nil
}

func researchBinding(key string, route codemode.Route, service string, account codemode.Account, auth codemode.Authorization) codemode.ResearchSessionBinding {
	return codemode.ResearchSessionBinding{APIKeyID: key, RevisionID: route.RevisionID, Service: service, AccountID: account.ID, CredentialID: auth.CredentialID, Generation: auth.GrantGeneration}
}

func (a *researchAttempt) finish(ctx context.Context) {
	// Research consumes requests and concurrency, without inference token usage.
	zero := int64(0)
	quota.SettleKey(ctx, a.lease, a.dispatched, &zero, a.log)
	quota.SettleKey(ctx, a.providerLease, a.dispatched, &zero, a.log)
	quota.SettleKey(ctx, a.streamLease, a.dispatched, &zero, a.log)
}
