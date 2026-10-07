package gateway

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	// probeTick is how often the probe worker looks for targets due a probe.
	probeTick = 15 * time.Second
	// probeDeadline bounds one probe, below any route deadline.
	probeDeadline = 30 * time.Second
)

// probeTarget is one provider model a route serves through a connection that
// opted into probing. Each is probed once per interval across the fleet, on
// the first route that serves it.
type probeTarget struct {
	route     runtime.Route
	target    runtime.Target
	provider  string
	operation string
	interval  time.Duration
}

func (t probeTarget) key() string { return t.provider + "/" + t.target.ProviderModel }

// probeRequests are the certification requests a probe sends, by operation:
// the smallest each operation answers, addressed to the probed route.
var probeRequests = map[string]struct {
	family openai.Family
	body   string
}{
	operationGeneration: {openai.FamilyChat, `{"model":%q,"messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":16}`},
	"embeddings":        {openai.FamilyEmbeddings, `{"model":%q,"input":"embedding probe"}`},
	"rerank":            {openai.FamilyRerank, `{"model":%q,"query":"rerank probe","documents":["first document","second document"]}`},
}

// RunHealthProbes probes every target whose connection enables probing, each
// once per its interval across the fleet, until ctx ends. Every pass is
// reported to checkpoint. Probes need shared state to divide work and share
// results, so without it this returns at once.
func (s *Server) RunHealthProbes(ctx context.Context, checkpoint func(ctx context.Context, probed int, err error)) {
	if !s.Admission.ready() {
		return
	}
	ticker := time.NewTicker(probeTick)
	defer ticker.Stop()
	for {
		probed, err := s.probeDue(ctx)
		if ctx.Err() != nil {
			return
		}
		checkpoint(ctx, probed, err)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// probeDue probes the targets whose fleet-wide claim this replica wins.
func (s *Server) probeDue(ctx context.Context) (int, error) {
	release := s.Runtime.Release()
	if release == nil || release.Snapshot == nil {
		return 0, nil
	}
	limiter := s.Admission.limiter
	probed := 0
	for _, target := range probeTargets(release.Snapshot) {
		call, cancel := context.WithTimeout(ctx, reserveTimeout)
		claimed, err := limiter.ClaimProbe(call, target.key(), target.interval)
		cancel()
		if err != nil {
			return probed, err
		}
		if !claimed {
			continue
		}
		result, ok := s.probe(ctx, release, target)
		if !ok {
			continue
		}
		probed++
		if err := s.shareProbe(ctx, limiter, target, result); err != nil {
			return probed, err
		}
	}
	return probed, nil
}

// probeTargets lists the probe-enabled provider models in route order. A
// probe speaks the canonical OpenAI shape, so strict routes, which carry a
// native dialect verbatim, are never probed through.
func probeTargets(snapshot *runtime.Snapshot) []probeTarget {
	var targets []probeTarget
	seen := map[string]bool{}
	for _, slug := range slices.Sorted(maps.Keys(snapshot.Routes)) {
		route := snapshot.Routes[slug]
		operation := ""
		for _, candidate := range []string{operationGeneration, "embeddings", "rerank"} {
			if slices.Contains(route.Operations, candidate) {
				operation = candidate
				break
			}
		}
		if operation == "" || route.Fidelity.Strict() {
			continue
		}
		for _, target := range route.Targets {
			provider, ok := snapshot.Providers[target.ProviderID]
			if target.Shadow != nil || !ok || !provider.Enabled || provider.HealthProbe == nil {
				continue
			}
			candidate := probeTarget{route: route, target: target, provider: provider.ID, operation: operation, interval: provider.HealthProbe.Interval()}
			if !seen[candidate.key()] {
				seen[candidate.key()] = true
				targets = append(targets, candidate)
			}
		}
	}
	return targets
}

// probe sends one synthetic request to the target through the route's own
// planning and execution, so it is gated, priced and recorded like any
// attempt, and accounted to the installation as a probe. It reports false
// when no attempt reached the provider, which says nothing about its health.
func (s *Server) probe(ctx context.Context, release *runtime.Release, target probeTarget) (limits.ProbeResult, bool) {
	shape := probeRequests[target.operation]
	parsed, err := protocols.Parse(shape.family, fmt.Appendf(nil, shape.body, target.route.Slug), target.route.Slug)
	if err != nil {
		s.log.Warn("health probe request could not be built", "operation", target.operation, "error", err.Error())
		return limits.ProbeResult{}, false
	}
	// The probe plans against the route narrowed to the probed target, with
	// one attempt, no route behavior beyond its spend cap, and a short
	// deadline.
	route := target.route
	route.Targets = []runtime.Target{target.target}
	route.MaxAttempts = 1
	route.OverallTimeout = min(route.OverallTimeout, probeDeadline.Milliseconds())
	route.Behavior = runtime.Behavior{Budget: route.Budget}
	snapshot := *release.Snapshot
	snapshot.Routes = map[string]runtime.Route{route.Slug: route}
	narrowed := *release
	narrowed.Snapshot = &snapshot
	x := &execution{
		request:  request{id: uuid.Must(uuid.NewV7()).String(), minted: true, startedAt: s.now(), release: &narrowed},
		family:   shape.family,
		parsed:   parsed,
		actor:    "system",
		mode:     "unary",
		route:    &route,
		origin:   usage.OriginProbe,
		priority: runtime.PriorityLow,
	}
	x.bind(canonicalPlanner, nil, true)
	defer s.settleCaps(ctx, x)
	if e := s.planCanonical(ctx, x); e != nil {
		s.log.Info("health probe not planned", "provider_id", target.provider, "route", route.Slug, "code", e.Code)
		return limits.ProbeResult{}, false
	}
	x.budget, x.allowance = 1, 1
	out := s.execute(ctx, x)
	s.finish(x, out, cmp.Or(errorStatus(out.err), 200))
	if !x.dispatched || len(x.facts) == 0 {
		return limits.ProbeResult{}, false
	}
	fact := x.facts[len(x.facts)-1]
	result := limits.ProbeResult{
		Status:     limits.ProbeHealthy,
		Model:      target.target.ProviderModel,
		ObservedAt: s.now().UTC(),
		LatencyMS:  fact.Duration.Milliseconds(),
	}
	if out.err != nil {
		result.Class = fact.Class
		if unhealthyClass(fact.Class) {
			result.Status = limits.ProbeUnhealthy
		}
	}
	return result, true
}

// shareProbe publishes a probe result: an unhealthy one opens the provider's
// fleet circuit until the next probe can decide again, a healthy one closes
// it.
func (s *Server) shareProbe(ctx context.Context, limiter *limits.Limiter, target probeTarget, result limits.ProbeResult) error {
	call, cancel := context.WithTimeout(ctx, reserveTimeout)
	defer cancel()
	if err := limiter.RecordProbe(call, target.provider, result); err != nil {
		return err
	}
	var until time.Time
	if result.Status == limits.ProbeUnhealthy {
		until = s.now().Add(max(target.interval, circuitOpenFor))
	}
	return limiter.PublishCircuit(call, target.provider, until)
}

// unhealthyClass reports whether a failure says the provider itself failed,
// as opposed to the request, its credential or its quota.
func unhealthyClass(class string) bool {
	switch class {
	case classUpstreamServer, classConnect, classTimeout, classProtocol:
		return true
	}
	return false
}

func errorStatus(e *Error) int {
	if e == nil {
		return 0
	}
	return e.Status
}
