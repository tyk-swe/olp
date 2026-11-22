package codeforward

import (
	"log/slog"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

// Runtime is the published release and credential source code mode reads.
type Runtime interface {
	Release() *release.Release
	RoutingInputs() *routinginputs.Inputs
	release.Credentials
	CredentialRefused(credentialID string, generation int64)
}

type Services struct {
	Ledger       Ledger
	Authorizer   Authorizer
	Research     ResearchLedger
	ResearchAuth ResearchAuthorizer
}

// Dependencies are the collaborators code mode forwards through.
type Dependencies struct {
	Services
	Runtime       Runtime
	Sink          events.Sink
	Admission     *quota.Admission
	Gate          *ingress.Gate
	Egress        *egress.Policy
	Auth          *connectors.Auth
	Log           *slog.Logger
	Now           func() time.Time
	MaxBodyBytes  int64
	HeaderTimeout time.Duration
}

// Forwarder serves the code mode surface under /code/{slug}.
type Forwarder struct {
	ledger        Ledger
	authorizer    Authorizer
	research      ResearchLedger
	researchAuth  ResearchAuthorizer
	runtime       Runtime
	sink          events.Sink
	admission     *quota.Admission
	gate          *ingress.Gate
	egress        *egress.Policy
	auth          *connectors.Auth
	log           *slog.Logger
	now           func() time.Time
	maxBody       int64
	headerTimeout time.Duration
}

func resolveServices(services Services) Services {
	research := services.Research
	if research == nil {
		research, _ = services.Ledger.(ResearchLedger)
	}
	researchAuth := services.ResearchAuth
	if researchAuth == nil {
		researchAuth, _ = services.Authorizer.(ResearchAuthorizer)
	}
	return Services{
		Ledger:       services.Ledger,
		Authorizer:   services.Authorizer,
		Research:     research,
		ResearchAuth: researchAuth,
	}
}

func New(deps Dependencies) *Forwarder {
	services := resolveServices(deps.Services)
	return &Forwarder{
		ledger: services.Ledger, authorizer: services.Authorizer, research: services.Research, researchAuth: services.ResearchAuth,
		runtime: deps.Runtime, sink: deps.Sink, admission: deps.Admission, gate: deps.Gate, egress: deps.Egress, auth: deps.Auth,
		log: deps.Log, now: deps.Now, maxBody: deps.MaxBodyBytes, headerTimeout: deps.HeaderTimeout,
	}
}
