package gateway

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/routing"
)

// requestPreparation belongs to one immutable effective source on one route leg.
// Each target record keeps its admission evidence when its dispatch plan is evicted.
type requestPreparation struct {
	source        oif.Document
	route         *routing.Route
	targets       map[preparationKey]*targetPreparation
	sourceSummary *requestSummary
}

type preparationKey struct{ target, provider, revision, model string }

type targetPreparation struct {
	provider  preparedProvider
	unary     unaryAdmission
	encoding  encodedRequest
	output    effectiveOutput
	hasOutput bool
}

type unaryAdmission struct {
	plan       *operationplan.Plan
	estimate   int64
	parameters routing.Names
	receipt    oif.Receipt
}

func (x *call) prepareSource() {
	var source oif.Document
	if x.unary != nil {
		source = x.unary.source.Document()
	} else if x.generation.parsed != nil {
		source = x.generation.parsed.OIF().Document()
	}
	if x.requestPreparation.source != source || x.requestPreparation.route != x.route {
		x.requestPreparation = requestPreparation{source: source, route: x.route}
	}
}
func (x *call) targetPreparation(p *routing.Provider, targetID, model string) *targetPreparation {
	x.prepareSource()
	key := preparationKey{target: targetID, provider: p.ID, revision: p.RevisionID, model: model}
	if record := x.targets[key]; record != nil {
		return record
	}
	if x.targets == nil {
		x.targets = make(map[preparationKey]*targetPreparation)
	}
	record := &targetPreparation{}
	x.targets[key] = record
	return record
}
func (x *call) providerAdmission(p *routing.Provider, targetID, model string) (preparedProvider, error) {
	record := x.targetPreparation(p, targetID, model)
	if record.provider.demand != nil {
		return record.provider, nil
	}
	return x.preparedProvider(p, targetID, model)
}
func (x *call) unaryAdmission(p *routing.Provider, targetID, model string) (unaryAdmission, error) {
	record := x.targetPreparation(p, targetID, model)
	if record.unary.receipt.Operation != "" {
		return record.unary, nil
	}
	_, err := x.unaryPlan(p, targetID, model)
	return record.unary, err
}
func (r *targetPreparation) heavy() bool {
	return r.encoding.body != nil || r.provider.plan != nil || r.provider.invocation.Prepared.Document().Valid() || r.unary.plan != nil
}
func (r *targetPreparation) discard(planning bool) {
	r.encoding = encodedRequest{}
	r.provider.plan, r.provider.invocation = nil, providerinvoke.Invocation{}
	r.unary.plan = nil
	if planning {
		if r.provider.parameters != nil {
			r.provider.parameters = routing.Listed(r.provider.parameters())
		}
	} else {
		r.provider.parameters, r.unary.parameters = nil, nil
	}
}
func (x *call) retainPreparation(record *targetPreparation) {
	count := 0
	for _, other := range x.targets {
		if other != record && other.heavy() {
			count++
		}
	}
	if count >= keptEncodings {
		record.discard(x.requiresParameters())
	}
}
func (x *call) finishPreparationPlanning() {
	keep := make(map[preparationKey]bool, keptEncodings)
	for _, a := range x.attempts {
		if len(keep) >= keptEncodings {
			break
		}
		keep[preparationKey{target: a.TargetID, provider: a.ProviderID, revision: a.ProviderRevisionID, model: a.UpstreamModel}] = true
	}
	for key, record := range x.targets {
		if !keep[key] {
			record.discard(false)
		}
	}
}

// activatePreparation releases unused bodies before dispatch and long streaming.
func (x *call) activatePreparation(p *routing.Provider, targetID, model string) {
	key := preparationKey{target: targetID, provider: p.ID, revision: p.RevisionID, model: model}
	for other, record := range x.targets {
		if other != key {
			record.discard(false)
		}
	}
}
func (x *call) hasPreparedProviders() bool {
	for _, record := range x.targets {
		if record.provider.demand != nil {
			return true
		}
	}
	return false
}

// requiresParameters is fixed by the policies of this route leg. Only those
// policies need parameter metadata after an evicted plan has been released.
func (x *call) requiresParameters() bool {
	requested := func(p *bool) bool { return p != nil && *p }
	if x.preferences != nil && requested(x.preferences.RequireParameters) {
		return true
	}
	requires := func(p *routing.Policy) bool {
		return p != nil && (requested(p.Constraints.RequireParameters) || requested(p.Defaults.RequireParameters))
	}
	if x.route != nil && requires(x.route.Policy) {
		return true
	}
	var snapshot *routing.Snapshot
	if x.historicalSnapshot != nil {
		snapshot = x.historicalSnapshot
	} else if x.request.release != nil {
		snapshot = x.request.release.Snapshot
	}
	return snapshot != nil && (requires(snapshot.InstallationPolicy) || requires(snapshot.KeyPolicies[x.keyID]))
}
