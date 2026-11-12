package codeforward

import (
	"log/slog"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
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

// Dependencies are the collaborators code mode forwards through.
type Dependencies struct {
	Ledger        Ledger
	Authorizer    Authorizer
	Research      ResearchLedger
	ResearchAuth  ResearchAuthorizer
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

func New(deps Dependencies) *Forwarder {
	return &Forwarder{
		ledger: deps.Ledger, authorizer: deps.Authorizer, research: deps.Research, researchAuth: deps.ResearchAuth,
		runtime: deps.Runtime, sink: deps.Sink, admission: deps.Admission, gate: deps.Gate, egress: deps.Egress, auth: deps.Auth,
		log: deps.Log, now: deps.Now, maxBody: deps.MaxBodyBytes, headerTimeout: deps.HeaderTimeout,
	}
}
