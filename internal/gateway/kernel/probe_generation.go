package kernel

import (
	"cmp"
	"context"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type ProbeAttempt struct {
	Unplanned *fault.Error
	Reached   bool
	Failed    bool
	Class     string
	Latency   time.Duration
}

func (s *Server) ProbeGeneration(ctx context.Context, release *release.Release, route routing.Route, parsed *wire.Request) ProbeAttempt {
	x := newCall(s.mintedRequest("", release), parsed.Family, callIdentity{actor: "system"},
		callLineage{origin: usage.OriginProbe, priority: routing.PriorityLow},
		callWork{
			mode:               "unary",
			historicalSnapshot: release.Snapshot().WithRoute(route),
			routeLeg:           routeLeg{Route: &route},
			generation:         generationCall{parsed: parsed},
		})
	x.bind(canonicalPlanner, nil, true)
	defer s.SettleAdmission(ctx, x)
	if e := s.planCanonical(ctx, x); e != nil {
		return ProbeAttempt{Unplanned: e}
	}
	x.budget, x.allowance = 1, 1
	if e := s.reserveSystemBudgets(ctx, x); e != nil {
		s.Finish(x, &Outcome{Err: e}, e.Status)
		return ProbeAttempt{}
	}
	out := s.execute(ctx, x)
	s.Finish(x, out, cmp.Or(errorStatus(out.Err), 200))
	if !x.dispatched || len(x.facts) == 0 {
		return ProbeAttempt{}
	}
	fact := x.facts[len(x.facts)-1]
	return ProbeAttempt{Reached: true, Failed: out.Err != nil, Class: fact.Class, Latency: fact.Duration}
}

func errorStatus(e *fault.Error) int {
	if e == nil {
		return 0
	}
	return e.Status
}
