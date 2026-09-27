package access

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// Handler serves a management request whose caller the route has already
// authenticated and admitted.
type Handler func(*http.Request, Principal) (Reply, error)

// StreamHandler is a Handler that writes its own response.
type StreamHandler func(http.ResponseWriter, *http.Request, Principal) error

// PublicHandler serves a management request that authenticates no principal.
type PublicHandler func(*http.Request) (Reply, error)

type routeOptions struct {
	maxBody int64
	timeout time.Duration
}

// RouteOption adjusts a route's request bounds.
type RouteOption func(*routeOptions)

// MaxBody raises the request body limit for operations whose documented
// payloads exceed the default 64 KiB.
func MaxBody(n int64) RouteOption { return func(o *routeOptions) { o.maxBody = n } }

// Deadline extends the request deadline for operations that legitimately
// outlive the default 15 seconds.
func Deadline(d time.Duration) RouteOption { return func(o *routeOptions) { o.timeout = d } }

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
	mux.HandleFunc(pattern, s.serve(o.maxBody, o.timeout, func(r *http.Request) (Reply, error) {
		r, p, err := s.admitRoute(r, req)
		if err != nil {
			return Reply{}, err
		}
		return h(r, p)
	}))
}

// Stream mounts a secured streaming handler, admitted like Route.
func (s *Server) Stream(mux *http.ServeMux, pattern string, h StreamHandler, opts ...RouteOption) {
	req := declared(pattern, false)
	o := options(opts)
	mux.HandleFunc(pattern, s.serveStream(o.maxBody, o.timeout, func(w http.ResponseWriter, r *http.Request) error {
		r, p, err := s.admitRoute(r, req)
		if err != nil {
			return err
		}
		return h(w, r, p)
	}))
}

// Public mounts h at a pattern the contract declares public.
func (s *Server) Public(mux *http.ServeMux, pattern string, h PublicHandler, opts ...RouteOption) {
	declared(pattern, true)
	o := options(opts)
	mux.HandleFunc(pattern, s.serve(o.maxBody, o.timeout, h))
}

// admitRoute authenticates the caller and checks the route requirement, carrying
// the requirement in the request so a transaction can check it again.
func (s *Server) admitRoute(r *http.Request, req Requirement) (*http.Request, Principal, error) {
	r = r.WithContext(context.WithValue(r.Context(), requirementKey{}, req))
	p, err := s.Authenticate(r, s.Pool)
	if err != nil {
		return r, p, err
	}
	return r, p, req.Admits(p)
}

// Reauthorize resolves the caller again inside tx, after Begin has taken the
// installation lock, and checks the route requirement again, so a mutation
// never commits under authority revoked since the request was admitted.
func (s *Server) Reauthorize(r *http.Request, tx pgx.Tx) (Principal, error) {
	req, ok := r.Context().Value(requirementKey{}).(Requirement)
	if !ok {
		return Principal{}, fmt.Errorf("%s was not admitted by a secured route", r.Pattern)
	}
	p, err := s.Authenticate(r, tx)
	if err != nil {
		return p, err
	}
	return p, req.Admits(p)
}
