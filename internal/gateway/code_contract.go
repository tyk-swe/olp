package gateway

import (
	"context"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
)

// CodeLedger is the durable authority and admission boundary used before each
// coding generation, including each generation on an upgraded connection.
type CodeLedger interface {
	Admit(context.Context, resources.CodeAdmission) (resources.CodePermit, error)
	BindConnection(context.Context, codemode.Route, string, codemode.Identity, string) (resources.CodePermit, error)
	MarkDispatched(context.Context, string) error
	Settle(context.Context, string, codemode.Usage) error
	Abort(context.Context, string) error
	ObserveHealth(context.Context, string, string) error
	ObserveAllowance(context.Context, string, codemode.Allowance) error
	ObserveReference(context.Context, string, string) error
	ObserveOutcome(context.Context, string, codemode.Outcome) error
	RecordRefusal(context.Context, codemode.Route, string, string) error
}

type CodeAuthorizer interface {
	AuthorizeCode(context.Context, runtime.Configuration, codemode.Account) (codemode.Authorization, error)
}
