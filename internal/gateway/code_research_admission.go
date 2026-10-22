package gateway

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

type researchAttempt struct {
	server               *Server
	account              codemode.Account
	config               routing.Configuration
	auth                 codemode.Authorization
	binding              codemode.ResearchSessionBinding
	lease, providerLease *limits.Lease
	dispatched           bool
}

func (s *Server) prepareResearch(r *http.Request, release *runtime.Release, route codemode.Route, service string, authority access.Authority, cancellation bool) (*researchAttempt, error) {
	ctx := r.Context()
	fresh, account, err := s.researchAuthority(r, route, service, authority)
	if err != nil {
		return nil, err
	}
	a := &researchAttempt{server: s, account: account}
	prepared := false
	defer func() {
		if !prepared {
			a.finish(ctx)
		}
	}()
	var failure *Error
	if cancellation {
		fresh = researchCancellationAuthority(fresh)
	}
	a.lease, failure = s.Admission.ReserveCodeRate(ctx, fresh, 0, researchTimeout+time.Minute, route.Slug)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_rate_limited")
	}
	var found bool
	a.config, found = release.Snapshot.CodeConnection(route, account.ProviderID)
	if !found || a.config.ProfileID != codeplans.ZAIProfile {
		return nil, codemode.Refuse(503, "code_configuration_unsupported")
	}
	quota := a.config
	if cancellation && quota.Options.Limits != nil {
		copy := *quota.Options.Limits
		copy.MaxConcurrency = nil
		quota.Options.Limits = &copy
	}
	a.providerLease, failure = s.Admission.reserveCodeProvider(ctx, account.ProviderID, quota, 0, researchTimeout+time.Minute)
	if failure != nil {
		return nil, codemode.Refuse(failure.Status, "code_provider_rate_limited")
	}
	a.auth, err = s.researchAuthorization(ctx, a.config, account, service)
	if err != nil {
		return nil, err
	}
	a.binding = researchBinding(fresh.ID, route, service, account, a.auth)
	prepared = true
	return a, nil
}

// Cancellation cannot start work. Keep its rate and identity policies, but do
// not make it wait for the concurrency slots held by the work it must stop.
func researchCancellationAuthority(authority access.Authority) access.Authority {
	// This also clones the nested route and end-user policies before editing.
	authority = withoutCostBudgets(authority)
	authority.Policy.MaxConcurrency = nil
	authority.BudgetGroupConcurrency = nil
	for slug, policy := range authority.Policy.RouteLimits {
		policy.MaxConcurrency = nil
		authority.Policy.RouteLimits[slug] = policy
	}
	for _, policy := range []*access.EndUserPolicy{authority.Policy.EndUserPolicy, authority.ProjectEndUserPolicy} {
		if policy != nil {
			policy.Defaults.MaxConcurrency = nil
			for digest, limits := range policy.Overrides {
				limits.MaxConcurrency = nil
				policy.Overrides[digest] = limits
			}
		}
	}
	return authority
}

func (s *Server) researchAuthority(r *http.Request, route codemode.Route, service string, authority access.Authority) (access.Authority, codemode.Account, error) {
	ctx := r.Context()
	fresh, account, err := s.CodeResearch.AuthorizeResearch(ctx, route, authority.ID, service)
	if err != nil {
		return fresh, account, err
	}
	if fresh.ID != authority.ID || account.ID != route.Tools.AccountID || account.ProjectID != route.ProjectID {
		return fresh, account, codemode.Refuse(403, "code_research_account_unavailable")
	}
	if err = bindCodeWorkload(&fresh, authority); err != nil {
		return fresh, account, err
	}
	failure := s.checkKeyAddress(r, fresh)
	if failure == nil {
		fresh, failure = s.identifyEndUser(r, fresh)
	}
	if failure != nil {
		return fresh, account, codemode.Refuse(failure.Status, "code_permission_denied")
	}
	if _, failure = s.parseAttribution(r, fresh); failure != nil {
		return fresh, account, codemode.Refuse(failure.Status, "code_invalid_attribution")
	}
	return fresh, account, nil
}

func (s *Server) researchAuthorization(ctx context.Context, config routing.Configuration, account codemode.Account, service string) (codemode.Authorization, error) {
	auth, err := s.ResearchAuth.AuthorizeResearch(ctx, config, account, service)
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
	settleKey(ctx, a.lease, a.dispatched, &zero, a.server.log)
	settleKey(ctx, a.providerLease, a.dispatched, &zero, a.server.log)
}
