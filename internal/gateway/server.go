package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/admission"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/export/capture"
	"github.com/tyk-swe/olp/internal/gateway/codeforward"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/health"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/gateway/traceingest"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/mcp"
	"github.com/tyk-swe/olp/internal/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/priority"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/telemetry"
	"github.com/tyk-swe/olp/internal/usage"
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
	AdmissionPool *admission.Pool
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
	WebRTCBound       time.Duration
	WebRTCReauth      time.Duration
	WebRTCHeartbeat   time.Duration
}

// Runtime is the pinned authority, release and credential source.
// *release.Manager implements it; fixtures and tests supply static releases.
type Runtime interface {
	// CheckRouteFidelity confirms a strict admission against current publication.
	CheckRouteFidelity(context.Context, routing.Route) error
	Release() *release.Release
	Authenticate(secret string) (*keyauthority.Authority, error)
	// RoutingInputs returns current price and performance measurements, or
	// nil when no measurements are available.
	RoutingInputs() *routinginputs.Inputs
	release.Credentials
	// GrantGeneration is the generation of the token currently served.
	GrantGeneration(credentialID string) int64
	// CredentialRefused reports that the upstream refused a credential
	// version's secret, so that a grant beneath the version is refreshed
	// early.
	CredentialRefused(credentialID string, generation int64)
}

type Dependencies struct {
	Egress         *egress.Policy
	Log            *slog.Logger
	Cache          *cache.Store
	Guardrails     func(guardrails.Caller) guardrails.Evaluator
	CodeLedger     codeforward.Ledger
	CodeAuthorizer codeforward.Authorizer
	CodeResearch   codeforward.ResearchLedger
	ResearchAuth   codeforward.ResearchAuthorizer
	Runtime        Runtime
	Sink           events.Sink
	Admission      *quota.Admission
	Debits         SessionDebitLedger
	WorkflowRuns   WorkflowRunStore
	WorkflowTools  WorkflowToolRunner
	Traces         *traceingest.Store
	Media          *MediaDeps
	Staging        *staging.Spool
	Uploads        *staging.Admission
	Capture        *capture.Manager
	Business       *telemetry.BusinessMetrics

	Resources *resources.Store
	Batches   *batches.Store

	Resolver *resources.Resolver
}

type Server struct {
	deps Dependencies

	log         *slog.Logger
	cfg         Config
	egress      *egress.Policy
	client      *http.Client
	connections *egress.ConnectionClientCache
	auth        *connectors.Auth
	gate        *ingress.Gate
	code        *codeforward.Forwarder
	health      *health.Tracker
	fleet       *health.Fleet
	// unhealthy is fleet.unhealthy bound once, so selection never allocates
	// a method value per request.
	unhealthy       func(string) bool
	shadows         *shadowing
	now             func() time.Time
	counted         func(estimate.Family)
	webrtcMu        sync.Mutex
	webrtcSessions  map[string]*webrtcSession
	webrtcExposures map[string]time.Time
}

const UpstreamHeaderTimeout = 5 * time.Minute

func New(deps Dependencies, cfg Config) *Server {
	policy, log := deps.Egress, deps.Log
	config := normalizedConfig(cfg)
	pool := inferencePool(config)

	auth := connectors.NewAuth(policy)
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
		log:         log,
		cfg:         config,
		egress:      policy,
		client:      policy.Client(UpstreamHeaderTimeout),
		connections: egress.NewConnectionClientCache(128),
		auth:        auth,
		gate:        gate,
		code:        codeForwarder(normalized, config, gate, auth),
		health:      health.NewTracker(time.Now),
		fleet:       fleet,
		unhealthy:   fleet.Unhealthy,
		shadows:     newShadowing(config.MaxShadowInFlight),
		now:         time.Now, deps: normalized,
	}
}

// AdmissionPool exposes the process-local inference admission pool the
// metrics endpoint renders.
func (s *Server) AdmissionPool() *admission.Pool { return s.gate.Pool() }

// RunFleetHealth shares this gateway's circuit transitions with the fleet
// and follows the fleet's, until ctx ends. Without shared state it returns at
// once, and local circuits alone decide.
func (s *Server) RunFleetHealth(ctx context.Context) {
	if !s.deps.Admission.Ready() {
		return
	}
	s.fleet.Follow(ctx, s.health, s.deps.Admission.Fleet(), quota.ReserveTimeout, s.log)
}

// OpenCircuits counts provider circuits currently open or half-open.
func (s *Server) OpenCircuits() int64 { return s.health.OpenCircuits() }

// Register mounts the OpenAI surface on the public mux.
func (s *Server) Register(mux *http.ServeMux) {
	if s.code != nil {
		s.code.Register(mux)
	}
	mux.HandleFunc("POST /v1/guardrails/apply", s.applyGuardrails)
	mux.HandleFunc("POST /v1/delegations", s.createDelegation)
	mux.Handle("POST /v1/traces", traceingest.New(s.deps.Runtime, s.cfg.TrustedProxies, s.deps.Traces))
	s.registerNative(mux)
	s.registerGeminiLifecycle(mux)
	s.registerMedia(mux)
	s.registerState(mux)
	mux.HandleFunc("POST /v1/realtime/calls", s.realtimeCallsCreate)
	mux.HandleFunc("POST /v1/chat/completions", s.inference(wire.Chat))
	mux.HandleFunc("POST /v1/responses", s.inference(wire.Responses))
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /v1/models/{model}", s.model)
	mux.HandleFunc("OPTIONS /v1/", s.preflight)
	mux.HandleFunc("OPTIONS /bedrock/", s.preflight)
	mux.HandleFunc("/v1/", s.unknown)
	mux.HandleFunc("/bedrock/", s.unknown)
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
		r.durableID = uuid.Must(uuid.NewV7()).String()
	}
	return r.durableID
}

// begin assigns the request identity, pins the release, and sets the
// headers every inference response carries.
func (s *Server) begin(w http.ResponseWriter, r *http.Request) request {
	id := r.Header.Get("X-Request-Id")
	minted := !requestIDPattern.MatchString(id)
	if minted {
		id = uuid.Must(uuid.NewV7()).String()
	}
	h := w.Header()
	h.Set("X-Request-Id", id)
	if fault.RequestSurface(r) == "anthropic" {
		// The Anthropic SDKs report the request id of an error from this header.
		h.Set("Request-Id", id)
	}
	h.Set("Cache-Control", "no-store")
	s.cors(w, r)
	release := s.deps.Runtime.Release()
	var measured *ingress.MeasuredBody
	if release != nil && release.Snapshot().HasBodyLimits() && r.Body != nil && r.Body != http.NoBody {
		measured = &ingress.MeasuredBody{ReadCloser: r.Body, Declared: r.ContentLength}
		r.Body = measured
	}
	return request{delegationAssertion: strings.TrimSpace(r.Header.Get("X-OLP-Agent-Identity")), cacheDirectives: strings.Join(r.Header.Values("X-OLP-Cache"), ","), guardrailMetadata: r.Header.Get("X-OLP-Guardrails") == "true", callerCredential: readCallerCredential(r.Header), body: measured, id: id, minted: minted, clientIP: consumerauth.ClientIP(r, s.cfg.TrustedProxies), startedAt: s.now(), release: release, trace: telemetry.RequestFromContext(r.Context()), counted: s.counted}
}

func (s *Server) browserOriginAllowed(r *http.Request) bool {
	values := r.Header.Values("Origin")
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	return slices.Contains(s.cfg.CORSAllowedOrigins, values[0]) || ingress.SameOrigin(r, values[0]) && values[0] == s.gatewayOrigin(r)
}

// cors permits browser SDK clients only from explicitly configured origins.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) {
	if !slices.Contains(s.cfg.CORSAllowedOrigins, r.Header.Get("Origin")) {
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

func (s *Server) preflight(w http.ResponseWriter, r *http.Request) {
	s.cors(w, r)
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, PATCH, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Idempotency-Key, X-Api-Key, X-Goog-Api-Key, X-Goog-Api-Client, X-Goog-Upload-Protocol, X-Goog-Upload-Command, X-Goog-Upload-Header-Content-Length, X-Goog-Upload-Header-Content-Type, X-Goog-Upload-Offset, Anthropic-Version, Anthropic-Beta, Anthropic-Dangerous-Direct-Browser-Access, Content-Type, X-Request-Id, X-OLP-Routing, X-OLP-Cache, OpenAI-Organization, OpenAI-Project, OpenAI-Beta, X-OLP-API-Key, X-OLP-Client-Contract, X-OLP-Route, X-OLP-Attribution, X-OLP-Guardrails, X-OLP-End-User, X-OLP-Provider-Credential, X-OLP-Continuation, X-OLP-Continuation-Handle, X-OLP-Submission-ID, X-OLP-Delegating-Key, X-OLP-Agent-Identity, X-Stainless-Lang, X-Stainless-Package-Version, X-Stainless-OS, X-Stainless-Arch, X-Stainless-Runtime, X-Stainless-Runtime-Version, X-Stainless-Retry-Count, X-Stainless-Timeout, X-Stainless-Helper-Method")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unknown(w http.ResponseWriter, r *http.Request) {
	s.begin(w, r)
	fault.WriteError(w, fault.NotFoundError("not_found", "Unknown endpoint "+r.Method+" "+r.URL.Path+"."))
}

func (s *Server) gatewayOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else {
		remote, _, err := net.SplitHostPort(r.RemoteAddr)
		addr, parseErr := netip.ParseAddr(remote)
		trusted := false
		if err == nil && parseErr == nil {
			for _, prefix := range s.cfg.TrustedProxies {
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

// routingHeader carries per-request routing preferences and the admission
// priority. Preferences can only lower the published attempt budget, and the
// priority can only stay within the key's ceiling.
const routingHeader = "X-OLP-Routing"

// routingControls reads the routing header and resolves the request's
// admission class: the key's own priority unless the header asks for another
// within the key's ceiling. Asking above it is refused like any other budget
// increase.
func routingControls(r *http.Request, policy keyauthority.KeyPolicy) (*routing.Preferences, string, *fault.Error) {
	preferences, e := routingPreferences(r)
	if e != nil {
		return nil, "", e
	}
	if preferences == nil || preferences.Priority == nil {
		return preferences, policy.DefaultPriority(), nil
	}
	requestedPriority, ceiling := *preferences.Priority, policy.PriorityCeiling()
	if priority.Rank(requestedPriority) < priority.Rank(ceiling) {
		param := routingHeader
		return nil, "", fault.InvalidRequest("priority_increase_forbidden", "This API key may request priority "+ceiling+" at most.", &param)
	}
	return preferences, requestedPriority, nil
}

func routingPreferences(r *http.Request) (*routing.Preferences, *fault.Error) {
	values := r.Header.Values("X-OLP-Routing")
	if len(values) == 0 {
		return nil, nil
	}
	param := "X-OLP-Routing"
	if len(values) != 1 {
		return nil, fault.InvalidRequest("invalid_request", "Provide exactly one X-OLP-Routing header.", &param)
	}
	p, err := routing.ParsePreferences([]byte(values[0]))
	if err != nil {
		return nil, fault.InvalidRequest("invalid_request", err.Error(), &param)
	}
	return p, nil
}
func requestError(err error) *fault.Error {
	var guard guardrails.Failure
	if errors.As(err, &guard) {
		return guardrailError(guard)
	}
	if gatewayError, ok := errors.AsType[*fault.Error](err); ok {
		return gatewayError
	}
	var contract interface {
		Incompatibility() (code, field, requirement, message string)
	}
	if errors.As(err, &contract) {
		code, field, requirement, message := contract.Incompatibility()
		var param *string
		if field != "" {
			param = &field
		}
		if code == "target_capability" && requirement == "source_control_mapping" {
			// Preserve the established ingress error for an unmapped control;
			// the planner and inspector retain its precise requirement/category.
			return fault.InvalidRequest("unsupported_parameter", message, param)
		}
		return fault.InvalidRequest(code, message, param)
	}
	if re, ok := errors.AsType[*wire.RequestError](err); ok {
		var param *string
		if re.Param != "" {
			param = &re.Param
		}
		return fault.InvalidRequest(re.Code, re.Message, param)
	}
	return fault.InvalidRequest("invalid_request", err.Error(), nil)
}

// noteCostExceeded remembers that a leg this request planned excluded a
// non-shadow target for its maximum cost; when nothing serves, the refusal is
// 400 cost_limit_exceeded rather than route-unavailable.
func (x *call) noteCostExceeded(plan routing.Plan) {
	for index := range plan.Decisions {
		if plan.Decisions[index].Reason != nil && *plan.Decisions[index].Reason == "cost_limit_exceeded" && !plan.Decisions[index].Shadow {
			x.costExcluded = true
			return
		}
	}
}

// costLimitExceeded is the refusal a request ends on when a leg it planned
// excluded a non-shadow target for its maximum cost.
func costLimitExceeded() *fault.Error {
	return fault.InvalidRequest("cost_limit_exceeded", "Every candidate exceeds the request's maximum cost.", func() *string { p := "max_cost"; return &p }())
}

func selectionError(err error, model string) *fault.Error {
	if se, ok := errors.AsType[*routing.SelectionError](err); ok {
		switch se.Code {
		case routing.RouteNotFound:
			return fault.ModelNotFound(model)
		case routing.OperationNotSupported:
			return fault.InvalidRequest("invalid_request", "The model `"+model+"` does not allow this operation.", nil)
		}
	}
	return fault.ServerError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently eligible to serve `"+model+"`.")
}

const requestBodyTimeout = 15 * time.Second

func (s *Server) inference(family wire.Format) http.HandlerFunc {
	return s.inferenceOperation(family, "")
}
func (s *Server) inferenceOperation(family wire.Format, dialect string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		x := &call{request: s.begin(w, r), family: family, actor: "api_key"}
		if dialect == "" && fault.RequestSurface(r) == "gemini" {
			// Gemini embeddings match OpenAI embedding capabilities but
			// speak Gemini on the wire.
			x.ingress = "gemini"
		}
		writeError := func(w http.ResponseWriter, e *fault.Error) {
			if x.strict() && x.dispatched && e.Status >= 500 {
				copy := *e
				copy.NoRetry = true
				e = &copy
			}
			fault.WriteSurfaceError(w, e, x.clientSurface())
		}
		x.semanticHeaders = semanticHeaders(r.Header)
		query, queryErr := url.ParseQuery(r.URL.RawQuery)
		x.semanticQuery, x.semanticQueryInvalid = query, queryErr != nil
		status := http.StatusInternalServerError
		var out *outcome
		defer func() {
			s.finish(x, out, status)
			// The budgets this request reserved are settled even when the
			// client is gone: a concurrency slot nobody releases is a slot
			// every replica keeps counting.
			s.settleAdmission(r.Context(), x)
			// Mirrors start once the caller's response is complete, so they
			// never add to its latency.
			if out != nil {
				s.mirror(x)
			}
		}()
		// A context timeout alone cannot interrupt a blocked socket read.
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(requestBodyTimeout)); err != nil {
			e := fault.ServerError(http.StatusInternalServerError, "internal_error", "The request could not be read.")
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		if !s.gate.AdmitLater(r.Context()) {
			x.failure, status = fault.Overloaded, fault.Overloaded.Status
			writeError(w, fault.Overloaded)
			return
		}
		defer s.gate.Release(r.Context())
		token, e := ingress.PresentedKey(r)
		if e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		authority, e := s.gate.AuthenticateRequest(r, token, "inference")
		if e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		s.bindAuthority(x, authority)
		x.mirrors = true
		if x.attribution, e = ingress.ParseAttribution(r, authority); e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		body, e := s.gate.ReadBody(r)
		if e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		// Keep the deadline on failed reads so HTTP/1 body draining stays
		// bounded; successful uploads must not limit the inference stream.
		rc.SetReadDeadline(time.Time{})
		if selected, err := s.selectUnary(x, family, dialect, body, r.PathValue("model"), r.Header.Get(stateRouteHeader)); selected || err != nil {
			if err != nil {
				e = requestError(err)
				x.failure, status = e, e.Status
				writeError(w, e)
				return
			}
			out, status = s.serveUnary(w, r, x, body)
			return
		}
		var parsed *wire.Request
		var err error
		if family == wire.Chat || family == wire.Responses || family == wire.Anthropic || family == wire.Gemini || family == wire.GeminiStream {
			var prompt parsedRequest
			prompt, e = s.parsePrompt(x, family, body, r.PathValue("model"), s.keyAuthorizer(x.authority))
			parsed, x.prompt = prompt.request, prompt.reference
			if x.request.body != nil {
				x.request.body.Decoded = max(x.request.body.Decoded, prompt.decoded)
			}
		} else if family == wire.Bedrock {
			parsed, err = protocols.ParseBedrockRequest(body, r.PathValue("model"), strings.HasSuffix(r.URL.Path, "/converse-stream"))
		} else {
			parsed, err = protocols.Parse(family, body, r.PathValue("model"))
		}
		if err != nil {
			e = requestError(err)
		}
		if e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		if err := protocols.ValidateInlineMedia(parsed, s.cfg.InlineMedia); err != nil {
			e = requestError(err)
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		x.generation.parsed, x.generation.approval, err = stripWorkflowApproval(parsed)
		if err != nil {
			e = fault.InvalidRequest("invalid_request", "The workflow approval could not be removed from the request.", nil)
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		if e := s.prepareContinuation(r.Context(), x); e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		if x.generation.continuation != nil && x.generation.continuation.replay != nil {
			var deliveryErr error
			status, deliveryErr = s.replayContinuation(w, x)
			out = continuationDeliveryOutcome(deliveryErr)
			// Replaying a committed response never starts shadow inference.
			x.mirrors = false
			return
		}
		if family == wire.Responses {
			if e := s.responsesStateGate(r.Context(), x, authority, parsed); e != nil {
				x.failure, status = e, e.Status
				writeError(w, e)
				return
			}
		}
		if e := s.bindGenerationReferences(r.Context(), x); e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		if x.preferences, x.priority, e = routingControls(r, authority.Policy); e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		if e := s.prepare(r.Context(), x, s.keyAuthorizer(authority)); e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		if x.named().ToolExecution == "gateway" {
			if e := s.prepareWorkflow(r.Context(), x, token); e != nil {
				x.failure, status = e, e.Status
				writeError(w, e)
				return
			}
			if e := s.prepareWorkflowResume(r.Context(), x); e != nil {
				x.failure, status = e, e.Status
				writeError(w, e)
				return
			}
		} else if len(x.generation.approval) > 0 {
			e := fault.PermissionError("tool_approval_invalid", "The workflow approval is invalid or expired.")
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		if s.deps.Capture != nil && x.prompt == nil && x.workflow == nil && x.origin == "" && x.operationName() == "generation" {
			x.captureContext = r.Context()
			x.capture = s.deps.Capture.Resolve(x.captureProject(), x.named().Slug, x.keyID, x.endUserDigest, x.request.accountingID())
		}

		// Admission happens once the request is understood and before any
		// provider is called, so a rejected request costs an upstream nothing.
		// The lease outlives the route deadline it is sized against: it is the
		// backstop for a replica that dies mid-request, not the deadline.
		x.estimate = requestEstimate(x)
		overall := time.Duration(x.named().OverallTimeout) * time.Millisecond
		// Admission is part of the same deadline as execution; starting a new
		// full deadline afterwards could outlive the concurrency reservation.
		ctx, cancel := x.routeContext(r.Context())
		defer cancel()
		if e = s.gate.AwaitAdmission(ctx, x.priority); e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		reservationEstimate := quota.KeyReservationEstimate(x.estimate, s.dispatchableAttempts(x))
		admitted := x.admissionAuthority(authority)
		if e = s.admitCacheAware(ctx, x, &admitted, reservationEstimate, overall, s.costReservation(x, authority)); e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		workflowLease, e := s.reserveWorkflowRun(ctx, x)
		if e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		defer func() {
			if err := workflowLease.Release(ctx); err != nil {
				s.deps.Admission.Logger().Error("release workflow concurrency", "error", err)
			}
		}()
		if x.workflow != nil && x.workflow.runID == "" {
			runID, err := s.deps.WorkflowRuns.Start(ctx, authority, x.named().ID, x.named().RevisionID)
			if err != nil {
				x.failure = fault.ServerError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The workflow run could not be reserved.")
				status = x.failure.Status
				writeError(w, x.failure)
				return
			}
			x.workflow.runID = runID
		}
		if parsed.Stream && x.workflow == nil {
			sw := &fault.StreamWriter{W: w, Family: family, Headers: streamedHeaders{x}}
			x.generation.emit = func(frame []byte) error {
				err := sw.Emit(frame)
				if err == nil {
					x.captureCacheFrame(frame)
					x.delivered(s.now())
				}
				return err
			}
			out = s.executeModelStep(ctx, x, x.generation.parsed, x.authority)
			status = sw.Finish(out.err, out.cancelled)
			return
		}
		if x.workflow != nil {
			out = s.executeToolLoop(ctx, x)
		} else {
			out = s.executeModelStep(ctx, x, x.generation.parsed, x.authority)
		}
		if out.err != nil {
			status = out.err.Status
			if out.err.Status > 0 {
				writeError(w, out.err)
			}
			return
		}
		if out.approval != nil {
			if parsed.Stream {
				frames, err := workflowPauseFrames(family, x.named().Slug, x.request.accountingID(), out.approval, int(s.cfg.MaxEventBytes), int(s.cfg.MaxResponseBytes))
				if err != nil {
					out.err = fault.ServerError(http.StatusBadGateway, "invalid_tool_response", "The approval response could not be prepared.")
					status = out.err.Status
					writeError(w, out.err)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				status = http.StatusOK
				if _, err := w.Write(frames); err != nil {
					out.cancelled = true
				}
				return
			}
			w.Header().Set("Content-Type", "application/json")
			status = http.StatusOK
			if err := json.NewEncoder(w).Encode(out.approval); err != nil {
				out.cancelled = true
			}
			return
		}
		if parsed.Stream && x.workflow != nil {
			sw := &fault.StreamWriter{W: w, Family: family, Headers: streamedHeaders{x}}
			if err := sw.Emit(x.workflow.finalFrames); err != nil {
				out.err = (&attemptFailure{class: classCancelled}).toError()
				out.cancelled = true
			} else {
				out.committed = true
				x.facts[len(x.facts)-1].Committed = true
				x.delivered(s.now())
			}
			status = sw.Finish(out.err, out.cancelled)
			return
		}
		if err := rc.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
			out.err = fault.ServerError(http.StatusInternalServerError, "internal_error", "The response could not be written.")
			status = out.err.Status
			writeError(w, out.err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if family == wire.Responses && x.providerState {
			mapped, me := s.mapStoredResponse(r.Context(), x, authority, out.completion.Body)
			if me != nil {
				out.err = me
				status = me.Status
				writeError(w, me)
				return
			}
			out.completion.Body = mapped
		}
		if family == wire.Responses && len(x.facts) > 0 {
			identity := responseNativeIdentity(x, &x.facts[len(x.facts)-1], "")
			mapped, err := s.mapGenerationResultReferences(r.Context(), x, identity, out.completion.Body)
			if err != nil {
				out.err = fault.ServerError(http.StatusBadGateway, "resource_affinity", "The provider returned an unowned result reference.")
				status = out.err.Status
				writeError(w, out.err)
				return
			}
			out.completion.Body = mapped
		}
		enforced, pe := s.enforceContentOutput(x, out.completion.Body)
		if pe != nil {
			out.err = pe
			status = pe.Status
			writeError(w, pe)
			return
		}
		out.completion.Body = enforced
		status = http.StatusOK
		x.responseHeaders(w.Header(), false)
		w.WriteHeader(http.StatusOK)
		out.committed = true
		x.facts[len(x.facts)-1].Committed = true
		if fact := &x.facts[len(x.facts)-1]; fact.Interaction != nil {
			fact.Interaction.ClientState = usage.ClientPartial
		}
		_, err = w.Write(out.completion.Body)
		if err == nil {
			// Flush buffered responses before recording successful delivery.
			err = rc.Flush()
		}
		if err != nil {
			out.err = (&attemptFailure{class: classCancelled}).toError()
			out.cancelled = true
		} else {
			x.delivered(s.now())
			if fact := &x.facts[len(x.facts)-1]; fact.Interaction != nil {
				fact.Interaction.ClientState = usage.ClientTerminal
			}
		}
	}
}

// prepare resolves the route, lets authorize refuse the caller's use of it in
// the caller's own terms, and ranks the eligible attempts against the pinned
// snapshot.
func (s *Server) prepare(ctx context.Context, x *call, authorize func(*routing.Route) *fault.Error) *fault.Error {
	x.mode = "unary"
	if x.generation.parsed.Stream {
		x.mode = "streaming"
	}
	route, ok := x.snapshot().FindRoute(x.generation.parsed.Route)
	if !ok {
		return fault.ModelNotFound(x.generation.parsed.Route)
	}
	x.route = &route
	if e := authorize(&route); e != nil {
		return e
	}
	if route.ToolExecution != "gateway" && len(x.generation.approval) > 0 {
		return fault.PermissionError("tool_approval_invalid", "The workflow approval is invalid or expired.")
	}
	if route.ToolExecution == "gateway" && x.strict() {
		return fault.InvalidRequest("tool_execution_strict_route", "Gateway tool execution requires a transformed route.", nil)
	}
	if route.ToolExecution == "gateway" && !x.generation.toolsetsSelected {
		x.mirrors = false
		bounds, callerBounds, bounded, boundErr := workflowBounds(route.RunLimits, x.authority.Policy.RunLimits, x.generation.parsed)
		if boundErr != nil {
			return boundErr
		}
		x.generation.runLimits = bounds
		x.generation.callerRunLimits = callerBounds
		x.generation.parsed = bounded
		selected, parsed, e := selectToolsets(route.Toolsets, x.generation.parsed)
		if e != nil {
			return e
		}
		x.generation.toolsets = selected
		x.generation.parsed = parsed
		x.generation.toolsetsSelected = true
	}
	if route.ToolExecution == "gateway" && (s.deps.WorkflowRuns == nil || s.deps.WorkflowTools == nil || s.deps.Debits == nil) {
		return fault.ServerError(http.StatusServiceUnavailable, "tool_execution_unavailable", "Gateway tool execution requires durable accounting and tool services.")
	}
	if x.pin != nil {
		if e := s.pinAttempts(ctx, x); e != nil {
			return e
		}
	}
	// A pinned resource, a continuation and provider state belong to the
	// route that holds them, so those requests never leave it.
	x.bind(canonicalPlanner, authorize, x.pin != nil || x.generation.continuation != nil || x.providerState)
	return s.planNamed(ctx, x)
}

// planCanonical plans x.route for a canonical request and applies the route's
// content policy to it. A selector that delegates moves the request on to its
// route, which is planned in turn.
func (s *Server) planCanonical(ctx context.Context, x *call) *fault.Error {
	if x.named().ToolExecution == "gateway" && x.route.Fidelity.Strict() {
		return fault.InvalidRequest("tool_execution_strict_route", "Gateway tool execution requires a transformed route.", nil)
	}
	if e := s.bindGuardrails(ctx, x); e != nil {
		return e
	}
	if x.prompt != nil && x.route.Fidelity.Strict() {
		param := "olp_prompt"
		return fault.InvalidRequest("invalid_prompt", "Published prompts require a transformed route.", &param)
	}
	if e := s.checkRouteFidelity(ctx, x.route); e != nil {
		return e
	}
	if x.strict() {
		if x.actor == "playground" {
			param := "client_contract"
			return fault.InvalidRequest("state_carrier", "This playground projection does not yet preserve the strict native interaction; use the admitted native API client.", &param)
		}
		if x.semanticQueryInvalid {
			param := "query"
			return fault.InvalidRequest("invalid_request", "The query must be unambiguous URL-encoded parameters.", &param)
		}
		if e := x.dropIngressQuery(); e != nil {
			return e
		}
	}
	if e := x.checkAnthropicBeta(); e != nil {
		return e
	}
	session := s.planning(x, canonicalPlanning, x.route, x.family.Operation(), x.family.Surface(), x.mode)
	source := x.summarizeSource()
	options := s.selectionOptions(ctx, x)
	// A target's context window is weighed against the request as the model that
	// would serve it counts it, so the demand follows the target.
	options.Parameters = source.parameters
	options.Demand = func(_ routing.Provider, t routing.Target) *routing.TokenDemand {
		if x.guardrailEngine.HasAction(guardrails.Input, guardrails.Redact) || x.guardrailEngine.HasAction(guardrails.Input, guardrails.Mask) {
			return nil
		}
		return x.sourceDemand(t.ProviderModel)
	}
	options.Prepare = func(p routing.Provider, t routing.Target) (*routing.CandidatePreparation, error) {
		cfg := p.Connector()
		if x.named().ToolExecution == "gateway" && !p.SupportsToolCalls(t.ProviderModel, x.family.Surface(), x.mode) {
			return nil, errors.New("model tool-call capability unavailable")
		}
		if p.Network != nil && p.Network.CredentialID != "" && s.deps.Runtime.Eligibility(p.Network.CredentialID) != routing.Eligible {
			return nil, errors.New("provider network credential unavailable")
		}
		if !cfg.Supports(x.family.Operation(), x.family.Surface(), x.mode) {
			return nil, errors.New("connector capability unavailable")
		}
		if x.providerState && !stateQualified(&p, t.ProviderModel, x.family.Operation(), x.mode) {
			session.semantic = errors.New("provider-state capability unavailable")
			return nil, session.semantic
		}
		if !x.strict() && protocols.StructuredOutputRequested(x.generation.parsed) {
			metadata, _ := p.Metadata(t.ProviderModel)
			if metadata.SupportedParameters == nil || !slices.Contains(*metadata.SupportedParameters, "response_format") {
				session.semantic = errors.New("model does not declare structured-output support")
				return nil, session.semantic
			}
		}
		prepared, err := x.prepareCandidate(&p, t.ID, t.ProviderModel)
		if err != nil {
			session.semantic, session.decisions = err, prepared.policyDecisions
			return nil, err
		}
		return prepared.candidate, nil
	}
	options.Preferences = x.legPreferences()
	_, delegated, failure := session.selectPlan(ctx, options)
	if failure != nil || delegated {
		return failure
	}
	return s.enforceContentPolicy(x)
}

// modelObject renders a route as an OpenAI model object.
func (s *Server) modelObject(ctx context.Context, release *release.Release, authority keyauthority.Authority, route *routing.Route) map[string]any {
	capabilities := s.modelCapabilities(ctx, release, authority, *route, "openai")
	return map[string]any{
		"id":           route.Slug,
		"object":       "model",
		"created":      route.PublishedAt.Unix(),
		"owned_by":     "openllmproxy",
		"capabilities": capabilities,
	}
}

type modelCapabilities struct {
	routing.RouteCapabilities
	GatewayExecution *routing.WorkflowCapability `json:"gateway_execution,omitempty"`
}

func (s *Server) modelCapabilities(ctx context.Context, release *release.Release, authority keyauthority.Authority, route routing.Route, surface string) modelCapabilities {
	return modelCapabilities{
		RouteCapabilities: routing.EffectiveCapabilities(release.Snapshot(), route),
		GatewayExecution:  mcp.DescribeReachableWorkflow(ctx, s.deps.WorkflowTools, authority, release, route, surface, s.deps.WorkflowRuns != nil && s.deps.Debits != nil, s.now()),
	}
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	req := s.begin(w, r)
	authority, e := s.gate.Authenticate(r, "models_read")
	if e != nil {
		fault.WriteError(w, e)
		return
	}
	now := s.now()
	data := []map[string]any{}
	for _, slug := range req.release.Snapshot().RouteSlugs() {
		if authority.Allows("models_read", slug, req.release.Snapshot().Route(slug).ProjectID, now) {
			route := req.release.Snapshot().Route(slug)
			data = append(data, s.modelObject(r.Context(), req.release, authority, &route))
		}
	}
	fault.WriteJSON(w, map[string]any{"object": "list", "data": data})
}

func (s *Server) model(w http.ResponseWriter, r *http.Request) {
	req := s.begin(w, r)
	authority, e := s.gate.Authenticate(r, "models_read")
	if e != nil {
		fault.WriteError(w, e)
		return
	}
	slug := r.PathValue("model")
	route, ok := req.release.Snapshot().FindRoute(slug)
	if !ok || !authority.Allows("models_read", slug, route.ProjectID, s.now()) {
		fault.WriteError(w, fault.ModelNotFound(slug))
		return
	}
	fault.WriteJSON(w, s.modelObject(r.Context(), req.release, authority, &route))
}

// WriteAdmissionOverload renders the surface's 503 rejection. The public
// admission middleware calls it before routing, so the request context is the
// only state available — writeSurfaceError applies the write deadline itself.
func WriteAdmissionOverload(w http.ResponseWriter, r *http.Request) {
	fault.WriteSurfaceError(w, fault.Overloaded, fault.RequestSurface(r))
}

// WriteNotFound renders the surface's 404 for a request nothing routed.
// Process composition mounts it under reserved inference prefixes the gateway
// only partially claims, so an unknown endpoint or a method mismatch still
// answers in the envelope the surface's other errors use.
func WriteNotFound(w http.ResponseWriter, r *http.Request, surface string) {
	fault.WriteSurfaceError(w, fault.NotFoundError("not_found", "Unknown endpoint "+r.Method+" "+r.URL.Path+"."), surface)
}

// bindAuthority attaches the authenticated caller's authority to x and pins
// its consumer rate card when the routing inputs are fresh. Stale inputs leave
// the request unpinned so accounting resolves the card at fact time.
func (s *Server) bindAuthority(x *call, authority keyauthority.Authority) {
	x.authority = authority
	x.keyID, x.affinity = authority.ID, []byte(authority.ID)
	x.endUserDigest = authority.EndUserDigest
	x.budgetGroupID = authority.BudgetGroupID
	x.responseMetadata = authority.Policy.ResponseMetadata
	if s.deps.Runtime == nil {
		return
	}
	inputs := s.deps.Runtime.RoutingInputs()
	x.routingInputs = inputs
	if authority.ProjectID != nil && inputs != nil {
		if revision, known := inputs.RateCardRevision(authority.ProjectID, s.now()); known {
			x.cardPinned = true
			if revision != nil {
				x.cardRevisionID = &revision.ID
				x.cardRev = revision
			}
		}
	}
}

func (s *Server) WorkflowResolver() WorkflowToolRunner {
	if s == nil || s.deps.WorkflowRuns == nil || s.deps.WorkflowTools == nil || s.deps.Debits == nil {
		return nil
	}
	return s.deps.WorkflowTools
}

func (s *Server) LimiterCounts() (failOpen, daily, weekly, monthly int64) {
	admission := s.deps.Admission
	if admission == nil {
		return 0, 0, 0, 0
	}
	return admission.FailOpenTotal(), admission.BudgetRejections(limits.DimensionDailyCost), admission.BudgetRejections(limits.DimensionWeeklyCost), admission.BudgetRejections(limits.DimensionMonthlyCost)
}

func normalizedConfig(cfg Config) Config {
	return Config{
		Region: cfg.Region, MaxInFlight: cfg.MaxInFlight, MaxBodyBytes: cfg.MaxBodyBytes,
		MaxMediaBodyBytes: positiveOrDefault(cfg.MaxMediaBodyBytes, 64<<20), MaxResponseBytes: cfg.MaxResponseBytes, MaxEventBytes: cfg.MaxEventBytes,
		TrustedProxies: slices.Clone(cfg.TrustedProxies), CORSAllowedOrigins: slices.Clone(cfg.CORSAllowedOrigins),
		InlineMedia: normalizedInlineMedia(cfg.InlineMedia), AdmissionPool: cfg.AdmissionPool, Predicates: cfg.Predicates, Signer: cfg.Signer, Carrier: cfg.Carrier,
		UnconfinedPlugins: cfg.UnconfinedPlugins, MaxShadowInFlight: cfg.MaxShadowInFlight,
		WebRTCBound: positiveOrDefault(cfg.WebRTCBound, time.Hour), WebRTCReauth: positiveOrDefault(cfg.WebRTCReauth, ingress.ReauthInterval), WebRTCHeartbeat: positiveOrDefault(cfg.WebRTCHeartbeat, realtimePing),
	}
}

func positiveOrDefault[T ~int64](value, fallback T) T {
	if value > 0 {
		return value
	}
	return fallback
}

func normalizedInlineMedia(value protocols.InlineMediaLimits) protocols.InlineMediaLimits {
	if value.Items != 0 {
		return value
	}
	return protocols.InlineMediaLimits{Items: 4, ItemBytes: 1 << 20, TotalBytes: 2 << 20}
}

func inferencePool(cfg Config) *admission.Pool {
	if cfg.AdmissionPool != nil {
		return cfg.AdmissionPool
	}
	return admission.NewPool(max(cfg.MaxInFlight, 1))
}

func codeForwarder(deps Dependencies, cfg Config, gate *ingress.Gate, auth *connectors.Auth) *codeforward.Forwarder {
	if deps.CodeLedger == nil || deps.CodeAuthorizer == nil {
		return nil
	}
	return codeforward.New(codeforward.Dependencies{
		Ledger: deps.CodeLedger, Authorizer: deps.CodeAuthorizer, Research: deps.CodeResearch, ResearchAuth: deps.ResearchAuth,
		Runtime: deps.Runtime, Sink: deps.Sink, Admission: deps.Admission, Gate: gate, Egress: deps.Egress, Auth: auth,
		Log: deps.Log, Now: time.Now, MaxBodyBytes: cfg.MaxBodyBytes, HeaderTimeout: UpstreamHeaderTimeout,
	})
}

func normalizedDependencies(deps Dependencies) Dependencies {
	return Dependencies{
		Egress: deps.Egress, Log: deps.Log, Cache: deps.Cache, Guardrails: guardrailRunner(deps),
		CodeLedger: deps.CodeLedger, CodeAuthorizer: deps.CodeAuthorizer, CodeResearch: deps.CodeResearch, ResearchAuth: deps.ResearchAuth,
		Runtime: deps.Runtime, Sink: configuredSink(deps.Sink, deps.Log), Admission: deps.Admission, Debits: deps.Debits, WorkflowRuns: deps.WorkflowRuns, WorkflowTools: deps.WorkflowTools,
		Traces: deps.Traces, Media: deps.Media, Staging: deps.Staging, Uploads: deps.Uploads, Capture: deps.Capture, Business: deps.Business, Resources: deps.Resources, Batches: deps.Batches, Resolver: deps.Resolver,
	}
}

func configuredSink(sink events.Sink, log *slog.Logger) events.Sink {
	if sink != nil {
		return sink
	}
	return events.LogSink{Log: log}
}

// CatalogAuthority shares the gateway's pinned authority, revocation, scope
// and client-address checks with the management catalog's consumer-key reads.
func (s *Server) CatalogAuthority(r *http.Request) (keyauthority.Authority, error) {
	authority, failure := s.gate.Authenticate(r, "models_read")
	if failure != nil {
		return keyauthority.Authority{}, access.Fail(failure.Status, failure.Code, failure.Message)
	}
	return authority, nil
}
