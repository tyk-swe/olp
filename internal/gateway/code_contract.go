package gateway

import (
	"context"
	"net/http"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
)

// CodeLedger is the durable authority and admission boundary used before each
// coding generation, including each generation on an upgraded connection.
type CodeLedger interface {
	Admit(context.Context, resources.CodeAdmission) (resources.CodePermit, error)
	BindConnection(context.Context, codemode.Route, string, codemode.Identity) (resources.CodePermit, error)
	MarkDispatched(context.Context, string) error
	Settle(context.Context, string, codemode.Usage) error
	Abort(context.Context, string) error
	ObserveHealth(context.Context, string, string) error
	ObserveAllowance(context.Context, string, codemode.Allowance) error
	ObserveReference(context.Context, string, string) error
	RecordRefusal(context.Context, codemode.Route, string, string) error
}

// CodeTransport owns raw HTTP/SSE and WebSocket forwarding. Composition mounts
// it only when an implementation is supplied; no inference is simulated here.
type CodeTransport interface{ RegisterCode(*http.ServeMux, *Server) }

type CodeAuthorization = codemode.Authorization

type CodeAuthorizer interface {
	AuthorizeCode(context.Context, runtime.Configuration, codemode.Account) (CodeAuthorization, error)
}
