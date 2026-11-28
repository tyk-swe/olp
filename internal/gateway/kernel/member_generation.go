package kernel

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type Member struct {
	UserID      string
	Preferences *routing.Preferences
	Document    []byte
	Admit       func(*routing.Route) *fault.Error
}

type MemberGeneration struct {
	server *Server
	call   *Call
	admit  func(*routing.Route) *fault.Error
}

type MemberTrail struct {
	ID        string
	Route     string
	Elapsed   time.Duration
	Facts     []events.AttemptFact
	Decisions []routing.Decision
	Attempts  []routing.Attempt
}

func (s *Server) MemberGeneration(r *http.Request, member Member) *MemberGeneration {
	x := newCall(
		request{id: ids.New(), minted: true, clientIP: consumerauth.ClientIP(r, s.Config.TrustedProxies), startedAt: s.Now(), release: s.Deps.Runtime.Release(), trace: telemetry.RequestFromContext(r.Context())},
		noFamily,
		callIdentity{actor: "playground", userID: member.UserID},
		callLineage{origin: usage.OriginPlayground},
		callWork{preferences: member.Preferences, routeLeg: routeLeg{affinity: []byte(member.UserID)}},
	)
	if member.Document != nil {
		x.request.body = &ingress.MeasuredBody{Decoded: int64(len(member.Document))}
	}
	return &MemberGeneration{server: s, call: x, admit: member.Admit}
}

func (g *MemberGeneration) ExpandPrompt(family wire.Format, doc oif.Document, model string, bindings []oif.Change) (ParsedRequest, *fault.Error) {
	return g.server.parsePromptDocument(g.call, family, doc, model, g.admit, bindings)
}

func (g *MemberGeneration) Run(ctx context.Context, parsed ParsedRequest, emit func([]byte) error) *Outcome {
	s, x := g.server, g.call
	x.generation.parsed, x.family, x.prompt = parsed.Request, parsed.Request.Family, parsed.Reference
	if x.request.body != nil {
		x.request.body.Decoded = max(x.request.body.Decoded, parsed.Decoded)
	}
	if e := s.prepare(ctx, x, g.admit); e != nil {
		return g.refuse(e)
	}
	x.estimate = requestEstimate(x)
	if !s.Gate.Admit(ctx) {
		return g.refuse(fault.Overloaded)
	}
	defer s.Gate.Release(ctx)
	defer s.SettleAdmission(ctx, x)
	if e := s.reserveSystemBudgets(ctx, x); e != nil {
		return g.refuse(e)
	}
	if emit != nil {
		x.generation.emit = emit
		out := s.execute(ctx, x)
		status := http.StatusOK
		if out.Err != nil && out.Err.Status > 0 {
			status = out.Err.Status
		}
		s.Finish(x, out, status)
		return out
	}
	out := s.execute(ctx, x)
	if out.Err != nil {
		s.Finish(x, out, out.Err.Status)
		return out
	}
	s.Finish(x, out, http.StatusOK)
	return out
}

func (g *MemberGeneration) refuse(e *fault.Error) *Outcome {
	g.call.Failure = e
	g.server.Finish(g.call, nil, e.Status)
	return &Outcome{Err: e}
}

func (g *MemberGeneration) Trail() MemberTrail {
	x := g.call
	return MemberTrail{
		ID: x.request.id, Route: x.named().Slug, Elapsed: g.server.Now().Sub(x.request.startedAt),
		Facts: x.facts, Decisions: x.decisions, Attempts: x.attempts,
	}
}
