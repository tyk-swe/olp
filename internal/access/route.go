package access

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/keyauthority"
)

// Handler serves a management request whose caller the route has already
// authenticated and admitted.
type Handler func(*http.Request, authz.Principal) (httpkit.Reply, error)

// StreamHandler is a Handler that writes its own response.
type StreamHandler func(http.ResponseWriter, *http.Request, authz.Principal) error

// PublicHandler serves a management request that authenticates no principal.
type PublicHandler func(*http.Request) (httpkit.Reply, error)

type routeOptions struct {
	maxBody int64
	timeout time.Duration
	keyAuth func(*http.Request) (keyauthority.Authority, error)
	browser browserPolicy
}

// browserPolicy says which browser requests a route accepts when the caller
// presents no management bearer token.
type browserPolicy int

const (
	sameOrigin browserPolicy = iota
	crossSiteNavigation
	anyOrigin
)

// RouteOption adjusts a route's request bounds.
type RouteOption func(*routeOptions)

// MaxBody raises the request body limit for operations whose documented
// payloads exceed the default 64 KiB.
func MaxBody(n int64) RouteOption { return func(o *routeOptions) { o.maxBody = n } }

// Deadline extends the request deadline for operations that legitimately
// outlive the default 15 seconds.
func Deadline(d time.Duration) RouteOption { return func(o *routeOptions) { o.timeout = d } }

// InferenceKeyAuth supplies the pinned gateway authority for a contract's
// read-only apiKeyBearer alternative. Other routes cannot enable it.
func InferenceKeyAuth(authenticate func(*http.Request) (keyauthority.Authority, error)) RouteOption {
	return func(o *routeOptions) { o.keyAuth = authenticate }
}

// CrossSiteNavigation admits a navigation that another site started, for the
// endpoints an identity provider redirects the browser back to.
func CrossSiteNavigation() RouteOption {
	return func(o *routeOptions) { o.browser = crossSiteNavigation }
}

// AnyOrigin skips the cookie and origin checks, for endpoints whose handler
// trusts something other than the console session: a signed identity provider
// response or a bearer token.
func AnyOrigin() RouteOption { return func(o *routeOptions) { o.browser = anyOrigin } }

func options(opts []RouteOption) routeOptions {
	o := routeOptions{maxBody: 65536, timeout: 15 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

type requirementKey struct{}

// declared returns the contract requirement for a secured pattern. A route the
// contract does not declare, or declares as public, cannot be mounted as
// secured: registration panics, so every process and test that builds the
// management API finds the mistake.
func declared(pattern string, public bool) Requirement {
	req, ok := requirementFor(pattern)
	if !ok {
		panic(fmt.Sprintf("management route %s is not declared in the contract", pattern))
	}
	if req.Public != public {
		panic(fmt.Sprintf("management route %s is registered with the wrong visibility", pattern))
	}
	return req
}

// Route mounts h at pattern. Before h runs, the caller is authenticated and
// must satisfy the requirement the contract declares for pattern; h receives
// the admitted Principal and may demand more, never less.
func (s *Server) Route(mux *http.ServeMux, pattern string, h Handler, opts ...RouteOption) {
	req := declared(pattern, false)
	o := options(opts)
	keyAlternative := false
	for _, alternative := range req.Alternatives {
		keyAlternative = keyAlternative || alternative.Kind == "key"
	}
	if keyAlternative != (o.keyAuth != nil) || keyAlternative && !strings.HasPrefix(pattern, "GET ") {
		panic(fmt.Sprintf("management route %s has an invalid inference-key authenticator", pattern))
	}
	mux.HandleFunc(pattern, s.serve(o, func(r *http.Request) (httpkit.Reply, error) {
		if o.keyAuth != nil && presentedInferenceKey(r) {
			authority, err := o.keyAuth(r)
			if err != nil {
				return httpkit.Reply{}, err
			}
			p := authz.Principal{Kind: "key", KeyAuthority: &authority}
			if req.Admits(p) != nil {
				return httpkit.Reply{}, httpkit.Forbidden()
			}
			return h(r, p)
		}
		r, p, err := s.admitRoute(r, req)
		if err != nil {
			return httpkit.Reply{}, err
		}
		return h(r, p)
	}))
}

func presentedInferenceKey(r *http.Request) bool {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, "olp_") {
		return true
	}
	parts := strings.Split(token, ".")
	return len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] != ""
}

// Stream mounts a secured streaming handler, admitted like Route.
func (s *Server) Stream(mux *http.ServeMux, pattern string, h StreamHandler, opts ...RouteOption) {
	req := declared(pattern, false)
	o := options(opts)
	mux.HandleFunc(pattern, s.serveStream(o, func(w http.ResponseWriter, r *http.Request) error {
		r, p, err := s.admitRoute(r, req)
		if err != nil {
			return err
		}
		return h(w, r, p)
	}))
}

// Admit returns the admission step of a secured route for a handler that
// writes its own response format. The step applies the transport guards and
// authenticates the caller against the requirement the contract declares for
// pattern. The caller must run the returned cancel function.
func (s *Server) Admit(pattern string, opts ...RouteOption) func(http.ResponseWriter, *http.Request) (*http.Request, authz.Principal, context.CancelFunc, error) {
	req := declared(pattern, false)
	o := options(opts)
	return func(w http.ResponseWriter, r *http.Request) (*http.Request, authz.Principal, context.CancelFunc, error) {
		guarded, cancel, err := s.guard(w, r, o)
		if err != nil {
			return guarded, authz.Principal{}, cancel, err
		}
		admitted, p, err := s.admitRoute(guarded, req)
		return admitted, p, cancel, err
	}
}

// Public mounts h at a pattern the contract declares public.
func (s *Server) Public(mux *http.ServeMux, pattern string, h PublicHandler, opts ...RouteOption) {
	declared(pattern, true)
	o := options(opts)
	mux.HandleFunc(pattern, s.serve(o, h))
}

// admitRoute authenticates the caller and checks the route requirement, carrying
// the requirement in the request so a transaction can check it again.
func (s *Server) admitRoute(r *http.Request, req Requirement) (*http.Request, authz.Principal, error) {
	r = r.WithContext(context.WithValue(r.Context(), requirementKey{}, req))
	p, err := s.Authenticate(r, s.Pool)
	if err != nil {
		return r, p, err
	}
	return r, p, req.Admits(p)
}

// reauthorize resolves the caller again inside tx, after begin has taken the
// installation lock, and checks the route requirement again, so a mutation
// never commits under authority revoked since the request was admitted.
func (s *Server) reauthorize(r *http.Request, tx pgx.Tx) (authz.Principal, error) {
	req, ok := r.Context().Value(requirementKey{}).(Requirement)
	if !ok {
		return authz.Principal{}, fmt.Errorf("%s was not admitted by a secured route", r.Pattern)
	}
	p, err := s.Authenticate(r, tx)
	if err != nil {
		return p, err
	}
	return p, req.Admits(p)
}

func (s *Server) beginMutation(r *http.Request) (pgx.Tx, authz.Principal, error) {
	tx, err := s.begin(r)
	if err != nil {
		return nil, authz.Principal{}, err
	}
	principal, err := s.reauthorize(r, tx)
	if err != nil {
		tx.Rollback(context.WithoutCancel(r.Context()))
		return nil, authz.Principal{}, err
	}
	return tx, principal, nil
}
