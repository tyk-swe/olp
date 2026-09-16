package gateway

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/runtime"
)

// Config bounds the inference surface.
type Config struct {
	MaxInFlight      int
	MaxBodyBytes     int64
	MaxResponseBytes int64
	MaxEventBytes    int64
	TrustedProxies   []netip.Prefix
}

// Runtime is the pinned authority and release source. *runtime.Manager
// implements it; fixtures and tests supply static releases.
type Runtime interface {
	Release() *runtime.Release
	Authenticate(secret string) (access.Authority, error)
	Revoked(credentialID string) bool
}

// Server serves the native OpenAI surface from pinned runtime releases.
type Server struct {
	Runtime Runtime
	Sink    Sink
	// Admission enforces the budgets shared by every replica. A nil Admission
	// means none were configured: keys and targets that bound nothing are
	// served, and anything that must be metered fails closed.
	Admission *Admission

	log       *slog.Logger
	cfg       Config
	egress    *egress.Policy
	client    *http.Client
	auth      *connectors.Auth
	admission chan struct{}
	health    *healthTracker
	now       func() time.Time
}

// upstreamHeaderTimeout caps the wait for upstream response headers; the
// per-attempt deadline is normally tighter.
const upstreamHeaderTimeout = 5 * time.Minute

func New(rt Runtime, policy *egress.Policy, cfg Config, log *slog.Logger) *Server {
	return &Server{
		Runtime:   rt,
		Sink:      LogSink{Log: log},
		log:       log,
		cfg:       cfg,
		egress:    policy,
		client:    policy.Client(upstreamHeaderTimeout),
		auth:      connectors.NewAuth(policy),
		admission: make(chan struct{}, max(cfg.MaxInFlight, 1)),
		health:    newHealthTracker(time.Now),
		now:       time.Now,
	}
}

// Health exposes windowed attempt statistics for the management API.
func (s *Server) Health() providers.HealthSource { return s.health }

// Register mounts the OpenAI surface on the public mux.
func (s *Server) Register(mux *http.ServeMux) {
	s.registerNative(mux)
	mux.HandleFunc("POST /v1/chat/completions", s.inference(openai.FamilyChat))
	mux.HandleFunc("POST /v1/responses", s.inference(openai.FamilyResponses))
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /v1/models/{model}", s.model)
	mux.HandleFunc("OPTIONS /v1/", s.preflight)
	mux.HandleFunc("/v1/", s.unknown)
}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// request carries per-request identity shared by handlers.
type request struct {
	id string
	// minted records that this gateway chose the request id. A caller may name
	// its own, and nothing stops two callers from naming the same one.
	minted    bool
	clientIP  string
	startedAt time.Time
	release   *runtime.Release
}

// accountingID is the identity durable records are stored under: the request
// id when this gateway minted it, and a fresh one when the caller named it.
func (r request) accountingID() string {
	if r.minted {
		return r.id
	}
	return uuid.Must(uuid.NewV7()).String()
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
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	s.cors(w, r)
	return request{id: id, minted: minted, clientIP: ClientIP(r, s.cfg.TrustedProxies), startedAt: s.now(), release: s.Runtime.Release()}
}

// cors allows browser SDK clients from any origin: the surface authenticates
// with bearer keys only, never cookies, so no credentialed access exists.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") == "" {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Expose-Headers", "X-Request-Id, Retry-After")
}

func (s *Server) preflight(w http.ResponseWriter, r *http.Request) {
	s.cors(w, r)
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, X-Api-Key, X-Goog-Api-Key, X-Goog-Api-Client, Anthropic-Version, Anthropic-Beta, Anthropic-Dangerous-Direct-Browser-Access, Content-Type, X-Request-Id, X-OLP-Routing, OpenAI-Organization, OpenAI-Project, OpenAI-Beta, X-Stainless-Lang, X-Stainless-Package-Version, X-Stainless-OS, X-Stainless-Arch, X-Stainless-Runtime, X-Stainless-Runtime-Version, X-Stainless-Retry-Count, X-Stainless-Timeout, X-Stainless-Helper-Method")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) unknown(w http.ResponseWriter, r *http.Request) {
	s.begin(w, r)
	writeError(w, notFoundError("not_found", "Unknown endpoint "+r.Method+" "+r.URL.Path+"."))
}

// ClientIP returns the address of the calling client, honouring
// X-Forwarded-For only from configured trusted proxies.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	isTrusted := func(a netip.Addr) bool {
		return slices.ContainsFunc(trusted, func(p netip.Prefix) bool { return p.Contains(a.Unmap()) })
	}
	if !isTrusted(addr) {
		return addr.String()
	}
	chain := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(chain) - 1; i >= 0; i-- {
		hop, err := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if err != nil {
			break
		}
		if !isTrusted(hop) {
			return hop.String()
		}
		addr = hop
	}
	return addr.String()
}

func (s *Server) admit() bool {
	select {
	case s.admission <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) release() { <-s.admission }

// authenticate resolves the bearer key against the pinned authority and
// checks the scope the endpoint needs.
func (s *Server) authenticate(r *http.Request, scope string) (access.Authority, *Error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		switch requestSurface(r) {
		case "anthropic":
			if key := r.Header.Get("X-Api-Key"); key != "" {
				header = "Bearer " + key
			}
		case "gemini":
			key := r.Header.Get("X-Goog-Api-Key")
			if key == "" {
				key = r.URL.Query().Get("key")
			}
			if key != "" {
				header = "Bearer " + key
			}
		}
	}
	if len(header) < 7 || !strings.EqualFold(header[:7], "Bearer ") {
		return access.Authority{}, authenticationError("invalid_api_key", "Provide an API key as a bearer token in the Authorization header.")
	}
	token := strings.TrimSpace(header[7:])
	authority, err := s.Runtime.Authenticate(token)
	switch {
	case errors.Is(err, runtime.ErrStaleAuthority):
		return access.Authority{}, serverError(http.StatusServiceUnavailable, "authority_unavailable", "Key authority is unavailable; retry shortly.")
	case err != nil:
		return access.Authority{}, authenticationError("invalid_api_key", "Incorrect API key provided.")
	case authority.RevokedAt != nil:
		return access.Authority{}, authenticationError("invalid_api_key", "This API key has been revoked.")
	case authority.ExpiresAt != nil && !authority.ExpiresAt.After(s.now()):
		return access.Authority{}, authenticationError("invalid_api_key", "This API key has expired.")
	case !slices.Contains(authority.Policy.Scopes, scope):
		return access.Authority{}, permissionError("permission_denied", "This API key does not have the "+scope+" scope.")
	}
	return authority, nil
}

// readBody bounds both the encoded and decoded body before parsing.
func (s *Server) readBody(r *http.Request) ([]byte, *Error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, &Error{Status: http.StatusUnsupportedMediaType, Type: "invalid_request_error", Code: "unsupported_media_type", Message: "Request bodies must be application/json."}
	}
	limit := s.cfg.MaxBodyBytes
	encoded := &io.LimitedReader{R: r.Body, N: limit + 1}
	var reader io.Reader = encoded
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(reader)
		if encoded.N == 0 {
			return nil, bodyTooLarge()
		}
		if err != nil {
			return nil, bodyReadError(err)
		}
		defer gz.Close()
		reader = io.LimitReader(gz, limit+1)
	default:
		return nil, &Error{Status: http.StatusUnsupportedMediaType, Type: "invalid_request_error", Code: "unsupported_content_encoding", Message: "Only identity and gzip request encodings are supported."}
	}
	data, err := io.ReadAll(reader)
	// Check the wire cap even when gzip has decoded a smaller body or reports
	// truncation after reaching the encoded limit.
	if encoded.N == 0 || int64(len(data)) > limit {
		return nil, bodyTooLarge()
	}
	if err != nil {
		return nil, bodyReadError(err)
	}
	return data, nil
}

func bodyTooLarge() *Error {
	return &Error{Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error", Code: "request_too_large", Message: "The request body exceeds the configured limit."}
}

func bodyReadError(err error) *Error {
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return &Error{Status: http.StatusRequestTimeout, Type: "invalid_request_error", Code: "request_timeout", Message: "The request body was not received within the deadline."}
	}
	return invalidRequest("invalid_request", "The request body could not be read.", nil)
}

// routingHeader carries per-request routing preferences. Only the weighted
// strategy exists, and the attempt budget can only be lowered below the
// route's published maximum.
const routingHeader = "X-OLP-Routing"

func routingPreferences(r *http.Request) (*runtime.Preferences, *Error) {
	values := r.Header.Values("X-OLP-Routing")
	if len(values) == 0 {
		return nil, nil
	}
	param := "X-OLP-Routing"
	if len(values) != 1 {
		return nil, invalidRequest("invalid_request", "Provide exactly one X-OLP-Routing header.", &param)
	}
	p, err := runtime.ParsePreferences([]byte(values[0]))
	if err != nil {
		return nil, invalidRequest("invalid_request", err.Error(), &param)
	}
	return p, nil
}
func attemptBudget(r *http.Request, route *runtime.Route) (int, *Error) {
	p, err := routingPreferences(r)
	if err != nil {
		return 0, err
	}
	if p != nil && p.MaxAttempts != nil && *p.MaxAttempts > route.MaxAttempts {
		return 0, invalidRequest("invalid_request", "max_attempts cannot increase the published route budget.", nil)
	}
	return p.Budget(route.MaxAttempts), nil
}

func requestError(err error) *Error {
	var re *openai.RequestError
	if errors.As(err, &re) {
		var param *string
		if re.Param != "" {
			param = &re.Param
		}
		return invalidRequest(re.Code, re.Message, param)
	}
	return invalidRequest("invalid_request", err.Error(), nil)
}

func selectionError(err error, model string) *Error {
	var se *runtime.SelectionError
	if errors.As(err, &se) {
		switch se.Code {
		case runtime.RouteNotFound:
			return modelNotFound(model)
		case runtime.OperationNotSupported:
			return invalidRequest("invalid_request", "The model `"+model+"` does not allow this operation.", nil)
		}
	}
	return serverError(http.StatusServiceUnavailable, "upstream_unavailable", "No provider is currently eligible to serve `"+model+"`.")
}

const requestBodyTimeout = 15 * time.Second

func (s *Server) inference(family openai.Family) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeError := func(w http.ResponseWriter, e *Error) { writeSurfaceError(w, e, family.Surface()) }
		x := &execution{request: s.begin(w, r), family: family, actor: "api_key"}
		status := http.StatusInternalServerError
		var out *outcome
		defer func() {
			s.finish(x, out, status)
			// The budgets this request reserved are settled even when the
			// client is gone: a concurrency slot nobody releases is a slot
			// every replica keeps counting.
			settleKey(r.Context(), x.lease, x.dispatched, x.settledTokens(), s.log)
		}()
		// A context timeout alone cannot interrupt a blocked socket read.
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(requestBodyTimeout)); err != nil {
			e := serverError(http.StatusInternalServerError, "internal_error", "The request could not be read.")
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		if !s.admit() {
			x.failure, status = overloaded, overloaded.Status
			writeError(w, overloaded)
			return
		}
		defer s.release()
		authority, e := s.authenticate(r, "inference")
		if e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		x.keyID, x.affinity = authority.ID, []byte(authority.ID)
		body, e := s.readBody(r)
		if e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		// Keep the deadline on failed reads so HTTP/1 body draining stays
		// bounded; successful uploads must not limit the inference stream.
		rc.SetReadDeadline(time.Time{})
		parsed, err := protocols.Parse(family, body, r.PathValue("model"))
		if err != nil {
			e = requestError(err)
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		x.parsed = parsed
		if x.preferences, e = routingPreferences(r); e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		if e := s.prepare(x, func(slug string) bool { return authority.Allows("inference", slug, s.now()) }); e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}

		// Admission happens once the request is understood and before any
		// provider is called, so a rejected request costs an upstream nothing.
		// The lease outlives the route deadline it is sized against: it is the
		// backstop for a replica that dies mid-request, not the deadline.
		x.estimate = requestEstimate(x)
		overall := time.Duration(x.route.OverallTimeout) * time.Millisecond
		// Admission is part of the same deadline as execution; starting a new
		// full deadline afterwards could outlive the concurrency reservation.
		ctx, cancel := context.WithTimeout(r.Context(), overall)
		defer cancel()
		reservationEstimate := keyReservationEstimate(x.estimate, s.dispatchableAttempts(x))
		if x.lease, e = s.Admission.reserveKey(ctx, authority, reservationEstimate, overall); e != nil {
			x.failure, status = e, e.Status
			writeError(w, e)
			return
		}
		if parsed.Stream {
			sw := &streamWriter{w: w, family: family}
			x.emit = func(frame []byte) error {
				err := sw.emit(frame)
				if err == nil {
					x.delivered(s.now())
				}
				return err
			}
			out = s.execute(ctx, x)
			status = sw.finish(out)
			return
		}
		out = s.execute(ctx, x)
		if out.err != nil {
			status = out.err.Status
			if out.err.Status > 0 {
				writeError(w, out.err)
			}
			return
		}
		if err := rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout)); err != nil {
			out.err = serverError(http.StatusInternalServerError, "internal_error", "The response could not be written.")
			status = out.err.Status
			writeError(w, out.err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		status = http.StatusOK
		w.WriteHeader(http.StatusOK)
		out.committed = true
		x.facts[len(x.facts)-1].Committed = true
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
		}
	}
}

// prepare resolves the route, checks the caller's route permission, and
// ranks the eligible attempts against the pinned snapshot.
func (s *Server) prepare(x *execution, permitted func(slug string) bool) *Error {
	x.mode = "unary"
	if x.parsed.Stream {
		x.mode = "streaming"
	}
	snapshot := x.request.release.Snapshot
	route, ok := snapshot.Routes[x.parsed.Route]
	if !ok {
		return modelNotFound(x.parsed.Route)
	}
	x.route = &route
	if !permitted(route.Slug) {
		return permissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`.")
	}
	var semantic error
	plan, err := runtime.PlanRequest(snapshot, route.Slug, x.family.Operation(), x.family.Surface(), x.mode, x.affinity, runtime.SelectionOptions{
		KeyID: x.keyID, Preferences: x.preferences, Parameters: protocols.ParameterNames(x.parsed), Inputs: s.routingInputs(), Now: s.now(), CheckSlots: true, CredentialRevoked: s.Runtime.Revoked,
		Accept: func(p runtime.Provider, t runtime.Target) error {
			cfg := p.Connector()
			if !connectors.Supports(p.Kind, p.VendorID, x.family.Operation(), x.family.Surface(), x.mode) {
				return errors.New("connector capability unavailable")
			}
			_, _, e := protocols.Encode(x.parsed, p.Kind, p.VendorID, cfg.Model(t.ProviderModel), p.ParameterDefaults)
			if e != nil {
				semantic = e
			}
			return e
		}})
	if err != nil {
		var se *runtime.SelectionError
		if errors.As(err, &se) && se.Code != runtime.NoEligibleTargets && se.Code != "attempt_budget_increase_forbidden" {
			return selectionError(err, route.Slug)
		}
		return requestError(err)
	}
	x.decisions = plan.Decisions
	x.policy = plan.Policy
	x.attempts = plan.Attempts
	x.budget = plan.Budget
	if len(plan.Attempts) == 0 {
		if semantic != nil {
			return requestError(semantic)
		}
		return selectionError(&runtime.SelectionError{Code: runtime.NoEligibleTargets}, route.Slug)
	}
	return nil
}

// modelObject renders a route as an OpenAI model object.
func modelObject(route *runtime.Route) map[string]any {
	return map[string]any{
		"id":       route.Slug,
		"object":   "model",
		"created":  route.PublishedAt.Unix(),
		"owned_by": "openllmproxy",
	}
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	req := s.begin(w, r)
	authority, e := s.authenticate(r, "models_read")
	if e != nil {
		writeError(w, e)
		return
	}
	now := s.now()
	data := []map[string]any{}
	for _, slug := range slices.Sorted(mapsKeys(req.release.Snapshot.Routes)) {
		if authority.Allows("models_read", slug, now) {
			route := req.release.Snapshot.Routes[slug]
			data = append(data, modelObject(&route))
		}
	}
	writeJSON(w, map[string]any{"object": "list", "data": data})
}

func (s *Server) model(w http.ResponseWriter, r *http.Request) {
	req := s.begin(w, r)
	authority, e := s.authenticate(r, "models_read")
	if e != nil {
		writeError(w, e)
		return
	}
	slug := r.PathValue("model")
	route, ok := req.release.Snapshot.Routes[slug]
	if !ok || !authority.Allows("models_read", slug, s.now()) {
		writeError(w, modelNotFound(slug))
		return
	}
	writeJSON(w, modelObject(&route))
}

func mapsKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// streamWriter commits the client response on the first upstream frame and
// applies a per-frame write deadline so slow readers cannot pin upstream
// work forever.
type streamWriter struct {
	w         http.ResponseWriter
	family    openai.Family
	committed bool
}

// responseWriteTimeout bounds unary delivery and each streamed frame.
const responseWriteTimeout = 30 * time.Second

// errClientWrite marks a frame the client could not or would not read; the
// executor classifies it as client cancellation rather than provider failure.
var errClientWrite = errors.New("client write failed")

func (sw *streamWriter) emit(frame []byte) error {
	rc := http.NewResponseController(sw.w)
	if !sw.committed {
		h := sw.w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("X-Accel-Buffering", "no")
		sw.w.WriteHeader(http.StatusOK)
		sw.committed = true
	}
	rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	if _, err := sw.w.Write(frame); err != nil {
		return fmt.Errorf("%w: %v", errClientWrite, err)
	}
	if err := rc.Flush(); err != nil {
		return fmt.Errorf("%w: %v", errClientWrite, err)
	}
	return nil
}

// finish completes the client response and returns the status it carried.
func (sw *streamWriter) finish(out *outcome) int {
	if out.err == nil {
		return http.StatusOK
	}
	if out.cancelled {
		return 0 // the client is gone; nothing more can be delivered
	}
	if !sw.committed {
		if out.err.Status > 0 {
			writeSurfaceError(sw.w, out.err, sw.family.Surface())
		}
		return out.err.Status
	}
	// The response is committed: signal the failure in-band the way the
	// official SDKs detect it, then end the stream without a completion
	// marker so the client cannot mistake it for success.
	frame := "data: " + string(out.err.surfaceBody(sw.family.Surface())) + "\n\n"
	if sw.family == openai.FamilyResponses || sw.family.Surface() == "anthropic" {
		frame = "event: error\n" + frame
	}
	sw.emit([]byte(frame))
	return http.StatusOK
}
