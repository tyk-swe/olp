package gateway

import (
	"context"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

// CodeLedger is the durable authority and admission boundary used before each
// coding generation, including each generation on an upgraded connection.
type CodeLedger interface {
	Models(context.Context, codemode.Route, string) (codeclient.Offers, error)
	Admit(context.Context, resources.CodeAdmission) (resources.CodePermit, error)
	BindConnection(context.Context, codemode.Route, string, codemode.Identity, string, []string) (resources.CodePermit, error)
	MarkDispatched(context.Context, string) error
	Settle(context.Context, string, codemode.Usage) error
	Abort(context.Context, string) error
	ObserveHealth(context.Context, string, codemode.HealthObservation) error
	ObserveAllowance(context.Context, string, codemode.Allowance) error
	ObserveReference(context.Context, string, string) error
	ObserveOutcome(context.Context, string, codemode.Outcome) error
	RecordRefusal(context.Context, codemode.Route, string, string, string) error
}

// CodeAuthorizer returns the upstream authorization of one request: its
// adapter and protocol, on an account whose provider connection the route
// revision froze.
type CodeAuthorizer interface {
	AuthorizeCode(context.Context, routing.Configuration, codemode.Account, codemode.Dispatch) (codemode.Authorization, error)
}

// ResearchAuthorizer resolves the selected subscription's grant without a model.
type ResearchAuthorizer interface {
	AuthorizeResearch(context.Context, routing.Configuration, codemode.Account, string) (codemode.Authorization, error)
}

// ResearchLedger checks live authority and retains bounded opaque session state.
type ResearchLedger interface {
	AuthorizeResearch(context.Context, codemode.Route, string, string) (access.Authority, codemode.Account, error)
	ResearchSession(context.Context, string, codemode.ResearchSessionBinding) (string, error)
	CreateResearchSession(context.Context, codemode.ResearchSessionBinding, string) (string, error)
	DeleteResearchSession(context.Context, string, string) error
}
