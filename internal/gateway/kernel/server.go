package kernel

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/gateway/codeforward"
	"github.com/tyk-swe/olp/internal/gateway/delegation"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/health"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/models"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/gateway/traceingest"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/platform/concurrency"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

// Config bounds the inference surface.
type Config struct {
	Region             string
	MaxInFlight        int
	MaxBodyBytes       int64
	MaxMediaBodyBytes  int64
	MaxResponseBytes   int64
	MaxEventBytes      int64
	TrustedProxies     []netip.Prefix
	CORSAllowedOrigins []string
	InlineMedia        protocols.InlineMediaLimits
	// AdmissionPool, when set, is the process-local inference admission pool
	// the server falls back to for direct handler calls; process composition
	// shares it with the public admission middleware so both bound the same
	// capacity and the metrics endpoint reads one truth.
	AdmissionPool *concurrency.Pool
	// Signer runs the signing hooks of plugin profiles. Without one, a
	// provider whose profile declares signing authorizes no request.
	// Predicates runs the route_predicate hooks of confined plugins that
	// route selectors name. Without it plugin predicates never match.
	Predicates RoutePredicates
	Signer     connectors.Signer
	// Carrier runs the unconfined plugins that carry their profiles' traffic.
	// Without one, a provider whose profile's plugin carries its traffic
	// sends no request.
	Carrier connectors.Carrier
	// UnconfinedPlugins is set where the deployment enables unconfined
	// plugins. Elsewhere, the gateway refuses targets of providers whose
	// plugin is unconfined.
	UnconfinedPlugins bool
	// MaxShadowInFlight bounds the mirrored requests in flight on this
	// gateway; mirrors beyond it are dropped. Zero means the default.
	MaxShadowInFlight int
}

// Runtime is the pinned authority, release and credential source.
// *release.Manager implements it; fixtures and tests supply static releases.
type Runtime interface {
	Release() *release.Release
	// RoutingInputs returns current price and performance measurements, or
	// nil when no measurements are available.
	RoutingInputs() *routinginputs.Inputs
	Authenticate(secret string) (*keyauthority.Authority, error)
	// CheckRouteFidelity confirms a strict admission against current publication.
	CheckRouteFidelity(context.Context, routing.Route) error
	// GrantGeneration is the generation of the token currently served.
	GrantGeneration(credentialID string) int64
	// CredentialRefused reports that the upstream refused a credential
	// version's secret, so that a grant beneath the version is refreshed
	// early.
	CredentialRefused(credentialID string, generation int64)
	release.Credentials
}

type Digester interface {
	Digest(secrets.DigestPurpose, string) []byte
}

type BudgetAuthority interface {
	BudgetAuthority(*string) (keyauthority.Authority, error)
}

type TaskAttemptAdmitter interface {
	AdmitTaskCapabilityAttempt(context.Context, keyauthority.Authority, string, string, string, *string, string) error
}

type Dependencies struct {
	ObserveOverlap              func(string, keyauthority.SecretOverlap, time.Time)
	Egress                      *egress.Policy
	Getenv                      func(string) string
	Log                         *slog.Logger
	Cache                       *cache.Store
	Guardrails                  func(guardrails.Caller) guardrails.Evaluator
	Code                        *codeforward.Services
	Runtime                     Runtime
	Digester                    Digester
	BudgetAuthority             BudgetAuthority
	TaskAttemptAdmitter         TaskAttemptAdmitter
	TaskCapabilityAuthenticator consumerauth.TaskCapabilityAuthenticator
	DelegationReauthenticator   DelegationReauthenticator
	Sink                        events.Sink
	Admission                   *quota.Admission
	Debits                      *SessionDebits
	WorkflowRuns                WorkflowRunStore
	WorkflowTools               WorkflowToolRunner
	Traces                      *traceingest.Store
	Staging                     *staging.Spool
	Uploads                     *staging.Admission
	Capture                     *capture.Manager
	Business                    *telemetry.BusinessMetrics

	Resources *resources.Store
	Batches   *batches.Store

	Resolver *resources.Resolver
}

type Server struct {
	Deps Dependencies

	Log         *slog.Logger
	Config      Config
	Egress      *egress.Policy
	client      *http.Client
	connections *egress.ConnectionClientCache
	auth        *connectors.Auth
	Gate        *ingress.Gate
	code        *codeforward.Forwarder
	health      *health.Tracker
	fleet       *health.Fleet
	// unhealthy is fleet.unhealthy bound once, so selection never allocates
	// a method value per request.
	unhealthy func(string) bool
	shadows   *shadowing
	Now       func() time.Time
	counted   func(estimate.Family)
	exposures *sessionExposures
}

type sessionExposures struct {
	mu   sync.Mutex
	held map[string]time.Time
}

func newSessionExposures() *sessionExposures {
	return &sessionExposures{held: map[string]time.Time{}}
}

const UpstreamHeaderTimeout = 5 * time.Minute

func New(deps Dependencies, cfg Config) *Server {
	policy, log := deps.Egress, deps.Log
	config := normalizedConfig(cfg)
	pool := inferencePool(config)

	auth := connectors.NewAuth(policy, deps.Getenv)
	auth.Signer = config.Signer
	// The tokenizers parse their rank tables on first use, which would land on
	// the first request that needs one. A table that cannot be read leaves its
	// family on the heuristic, which says so in every estimate it makes.
	if err := estimate.Preload(); err != nil {
		log.Warn("tokenizer unavailable; its models are estimated by the four-characters-per-token rule", "error", err.Error())
	}
	fleet := health.NewFleet(time.Now)
	normalized := normalizedDependencies(deps)
	gate := ingress.NewGate(normalized.Runtime, pool, config.TrustedProxies, config.MaxBodyBytes, time.Now)
	return &Server{
		Log:         log,
		Config:      config,
		Egress:      policy,
		client:      policy.Client(UpstreamHeaderTimeout),
		connections: egress.NewConnectionClientCache(128),
		auth:        auth,
		Gate:        gate,
		code:        codeForwarder(normalized, config, gate, auth),
		health:      health.NewTracker(time.Now),
		fleet:       fleet,
		unhealthy:   fleet.Unhealthy,
		shadows:     newShadowing(config.MaxShadowInFlight),
		Now:         time.Now,
		exposures:   newSessionExposures(),
		Deps:        normalized,
	}
}

// AdmissionPool exposes the process-local inference admission pool the
// metrics endpoint renders.
func (s *Server) AdmissionPool() *concurrency.Pool { return s.Gate.Pool() }

// RunFleetHealth shares this gateway's circuit transitions with the fleet
// and follows the fleet's, until ctx ends. Without shared state it returns at
// once, and local circuits alone decide.
func (s *Server) RunFleetHealth(ctx context.Context) {
	if !s.Deps.Admission.Ready() {
		return
	}
	s.fleet.Follow(ctx, s.health, s.Deps.Admission.Fleet(), quota.ReserveTimeout, s.Log)
}

// OpenCircuits counts provider circuits currently open or half-open.
func (s *Server) OpenCircuits() int64 { return s.health.OpenCircuits() }

// GeminiActions are the Gemini model actions a surface serves, keyed by the
// name after the colon in /models/{model}:{action}.
type GeminiActions map[string]http.HandlerFunc

func (s *Server) RegisterKernel(mux *http.ServeMux, actions GeminiActions) {
	if s.code != nil {
		s.code.Register(mux)
	}
	mux.Handle("POST /v1/delegations", delegation.New(delegation.Dependencies{Gate: s.Gate, Keys: s.Deps.Runtime, Begin: func(w http.ResponseWriter, r *http.Request) { s.begin(w, r) }}))
	mux.Handle("POST /v1/traces", traceingest.New(s.Deps.Runtime, s.Config.TrustedProxies, s.Deps.Traces))
	catalog := models.New(models.Dependencies{
		Gate:     s.Gate,
		Begin:    func(w http.ResponseWriter, r *http.Request) *release.Release { return s.begin(w, r).release },
		Now:      s.Now,
		Workflow: s.workflowCapability,
	})
	s.registerNative(mux, catalog, actions)
	mux.HandleFunc("POST /v1/chat/completions", s.inference(wire.Chat))
	mux.HandleFunc("POST /v1/responses", s.inference(wire.Responses))
	mux.HandleFunc("GET /v1/models", catalog.List)
	mux.HandleFunc("GET /v1/models/{model}", catalog.Get)
	mux.HandleFunc("OPTIONS /v1/", s.Preflight)
	mux.HandleFunc("OPTIONS /bedrock/", s.Preflight)
	mux.HandleFunc("/v1/", s.unknown)
	mux.HandleFunc("/bedrock/", s.unknown)
}

func (s *Server) workflowCapability(ctx context.Context, rel *release.Release, authority keyauthority.Authority, route routing.Route, surface string) *routing.WorkflowCapability {
	return mcp.DescribeReachableWorkflow(ctx, s.Deps.WorkflowTools, authority, rel, route, surface, s.Deps.WorkflowRuns != nil && s.Deps.Debits != nil, s.Now())
}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// request carries per-request identity shared by handlers.
type request struct {
	delegationAssertion string
	guardrailMetadata   bool
	cacheDirectives     string
	callerCredential    *callerCredential
	body                *ingress.MeasuredBody
	id                  string
	durableID           string
	// minted records that this gateway chose the request id. A caller may name
	// its own, and nothing stops two callers from naming the same one.
	minted    bool
	clientIP  string
	startedAt time.Time
	release   *release.Release
	// trace is the request's observability span; a no-op when tracing is off.
	trace *telemetry.RequestTrace
	// counted, when set, hears each time the request's prompt is counted for a
	// model family. Tests use it to see that a family is counted once.
	counted func(estimate.Family)
}

// accountingID is the identity durable records are stored under: the request
// id when this gateway minted it, and a fresh one when the caller named it.
func (r *request) accountingID() string {
	if r.minted {
		return r.id
	}
	if r.durableID == "" {
		r.durableID = ids.New()
	}
	return r.durableID
}

func (s *Server) mintedRequest(clientIP string, release *release.Release) request {
	return request{id: ids.New(), minted: true, clientIP: clientIP, startedAt: s.Now(), release: release}
}

// begin assigns the request identity, pins the release, and sets the
// headers every inference response carries.
func (s *Server) begin(w http.ResponseWriter, r *http.Request) request {
	id := r.Header.Get("X-Request-Id")
	minted := !requestIDPattern.MatchString(id)
	if minted {
		id = ids.New()
	}
	h := w.Header()
	h.Set("X-Request-Id", id)
	if fault.RequestSurface(r) == "anthropic" {
		// The Anthropic SDKs report the request id of an error from this header.
		h.Set("Request-Id", id)
	}
	h.Set("Cache-Control", "no-store")
	s.cors(w, r)
	release := s.Deps.Runtime.Release()
	var measured *ingress.MeasuredBody
	if release != nil && release.Snapshot().HasBodyLimits() && r.Body != nil && r.Body != http.NoBody {
		measured = &ingress.MeasuredBody{ReadCloser: r.Body, Declared: r.ContentLength}
		r.Body = measured
	}
	return request{delegationAssertion: strings.TrimSpace(r.Header.Get("X-OLP-Agent-Identity")), cacheDirectives: strings.Join(r.Header.Values("X-OLP-Cache"), ","), guardrailMetadata: r.Header.Get("X-OLP-Guardrails") == "true", callerCredential: readCallerCredential(r.Header), body: measured, id: id, minted: minted, clientIP: consumerauth.ClientIP(r, s.Config.TrustedProxies), startedAt: s.Now(), release: release, trace: telemetry.RequestFromContext(r.Context()), counted: s.counted}
}

func (s *Server) BrowserOriginAllowed(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	return slices.Contains(s.Config.CORSAllowedOrigins, values[0]) || ingress.SameOrigin(r, values[0]) && values[0] == s.GatewayOrigin(r)
}

// cors permits browser SDK clients only from explicitly configured origins.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) {
	if !slices.Contains(s.Config.CORSAllowedOrigins, r.Header.Get("Origin")) {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
	h.Add("Vary", "Origin")
	expose := exposedHeaders
	if fault.RequestSurface(r) == "anthropic" {
		expose = exposedHeadersAnthropic
	}
	h.Set("Access-Control-Expose-Headers", expose)
}

func (s *Server) Preflight(w http.ResponseWriter, r *http.Request) {
	s.cors(w, r)
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Idempotency-Key, X-Api-Key, X-Goog-Api-Key, X-Goog-Api-Client, X-Goog-Upload-Protocol, X-Goog-Upload-Command, X-Goog-Upload-Header-Content-Length, X-Goog-Upload-Header-Content-Type, X-Goog-Upload-Offset, Anthropic-Version, Anthropic-Beta, Anthropic-Dangerous-Direct-Browser-Access, Content-Type, X-Request-Id, X-OLP-Routing, X-OLP-Cache, OpenAI-Organization, OpenAI-Project, OpenAI-Beta, X-OLP-API-Key, X-OLP-Client-Contract, X-OLP-Route, X-OLP-Attribution, X-OLP-Guardrails, X-OLP-End-User, X-OLP-Provider-Credential, X-OLP-Continuation, X-OLP-Continuation-Handle, X-OLP-Submission-ID, X-OLP-Delegating-Key, X-OLP-Delegation-Consent, X-OLP-Agent-Identity, X-Stainless-Lang, X-Stainless-Package-Version, X-Stainless-OS, X-Stainless-Arch, X-Stainless-Runtime, X-Stainless-Runtime-Version, X-Stainless-Retry-Count, X-Stainless-Timeout, X-Stainless-Helper-Method")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unknown(w http.ResponseWriter, r *http.Request) {
	s.begin(w, r)
	fault.WriteError(w, fault.NotFoundError("not_found", "Unknown endpoint "+r.Method+" "+r.URL.Path+"."))
}

func (s *Server) GatewayOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else {
		remote, _, err := net.SplitHostPort(r.RemoteAddr)
		addr, parseErr := netip.ParseAddr(remote)
		trusted := false
		if err == nil && parseErr == nil {
			for _, prefix := range s.Config.TrustedProxies {
				if prefix.Contains(addr.Unmap()) {
					trusted = true
					break
				}
			}
		}
		if trusted && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
	}
	return scheme + "://" + r.Host
}

func (s *Server) LimiterCounts() (failOpen, daily, weekly, monthly int64) {
	admission := s.Deps.Admission
	if admission == nil {
		return 0, 0, 0, 0
	}
	return admission.FailOpenTotal(), admission.BudgetRejections(limits.DimensionDailyCost), admission.BudgetRejections(limits.DimensionWeeklyCost), admission.BudgetRejections(limits.DimensionMonthlyCost)
}
