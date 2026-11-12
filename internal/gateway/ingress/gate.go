package ingress

import (
	"context"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/tyk-swe/olp/internal/admission"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/gateway/fault"
)

// ReauthInterval is how often a long-lived session rechecks its key.
const ReauthInterval = 5 * time.Second

type Gate struct {
	auth    consumerauth.Authenticator
	pool    *admission.Pool
	trusted []netip.Prefix
	maxBody int64
	now     func() time.Time
}

func NewGate(auth consumerauth.Authenticator, pool *admission.Pool, trusted []netip.Prefix, maxBody int64, now func() time.Time) *Gate {
	return &Gate{auth: auth, pool: pool, trusted: trusted, maxBody: maxBody, now: now}
}

func (g *Gate) Pool() *admission.Pool { return g.pool }

func SameOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host == r.Host && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
}

// Admit takes an inference slot. When the public admission middleware already
// charged this request the context carries its permit and no second slot is
// taken; a permit the middleware only queued waits here in the normal class.
// Otherwise the server's own pool bounds the call, so direct handler use (and
// any composition that skips the middleware) stays bounded.
func (g *Gate) Admit(ctx context.Context) bool {
	return g.acquirePermit(ctx) != nil
}

func (g *Gate) acquirePermit(ctx context.Context) *admission.Permit {
	if permit := admission.PermitFromContext(ctx); permit != nil {
		if permit.Await(ctx, admission.Normal, time.Time{}) {
			return permit
		}
		return nil
	}
	return g.pool.AcquirePermit()
}

// AdmitLater is admit for requests that resolve their key and priority
// first: a queued permit the middleware only reserved waits for AwaitAdmission.
// Otherwise the server's own pool bounds the call.
func (g *Gate) AdmitLater(ctx context.Context) bool {
	if admission.PermitFromContext(ctx) != nil {
		return true
	}
	return g.pool.AcquirePermit() != nil
}

// AwaitAdmission waits for the slot a queued request reserved, in its
// priority class and never past ctx's deadline, then answers the same 503 a
// full pool does. A request admitted outright returns at once.
func (g *Gate) AwaitAdmission(ctx context.Context, priority string) *fault.Error {
	permit := admission.PermitFromContext(ctx)
	if !permit.Queued() {
		return nil
	}
	deadline, _ := ctx.Deadline()
	if permit.Await(ctx, admission.ClassOf(priority), deadline) {
		return nil
	}
	return fault.Overloaded
}

// Release returns the slot admit took, whether that was the outer permit or
// the local pool. Release is idempotent on the permit: media handlers hand
// their slot back once uploads finish staging, before the upstream call.
func (g *Gate) Release(ctx context.Context) {
	if permit := admission.PermitFromContext(ctx); permit != nil {
		permit.Release()
		return
	}
	g.pool.Release()
}
