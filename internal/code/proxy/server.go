package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/bodylimit"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/client"
	"github.com/tyk-swe/olp/internal/code/research"
	"github.com/tyk-swe/olp/internal/code/session"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/code/wire"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/oif"
	protocolwire "github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	GenerationTimeout    = 10 * time.Minute
	responseWriteTimeout = 30 * time.Second
	realtimeReauth       = 30 * time.Second
)

type Ledger interface {
	Models(context.Context, code.Route, string) (client.Offers, error)
	Admit(context.Context, session.Admission) (session.Permit, error)
	BindConnection(context.Context, code.Route, string, code.Identity, string, []string) (session.Permit, error)
	MarkDispatched(context.Context, string) error
	ConfirmNonexecution(context.Context, string, int) error
	ObserveCooldown(context.Context, string, time.Time) error
	ObserveCooldownClass(context.Context, string, string, vendor.Classification, time.Time) error
	Settle(context.Context, string, code.Usage) error
	Abort(context.Context, string) error
	ObserveHealth(context.Context, string, code.HealthObservation) error
	ObserveAllowance(context.Context, string, code.Allowance) error
	ObserveAllowanceCharge(context.Context, string, json.RawMessage) error
	ObserveReference(context.Context, string, string) error
	ObserveOutcome(context.Context, string, code.Outcome) error
	RecordRefusal(context.Context, code.Route, string, string, string) error
	ResolvePaidFallback(context.Context, session.Admission, keyauthority.Authority) (*code.Operation, error)
	ResolvePaidModel(context.Context, session.Admission, keyauthority.Authority) (code.Operation, error)
	AdmitPaid(context.Context, session.PaidAdmission) (session.PaidPermit, error)
}

type Authorizer interface {
	AuthorizeCode(context.Context, routing.Configuration, code.Account, code.Dispatch) (code.Authorization, error)
}

type PaidTargetLease interface {
	Settle(ctx context.Context, dispatched bool, tokens *int64)
}

type PaidTerminal struct {
	AttemptID           string
	Authority           keyauthority.Authority
	Route               code.Route
	RuntimeGenerationID string
	ReleaseSequence     int64
	Provider            routing.Provider
	Slot                routing.Slot
	Model               string
	Family              protocolwire.Format
	Stream              bool
	Status              int
	StartedAt           time.Time
	CompletedAt         time.Time
	Usage               *protocolwire.Usage
	UsageObserved       bool
	UsageComplete       bool
	BillingUncertain    bool
	Price               *usage.RoutingPrice
	Discount            *usage.RoutingDiscount
	Card                *usage.RoutingCardRevision
}

type StreamWriter interface {
	Emit(chunk []byte) error
	Fail(status int, errCode string, cancelled bool)
}

type Host interface {
	AdmitInFlight(ctx context.Context) bool
	ReleaseInFlight(ctx context.Context)
	Authenticate(r *http.Request) (keyauthority.Authority, int, bool)
	AuthenticatePresentedKey(r *http.Request) (keyauthority.Authority, int, bool)
	ParseAttribution(r *http.Request, authority keyauthority.Authority) (map[string]string, int, string, bool)
	CheckKeyAddress(r *http.Request, authority keyauthority.Authority) (int, bool)
	IdentifyEndUser(r *http.Request, authority keyauthority.Authority) (keyauthority.Authority, int, bool)
	Release() *release.Release
	RoutingInputs() *usage.RoutingInputs
	IsCredentialEligible(credentialID string) bool
	Secret(ctx context.Context, rel *release.Release, credentialID string) ([]byte, int64, error)
	CredentialRefused(credentialID string, generation int64)
	ApplyConnectorAuth(ctx context.Context, r *http.Request, connector connectors.Config, secret, body []byte) (egress.Sensitive, error)
	RedactFailureDocument(doc oif.Document, sensitive egress.Sensitive) (oif.Document, error)
	ReserveRate(ctx context.Context, authority keyauthority.Authority, estimate int64, ttl time.Duration, slug string) (*admission.Lease, int, bool)
	ReserveProvider(ctx context.Context, providerID string, cfg routing.Configuration, estimate int64, ttl time.Duration) (*admission.Lease, int, bool)
	ReservePaid(ctx context.Context, authority *keyauthority.Authority, route code.Route, provider routing.Provider, slot routing.Slot, tokens int64, ttl time.Duration, requestID, cost, charge string) (*admission.Lease, PaidTargetLease, []*admission.Lease, error)
	SettleLease(ctx context.Context, lease *admission.Lease, dispatched bool, tokens *int64)
	RecordPaidTerminal(terminal PaidTerminal)
	ValidateEndpoint(target string) error
	ConnectionClient(ctx context.Context, rel *release.Release, cfg routing.Configuration, providerID string) (*http.Client, error)
	NewStreamWriter(w http.ResponseWriter, family protocolwire.Format) StreamWriter
	WriteRefusal(w http.ResponseWriter, r *http.Request, refusal *code.Refusal)
	WriteTranslatedRejection(w http.ResponseWriter, r *http.Request, refusal *code.Refusal)
	BodyLimit() int64
	Now() time.Time
	Log() *slog.Logger
}

type Server struct {
	Host       Host
	Ledger     Ledger
	Authorizer Authorizer
	Research   http.Handler
}

func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /code/{slug}/models", s.ServeModels)
	mux.HandleFunc("GET /code/{slug}/v1/models", s.ServeModels)
	if s.Research != nil {
		mux.Handle("/code/{slug}/mcp/{service}", s.Research)
	}
	mux.HandleFunc("/code/{slug}/{operation...}", s.ServeCode)
}

func (s *Server) ServeCode(w http.ResponseWriter, r *http.Request) {
	if !s.Host.AdmitInFlight(r.Context()) {
		s.writeError(w, r, code.Refuse(503, "code_overloaded"))
		return
	}
	defer s.Host.ReleaseInFlight(r.Context())
	authority, status, ok := s.Host.Authenticate(r)
	if !ok {
		s.writeError(w, r, code.Refuse(status, "code_authentication_refused"))
		return
	}
	rel := s.Host.Release()
	if rel == nil || rel.Snapshot() == nil {
		s.writeError(w, r, code.Refuse(503, "code_runtime_unavailable"))
		return
	}
	route, ok := rel.Snapshot().FindCodeRoute(r.PathValue("slug"))
	if !ok {
		s.writeError(w, r, code.Refuse(404, "code_route_unavailable"))
		return
	}
	refuse := func(err error) { s.refuse(w, r, route, authority.ID, authority.EndUserDigest, err) }
	if !route.Enabled || !authority.Allows("inference", route.Slug, &route.ProjectID, s.Host.Now()) {
		refuse(code.Refuse(404, "code_route_unavailable"))
		return
	}
	if _, failStatus, failCode, ok := s.Host.ParseAttribution(r, authority); !ok {
		refuse(code.Refuse(failStatus, "code_"+failCode))
		return
	}
	path := r.PathValue("operation")
	if r.URL.RawPath != "" {
		refuse(code.Refuse(400, "code_operation_unsupported"))
		return
	}
	if !rel.Snapshot().HasCodeAdapters(route) && len(route.PaidTargets)+len(route.PaidFallbackTargets) == 0 {
		refuse(code.Refuse(503, "code_adapter_unavailable"))
		return
	}
	ingress, ok := vendor.ForPath(path)
	if ok && r.Method == http.MethodGet && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && rel.Snapshot().HasCodeProviders(route, path, code.WebSocket) {
		s.serveWebSocket(w, r, rel, route, authority, ingress)
		return
	}
	if r.Method != http.MethodPost || !ok || !rel.Snapshot().HasCodeProviders(route, path, code.HTTP) && len(route.PaidTargets)+len(route.PaidFallbackTargets) == 0 {
		if isClientProbe(r.Method, path) && rel.Snapshot().HasCodeProviders(route, "v1/messages", code.HTTP) {
			s.writeError(w, r, code.Refuse(404, "code_operation_unsupported"))
			return
		}
		refuse(code.Refuse(400, "code_operation_unsupported"))
		return
	}
	limit := bodylimit.Lower(s.Host.BodyLimit(), route.MaxBodyBytes)
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(30 * time.Second))
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		refuse(code.Refuse(400, "code_body_invalid"))
		return
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	if len(r.Header.Values("Content-Encoding")) > 1 {
		refuse(code.Refuse(415, "code_encoding_unsupported"))
		return
	}
	body, err := wire.Decode(raw, r.Header.Get("Content-Encoding"), limit)
	if err != nil {
		refuse(err)
		return
	}
	observation, err := wire.ClassifyRequest(ingress, body, r.Header, false)
	if err != nil {
		refuse(err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), GenerationTimeout)
	defer cancel()
	observation.Recoverable = route.InitialRecoveryAttempts > 0 && wire.PortableInitialRequest(body, r.Header, observation)
	s.forwardHTTP(w, r.WithContext(ctx), rel, route, observation, ingress, raw, body, nil, refuse)
}

func (s *Server) forwardHTTP(w http.ResponseWriter, r *http.Request, rel *release.Release, route code.Route, observation code.Request, ingress vendor.Endpoint, raw, body []byte, recovery *code.Recovery, refuse func(error)) {
	sub, paid, err := s.prepareExecution(r, rel, route, observation, ingress, code.HTTP, recovery, body)
	if err != nil {
		refuse(err)
		return
	}
	if paid != nil {
		defer paid.finish(r.Context())
		paid.serveHTTP(r.Context(), w, r, rel, raw, body, refuse)
		return
	}
	defer sub.finish(r.Context())
	next, recoveryErr := sub.serveHTTP(r.Context(), w, r, rel, route, observation, ingress, raw, body, recovery, refuse)
	if recoveryErr != nil {
		refuse(recoveryErr)
		return
	}
	if next != nil {
		s.forwardHTTP(w, r, rel, route, observation, ingress, raw, body, next, refuse)
	}
}

func (s *Server) prepareExecution(r *http.Request, rel *release.Release, route code.Route, observation code.Request, ingress vendor.Endpoint, transport code.Transport, recovery *code.Recovery, body []byte) (*subscriptionAttempt, *paidAttempt, error) {
	authority, status, ok := s.Host.Authenticate(r)
	if !ok {
		return nil, nil, code.Refuse(status, "code_authentication_refused")
	}
	if !authority.Allows("inference", route.Slug, &route.ProjectID, s.Host.Now()) {
		return nil, nil, code.Refuse(403, "code_permission_denied")
	}
	resolved, err := route.ResolveModel(observation.Operation)
	if err != nil {
		return nil, nil, err
	}
	operation := resolved
	if recovery == nil && len(route.PaidTargets) > 0 && resolved.PublicModel() != resolved.Model {
		operation, err = s.Ledger.ResolvePaidModel(r.Context(), session.Admission{Route: route, APIKeyID: authority.ID, Operation: resolved, EndUserDigest: authority.EndUserDigest, PreviousResponse: observation.PreviousResponse}, authority)
		if err != nil {
			return nil, nil, err
		}
	}
	if _, isPaid := route.PaidTargets[operation.Model]; isPaid && recovery == nil {
		paid, err := s.preparePaid(r, rel, route, observation, operation, ingress, transport, authority, body, nil, false)
		return nil, paid, err
	}
	if recovery == nil && len(route.PaidFallbackTargets) > 0 {
		selected, err := s.Ledger.ResolvePaidFallback(r.Context(), session.Admission{Route: route, APIKeyID: authority.ID, Operation: operation, PreviousResponse: observation.PreviousResponse}, authority)
		if err != nil {
			return nil, nil, err
		}
		if selected != nil {
			paid, err := s.preparePaid(r, rel, route, observation, *selected, ingress, transport, authority, body, nil, true)
			return nil, paid, err
		}
	}
	providersFor := func(model string) []string {
		return rel.Snapshot().CodeModelProviders(route, ingress.ClientPath, model, transport)
	}
	if len(providersFor(operation.Model)) == 0 && len(route.PaidFallbackTargets) == 0 && (!route.ModelRewriting || len(rel.Snapshot().CodeProviders(route, ingress.ClientPath, transport)) == 0) {
		return nil, nil, code.Refuse(400, "code_operation_unsupported")
	}
	labels, failStatus, failCode, ok := s.Host.ParseAttribution(r, authority)
	if !ok {
		return nil, nil, code.Refuse(failStatus, "code_"+failCode)
	}
	lease, failStatus, ok := s.Host.ReserveRate(r.Context(), research.WithoutCostBudgets(authority), observation.Estimate, GenerationTimeout+time.Minute, route.Slug)
	if !ok {
		return nil, nil, code.Refuse(failStatus, "code_rate_limited")
	}
	permit, err := s.Ledger.Admit(r.Context(), session.Admission{
		Attribution: labels, Route: route, APIKeyID: authority.ID, EndUserDigest: authority.EndUserDigest,
		InitialRecovery: observation.Recoverable, Recovery: recovery, Operation: operation,
		ProvidersFor: providersFor, PreviousResponse: observation.PreviousResponse,
	})
	if err != nil {
		s.Host.SettleLease(r.Context(), lease, false, nil)
		_, isCredentialRefresh := func() (code.CredentialRefreshTarget, bool) {
			if recovery == nil {
				return code.CredentialRefreshTarget{}, false
			}
			return recovery.CredentialRefresh()
		}()
		if refusal, ok := errors.AsType[*code.Refusal](err); ok && refusal.Code == "code_account_unavailable" && !isCredentialRefresh && transport == code.HTTP && observation.Recoverable && len(route.PaidFallbackTargets) > 0 {
			selected, selectionErr := s.Ledger.ResolvePaidFallback(r.Context(), session.Admission{
				Route: route, APIKeyID: authority.ID, Operation: operation,
				InitialRecovery: observation.Recoverable, Recovery: recovery, PreviousResponse: observation.PreviousResponse,
			}, authority)
			if selectionErr != nil {
				return nil, nil, selectionErr
			}
			if selected != nil {
				paid, paidErr := s.preparePaid(r, rel, route, observation, *selected, ingress, transport, authority, body, recovery, true)
				return nil, paid, paidErr
			}
		}
		return nil, nil, err
	}
	sub, err := s.finishSubscriptionPrepare(r, rel, route, observation, operation, ingress, transport, authority, labels, permit, lease, recovery)
	return sub, nil, err
}

func isClientProbe(method, path string) bool {
	return method == http.MethodHead && path == "api/hello" || method == http.MethodPost && path == "v1/messages/count_tokens"
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

func buildUpstreamHeaders(source http.Header, authHeaders http.Header, originator string) http.Header {
	h := wire.ForwardHeaders(source, true)
	for name, values := range authHeaders {
		if strings.EqualFold(name, "Anthropic-Beta") {
			h[http.CanonicalHeaderKey(name)] = []string{strings.Join(slices.Concat(h.Values(name), values), ",")}
			continue
		}
		h[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	if originator != "" && !slices.ContainsFunc(h.Values("Originator"), func(value string) bool { return strings.TrimSpace(value) != "" }) {
		h.Set("Originator", originator)
	}
	if _, present := h["User-Agent"]; !present {
		h["User-Agent"] = []string{""}
	}
	return h
}

func subscriptionEndpointURL(host Host, cfg routing.Configuration, path, query string) (string, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", code.Refuse(502, "code_connection_invalid")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + path
	u.RawPath = ""
	if err := host.ValidateEndpoint(u.String()); err != nil {
		return "", code.Refuse(502, "code_egress_refused")
	}
	u.RawQuery = query
	return u.String(), nil
}

func writeUpstreamResponseHeaders(w http.ResponseWriter, response *http.Response, rewrittenModel bool) {
	maps.Copy(w.Header(), wire.ForwardHeaders(response.Header, false))
	if rewrittenModel {
		w.Header().Del("Content-Encoding")
		w.Header().Del("Content-Length")
	}
	for name := range wire.ForwardTrailers(response.Trailer, response.Header, false) {
		w.Header().Add("Trailer", name)
	}
	w.WriteHeader(response.StatusCode)
}

func writeUpstreamResponseTrailers(w http.ResponseWriter, response *http.Response) {
	for name, values := range wire.ForwardTrailers(response.Trailer, response.Header, false) {
		w.Header()[http.TrailerPrefix+name] = values
	}
}

func BindWorkload(fresh *keyauthority.Authority, verified keyauthority.Authority) error {
	if fresh.WorkloadIssuerID == nil && verified.WorkloadIssuerID == nil {
		return nil
	}
	if fresh.WorkloadIssuerID == nil || verified.WorkloadIssuerID == nil || *fresh.WorkloadIssuerID != *verified.WorkloadIssuerID || fresh.WorkloadDigest != verified.WorkloadDigest || fresh.WorkloadMapping != verified.WorkloadMapping || fresh.WorkloadRevision != verified.WorkloadRevision {
		return code.Refuse(409, "code_workload_changed")
	}
	fresh.EndUserDigest = verified.EndUserDigest
	fresh.ExpiresAt = verified.ExpiresAt
	return nil
}
