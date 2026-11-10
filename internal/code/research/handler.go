package research

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	Timeout              = 2 * time.Minute
	responseWriteTimeout = 30 * time.Second
)

type Ledger interface {
	AuthorizeResearch(ctx context.Context, route code.Route, key, service string) (keyauthority.Authority, code.Account, error)
	ResearchSession(ctx context.Context, id string, binding code.ResearchSessionBinding) (string, error)
	CreateResearchSession(ctx context.Context, binding code.ResearchSessionBinding, upstream string) (string, error)
	DeleteResearchSession(ctx context.Context, id, key string) error
}

type Authorizer interface {
	AuthorizeResearch(ctx context.Context, cfg routing.Configuration, account code.Account, service string) (code.Authorization, error)
}

type Host interface {
	AdmitInFlight(ctx context.Context) bool
	ReleaseInFlight(ctx context.Context)
	OriginAllowed(r *http.Request) bool
	Authenticate(r *http.Request) (keyauthority.Authority, int, bool)
	Release() *release.Release
	Now() time.Time
	BodyLimit() int64
	CheckResearchCaller(r *http.Request, fresh *keyauthority.Authority, verified keyauthority.Authority) error
	ReserveRate(ctx context.Context, authority keyauthority.Authority, estimate int64, ttl time.Duration, slug string) (*admission.Lease, int, bool)
	ReserveProvider(ctx context.Context, providerID string, cfg routing.Configuration, estimate int64, ttl time.Duration) (*admission.Lease, int, bool)
	ReserveStream(ctx context.Context, authority keyauthority.Authority, slug, service, providerID string, limits *routing.Limits) (*admission.Lease, int, bool)
	SettleLease(ctx context.Context, lease *admission.Lease, dispatched bool, tokens *int64)
	ValidateEndpoint(target string) error
	ConnectionClient(ctx context.Context, rel *release.Release, cfg routing.Configuration, providerID string) (*http.Client, error)
	WriteError(w http.ResponseWriter, r *http.Request, err error)
	Log() *slog.Logger
}

type Handler struct {
	Host       Host
	Ledger     Ledger
	Authorizer Authorizer
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	refuse := func(err error) { h.Host.WriteError(w, r, err) }
	if !h.Host.AdmitInFlight(r.Context()) {
		refuse(code.Refuse(503, "code_overloaded"))
		return
	}
	defer h.Host.ReleaseInFlight(r.Context())
	if !h.Host.OriginAllowed(r) || r.URL.RawPath != "" || r.URL.RawQuery != "" {
		refuse(code.Refuse(403, "code_research_origin_denied"))
		return
	}
	if r.Method != http.MethodPost && r.Method != http.MethodGet && r.Method != http.MethodDelete {
		w.Header().Set("Allow", "POST, GET, DELETE")
		refuse(code.Refuse(405, "code_research_method_denied"))
		return
	}
	authority, status, ok := h.Host.Authenticate(r)
	if !ok {
		refuse(code.Refuse(status, "code_authentication_refused"))
		return
	}
	rel := h.Host.Release()
	if rel == nil || rel.Snapshot() == nil {
		refuse(code.Refuse(503, "code_research_unavailable"))
		return
	}
	route, found := rel.Snapshot().FindCodeRoute(r.PathValue("slug"))
	service := r.PathValue("service")
	if !found || !route.Enabled || !route.Tools.Enables(service) || !authority.Allows("inference", route.Slug, &route.ProjectID, h.Host.Now()) {
		refuse(code.Refuse(404, "code_research_disabled"))
		return
	}
	if h.Ledger == nil || h.Authorizer == nil {
		refuse(code.Refuse(503, "code_research_unavailable"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), Timeout)
	defer cancel()
	r = r.WithContext(ctx)
	session := r.Header.Get("Mcp-Session-Id")
	for _, name := range []string{"Mcp-Session-Id", "Mcp-Protocol-Version"} {
		if values := r.Header.Values(name); len(values) > 1 || len(values) == 1 && !ValidSession(values[0]) {
			refuse(code.Refuse(400, "code_research_header_invalid"))
			return
		}
	}
	var body []byte
	var err error
	method := ""
	if r.Method == http.MethodPost {
		media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if media != "application/json" || r.Header.Get("Content-Encoding") != "" && r.Header.Get("Content-Encoding") != "identity" {
			refuse(code.Refuse(415, "code_research_content_type_invalid"))
			return
		}
		limit := bodylimit.Lower(min(h.Host.BodyLimit(), MaxRequest), route.MaxBodyBytes)
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
		body, err = io.ReadAll(io.LimitReader(r.Body, limit+1))
		_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
		if err != nil || int64(len(body)) > limit {
			refuse(code.Refuse(413, "code_research_body_invalid"))
			return
		}
		body, method, err = Request(body, service)
		if err != nil {
			if errors.Is(err, ErrProtocol) {
				err = code.Refuse(400, "code_research_message_invalid")
			}
			refuse(err)
			return
		}
		if method == "initialize" && session != "" {
			refuse(code.Refuse(400, "code_research_session_invalid"))
			return
		}
	}
	attempt, err := h.prepare(r, rel, route, service, authority, strings.HasPrefix(method, "notifications/"))
	if err != nil {
		refuse(err)
		return
	}
	defer attempt.finish(ctx)
	if method != "initialize" && session == "" {
		refuse(code.Refuse(400, "code_research_session_required"))
		return
	}
	lastEvent := ""
	if values := r.Header.Values("Last-Event-Id"); len(values) != 0 {
		if len(values) != 1 || r.Method != http.MethodGet || session == "" {
			refuse(code.Refuse(400, "code_research_resume_invalid"))
			return
		}
		lastEvent, err = OpenEventID(session, values[0], attempt.auth.Headers.Get("Authorization"))
		if err != nil {
			refuse(code.Refuse(400, "code_research_resume_invalid"))
			return
		}
	}
	upstreamSession := ""
	if session != "" {
		upstreamSession, err = h.Ledger.ResearchSession(ctx, session, attempt.binding)
		if err != nil {
			refuse(err)
			return
		}
	}
	if r.Method == http.MethodDelete && upstreamSession == "" {
		if err = h.Ledger.DeleteResearchSession(ctx, session, attempt.binding.APIKeyID); err != nil {
			refuse(err)
			return
		}
		attempt.dispatched = true
		w.WriteHeader(http.StatusNoContent)
		return
	}
	target := code.ResearchEndpoint(service)
	if err = h.Host.ValidateEndpoint(target); err != nil {
		refuse(code.Refuse(502, "code_egress_refused"))
		return
	}
	client, err := h.Host.ConnectionClient(ctx, rel, attempt.config, attempt.account.ProviderID)
	if err != nil {
		refuse(err)
		return
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, r.Method, target, &oneShotBody{data: body})
	if err != nil {
		refuse(code.Refuse(502, "code_connection_invalid"))
		return
	}
	request.GetBody, request.Close, request.ContentLength = nil, true, int64(len(body))
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Accept-Encoding", "identity")
	if r.Method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	if value := r.Header.Get("Mcp-Protocol-Version"); value != "" {
		request.Header.Set("Mcp-Protocol-Version", value)
	}
	if lastEvent != "" {
		request.Header.Set("Last-Event-Id", lastEvent)
	}
	if upstreamSession != "" {
		request.Header.Set("Mcp-Session-Id", upstreamSession)
	}
	for name, values := range attempt.auth.Headers {
		request.Header[name] = values
	}
	attempt.dispatched = true
	response, err := client.Do(request)
	if err != nil {
		refuse(code.Refuse(502, "code_research_upstream_unavailable"))
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound || r.Method == http.MethodDelete && response.StatusCode >= 200 && response.StatusCode < 300 {
		if session != "" {
			if err = h.Ledger.DeleteResearchSession(ctx, session, attempt.binding.APIKeyID); err != nil {
				refuse(code.Refuse(503, "code_research_session_unavailable"))
				return
			}
		}
	}
	upstreamID := response.Header.Get("Mcp-Session-Id")
	if upstreamID != "" {
		if !ValidSession(upstreamID) || len(response.Header.Values("Mcp-Session-Id")) != 1 || (method != "initialize" && upstreamID != upstreamSession) {
			refuse(code.Refuse(502, "code_research_session_invalid"))
			return
		}
	}
	if method == "initialize" && response.StatusCode == http.StatusOK {
		session, err = h.Ledger.CreateResearchSession(ctx, attempt.binding, upstreamID)
		if err != nil {
			refuse(err)
			return
		}
		w.Header().Set("Mcp-Session-Id", session)
	} else if upstreamID != "" && session != "" {
		w.Header().Set("Mcp-Session-Id", session)
	}
	initialized := false
	if method == "initialize" && session != "" {
		defer func() {
			if !initialized {
				cleanup, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
				defer cleanupCancel()
				if err := h.Ledger.DeleteResearchSession(cleanup, session, attempt.binding.APIKeyID); err != nil && h.Host.Log() != nil {
					h.Host.Log().Warn("research session cleanup unavailable", "session_id", session)
				}
			}
		}()
	}
	forwarding := streamForwarding{
		method: method, service: service, resumeKey: attempt.auth.Headers.Get("Authorization"), requestID: MessageID(body),
		answerPing: func(reply []byte) error {
			return h.answerPing(r, rel, route, service, authority, attempt, session, reply)
		},
	}
	initialized = h.copyResponse(w, r, response, forwarding)
}

type researchAttempt struct {
	host                 Host
	account              code.Account
	config               routing.Configuration
	auth                 code.Authorization
	binding              code.ResearchSessionBinding
	lease, providerLease *admission.Lease
	streamLease          *admission.Lease
	dispatched           bool
}

func (a *researchAttempt) finish(ctx context.Context) {
	zero := int64(0)
	a.host.SettleLease(ctx, a.lease, a.dispatched, &zero)
	a.host.SettleLease(ctx, a.providerLease, a.dispatched, &zero)
	a.host.SettleLease(ctx, a.streamLease, a.dispatched, &zero)
}

func (h *Handler) prepare(r *http.Request, rel *release.Release, route code.Route, service string, authority keyauthority.Authority, notification bool) (*researchAttempt, error) {
	ctx := r.Context()
	fresh, account, err := h.authority(r, route, service, authority)
	if err != nil {
		return nil, err
	}
	a := &researchAttempt{host: h.Host, account: account}
	prepared := false
	defer func() {
		if !prepared {
			a.finish(ctx)
		}
	}()
	streamAuthority := fresh
	control := notification || r.Method == http.MethodGet
	rateAuth := RateAuthority(fresh, control)
	lease, failStatus, ok := h.Host.ReserveRate(ctx, rateAuth, 0, Timeout+time.Minute, route.Slug)
	if !ok {
		return nil, code.Refuse(failStatus, "code_rate_limited")
	}
	a.lease = lease
	var found bool
	a.config, found = rel.Snapshot().CodeConnection(route, account.ProviderID)
	if !found || a.config.ProfileID != codeplans.ZAIProfile {
		return nil, code.Refuse(503, "code_configuration_unsupported")
	}
	quota := a.config
	if quota.Options.Limits != nil {
		copied := *quota.Options.Limits
		copied.TokensPerMinute = nil
		if control {
			copied.MaxConcurrency = nil
		}
		quota.Options.Limits = &copied
	}
	providerLease, failStatus, ok := h.Host.ReserveProvider(ctx, account.ProviderID, quota, 0, Timeout+time.Minute)
	if !ok {
		return nil, code.Refuse(failStatus, "code_provider_rate_limited")
	}
	a.providerLease = providerLease
	if r.Method == http.MethodGet {
		streamLease, failStatus, ok := h.Host.ReserveStream(ctx, streamAuthority, route.Slug, service, account.ProviderID, a.config.Options.Limits)
		if !ok {
			return nil, code.Refuse(failStatus, "code_research_stream_limited")
		}
		a.streamLease = streamLease
	}
	a.auth, err = h.authorization(ctx, a.config, account, service)
	if err != nil {
		return nil, err
	}
	a.binding = code.ResearchSessionBinding{
		APIKeyID: fresh.ID, RevisionID: route.RevisionID, Service: service,
		AccountID: account.ID, CredentialID: a.auth.CredentialID, Generation: a.auth.GrantGeneration,
	}
	prepared = true
	return a, nil
}

func (h *Handler) authority(r *http.Request, route code.Route, service string, authority keyauthority.Authority) (keyauthority.Authority, code.Account, error) {
	fresh, account, err := h.Ledger.AuthorizeResearch(r.Context(), route, authority.ID, service)
	if err != nil {
		return fresh, account, err
	}
	if fresh.ID != authority.ID || account.ID != route.Tools.AccountID || account.ProjectID != route.ProjectID {
		return fresh, account, code.Refuse(403, "code_research_account_unavailable")
	}
	if err := h.Host.CheckResearchCaller(r, &fresh, authority); err != nil {
		return fresh, account, err
	}
	return fresh, account, nil
}

func (h *Handler) authorization(ctx context.Context, config routing.Configuration, account code.Account, service string) (code.Authorization, error) {
	auth, err := h.Authorizer.AuthorizeResearch(ctx, config, account, service)
	if err != nil {
		return auth, err
	}
	zai, _ := vendor.Lookup(code.AdapterZAICoding)
	endpoint, _ := zai.Endpoint("v1/chat/completions")
	if err = zai.ValidateAuthorization(endpoint, auth, account.Principal); err != nil || auth.CredentialID != account.CredentialID || auth.GrantGeneration < 1 {
		return auth, code.Refuse(503, "code_authorization_invalid")
	}
	return auth, nil
}

func (h *Handler) answerPing(r *http.Request, rel *release.Release, route code.Route, service string, authority keyauthority.Authority, attempt *researchAttempt, session string, reply []byte) error {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	fresh, account, err := h.authority(r.WithContext(ctx), route, service, authority)
	if err != nil {
		return err
	}
	auth, err := h.authorization(ctx, attempt.config, account, service)
	if err != nil {
		return err
	}
	binding := code.ResearchSessionBinding{
		APIKeyID: fresh.ID, RevisionID: route.RevisionID, Service: service,
		AccountID: account.ID, CredentialID: auth.CredentialID, Generation: auth.GrantGeneration,
	}
	if binding != attempt.binding {
		return code.Refuse(409, "code_research_session_invalid")
	}
	upstream, err := h.Ledger.ResearchSession(ctx, session, attempt.binding)
	if err != nil {
		return err
	}
	client, err := h.Host.ConnectionClient(ctx, rel, attempt.config, account.ProviderID)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, code.ResearchEndpoint(service), &oneShotBody{data: reply})
	if err != nil {
		return err
	}
	request.ContentLength, request.Close, request.GetBody = int64(len(reply)), true, nil
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Accept-Encoding", "identity")
	if upstream != "" {
		request.Header.Set("Mcp-Session-Id", upstream)
	}
	if protocol := r.Header.Get("Mcp-Protocol-Version"); protocol != "" {
		request.Header.Set("Mcp-Protocol-Version", protocol)
	}
	for name, values := range auth.Headers {
		request.Header[name] = values
	}
	response, err := client.Do(request)
	if err != nil {
		return code.Refuse(502, "code_research_upstream_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return code.Refuse(502, "code_research_ping_refused")
	}
	return nil
}

type oneShotBody struct{ data []byte }

func (b *oneShotBody) Read(p []byte) (int, error) {
	if len(b.data) == 0 {
		b.data = nil
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, nil
}

func (b *oneShotBody) Close() error { return nil }

func RateAuthority(authority keyauthority.Authority, control bool) keyauthority.Authority {
	authority = WithoutCostBudgets(authority)
	authority.Policy.TokensPerMinute = nil
	authority.BudgetGroupTPM = nil
	if control {
		authority.Policy.MaxConcurrency = nil
		authority.BudgetGroupConcurrency = nil
	}
	for slug, policy := range authority.Policy.RouteLimits {
		policy.TokensPerMinute = nil
		if control {
			policy.MaxConcurrency = nil
		}
		authority.Policy.RouteLimits[slug] = policy
	}
	for _, policy := range []*keyauthority.EndUserPolicy{authority.Policy.EndUserPolicy, authority.ProjectEndUserPolicy} {
		if policy != nil {
			policy.Defaults.TokensPerMinute = nil
			if control {
				policy.Defaults.MaxConcurrency = nil
			}
			for digest, limits := range policy.Overrides {
				limits.TokensPerMinute = nil
				if control {
					limits.MaxConcurrency = nil
				}
				policy.Overrides[digest] = limits
			}
		}
	}
	return authority
}
