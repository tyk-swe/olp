package healthprobes

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/gateway/health"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/release"
)

const (
	generation = "generation"
	// probeTick is how often the probe worker looks for targets due a probe.
	probeTick = 15 * time.Second
	// probeDeadline bounds one probe, below any route deadline.
	probeDeadline = 30 * time.Second
)

// probeTarget is one provider model a route serves through a connection that
// opted into probing. Each is probed once per interval across the fleet, on
// the first route that serves it.
type probeTarget struct {
	route     routing.Route
	target    routing.Target
	provider  string
	operation string
	interval  time.Duration
}

func (t probeTarget) key() string { return t.provider + "/" + t.target.ProviderModel }

// probeRequests are the certification requests a probe sends, by operation:
// the smallest each operation answers, addressed to the probed route.
var probeRequests = map[string]struct {
	family wire.Format
	body   string
}{
	generation:   {wire.Chat, `{"model":%q,"messages":[{"role":"user","content":"Reply with OK."}],"max_tokens":16}`},
	"embeddings": {wire.Embeddings, `{"model":%q,"input":"embedding probe"}`},
	"rerank":     {wire.Rerank, `{"model":%q,"query":"rerank probe","documents":["first document","second document"]}`},
}

type Prober struct {
	Kernel    *kernel.Server
	Runtime   kernel.Runtime
	Admission *quota.Admission
}

// Run probes every target whose connection enables probing, each
// once per its interval across the fleet, until ctx ends. Every pass is
// reported to checkpoint. Probes need shared state to divide work and share
// results, so without it this returns at once.
func (p Prober) Run(ctx context.Context, checkpoint func(ctx context.Context, probed int, err error)) {
	if !p.Admission.Ready() {
		return
	}
	ticker := time.NewTicker(probeTick)
	defer ticker.Stop()
	for {
		probed, err := p.probeDue(ctx)
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
func (p Prober) probeDue(ctx context.Context) (int, error) {
	release := p.Runtime.Release()
	if release == nil || release.Snapshot() == nil {
		return 0, nil
	}
	limiter := p.Admission.Fleet()
	probed := 0
	for _, target := range probeTargets(release.Snapshot()) {
		call, cancel := context.WithTimeout(ctx, quota.ReserveTimeout)
		claimed, err := limiter.ClaimProbe(call, target.key(), target.interval)
		cancel()
		if err != nil {
			return probed, err
		}
		if !claimed {
			continue
		}
		result, ok := probe(ctx, p.Kernel, release, target)
		if !ok {
			continue
		}
		probed++
		if err := shareProbe(ctx, p.Kernel, limiter, target, result); err != nil {
			return probed, err
		}
	}
	return probed, nil
}

// probeTargets lists the probe-enabled provider models in route order. A
// probe speaks the canonical OpenAI shape, so strict routes, which carry a
// native dialect verbatim, are never probed through.
func probeTargets(snapshot *routing.View) []probeTarget {
	var targets []probeTarget
	seen := map[string]bool{}
	for _, slug := range snapshot.RouteSlugs() {
		route := snapshot.Route(slug)
		operation := ""
		for _, candidate := range []string{generation, "embeddings", "rerank"} {
			if slices.Contains(route.Operations, candidate) {
				operation = candidate
				break
			}
		}
		if operation == "" || route.Fidelity.Strict() {
			continue
		}
		for _, target := range route.Targets {
			provider, ok := snapshot.FindProvider(target.ProviderID)
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
func probe(ctx context.Context, s *kernel.Server, release *release.Release, target probeTarget) (fleet.ProbeResult, bool) {
	shape := probeRequests[target.operation]
	if target.operation == generation {
		provider := release.Snapshot().Provider(target.provider)
		if family, err := provider.Connector().TargetFamily(wire.Chat); err == nil && family == wire.Completions {
			shape.family = wire.Completions
			shape.body = `{"model":%q,"prompt":"Reply with OK.","max_tokens":16}`
		}
	}
	parsed, err := protocols.Parse(shape.family, fmt.Appendf(nil, shape.body, target.route.Slug), target.route.Slug)
	if err != nil {
		s.Log.Warn("health probe request could not be built", "operation", target.operation, "error", err.Error())
		return fleet.ProbeResult{}, false
	}
	route := target.route
	route.Targets = []routing.Target{target.target}
	route.MaxAttempts = 1
	route.OverallTimeout = min(route.OverallTimeout, probeDeadline.Milliseconds())
	route.Behavior = routing.Behavior{Budget: route.Budget}
	attempt := s.ProbeGeneration(ctx, release, route, parsed)
	if e := attempt.Unplanned; e != nil {
		s.Log.Info("health probe not planned", "provider_id", target.provider, "route", route.Slug, "code", e.Code)
	}
	if !attempt.Reached {
		return fleet.ProbeResult{}, false
	}
	result := fleet.ProbeResult{
		Status:     fleet.ProbeHealthy,
		Model:      target.target.ProviderModel,
		ObservedAt: s.Now().UTC(),
		LatencyMS:  attempt.Latency.Milliseconds(),
	}
	if attempt.Failed {
		result.Class = attempt.Class
		if unhealthyClass(attempt.Class) {
			result.Status = fleet.ProbeUnhealthy
		}
	}
	return result, true
}

// shareProbe publishes a probe result: an unhealthy one opens the provider's
// fleet circuit until the next probe can decide again, a healthy one closes
// it.
func shareProbe(ctx context.Context, s *kernel.Server, limiter *fleet.State, target probeTarget, result fleet.ProbeResult) error {
	call, cancel := context.WithTimeout(ctx, quota.ReserveTimeout)
	defer cancel()
	if err := limiter.RecordProbe(call, target.provider, result); err != nil {
		return err
	}
	var until time.Time
	if result.Status == fleet.ProbeUnhealthy {
		until = s.Now().Add(max(target.interval, health.CircuitOpenFor))
	}
	return limiter.PublishCircuit(call, target.provider, until)
}

// unhealthyClass reports whether a failure says the provider itself failed,
// as opposed to the request, its credential or its quota.
func unhealthyClass(class string) bool {
	switch class {
	case kernel.ClassUpstreamServer, kernel.ClassConnect, kernel.ClassTimeout, kernel.ClassProtocol:
		return true
	}
	return false
}
