package gateway

import (
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/providerinvoke"
	"github.com/tyk-swe/olp/internal/runtime"
)

// requestPreparation belongs to one immutable effective source on one route
// leg. Replacing that owner invalidates every derived representation together.
// Admission receipts outlive heavyweight bodies, which dispatch can rebuild
// deterministically from the same source and published templates.
type requestPreparation struct {
	source            oif.Document
	route             *runtime.Route
	preparedProviders map[string]preparedProvider
	unaryAdmissions   map[string]unaryAdmission
	encoded           map[string]encodedRequest
	effectiveOutputs  map[string]effectiveOutput
	sourceSummary     *requestSummary
}

type unaryAdmission struct {
	plan       *operationplan.Plan
	estimate   int64
	parameters runtime.Names
	receipt    oif.Receipt
}

func (x *execution) prepareSource() {
	var source oif.Document
	if x.unary != nil {
		source = x.unary.source.Document()
	} else if x.parsed != nil {
		source = x.parsed.OIF().Document()
	}
	if x.requestPreparation.source != source || x.requestPreparation.route != x.route {
		x.requestPreparation = requestPreparation{source: source, route: x.route}
	}
}

func (x *execution) providerAdmission(p *runtime.Provider, model string) (preparedProvider, error) {
	x.prepareSource()
	if admission, ok := x.preparedProviders[encodedKey(p, model)]; ok {
		return admission, nil
	}
	return x.preparedProvider(p, model)
}

func (x *execution) unaryAdmission(p *runtime.Provider, model string) (unaryAdmission, error) {
	x.prepareSource()
	if admission, ok := x.unaryAdmissions[encodedKey(p, model)]; ok {
		return admission, nil
	}
	_, err := x.unaryPlan(p, model)
	return x.unaryAdmissions[encodedKey(p, model)], err
}

func (x *execution) discardPrepared(key string) {
	if admission, ok := x.preparedProviders[key]; ok {
		admission.plan, admission.invocation = nil, providerinvoke.Invocation{}
		// Parameter names are needed only while routing. The lazy projection
		// otherwise retains an entire native document through long streams.
		admission.parameters = nil
		x.preparedProviders[key] = admission
	}
	if admission, ok := x.unaryAdmissions[key]; ok {
		admission.plan, admission.parameters = nil, nil
		x.unaryAdmissions[key] = admission
	}
}

func (x *execution) finishPreparationPlanning() {
	keep := map[string]bool{}
	for _, attempt := range x.attempts {
		if len(keep) >= keptEncodings {
			break
		}
		keep[attempt.ProviderID+"/"+attempt.ProviderRevisionID+"/"+attempt.UpstreamModel] = true
	}
	for key := range x.preparedProviders {
		if !keep[key] {
			x.discardPrepared(key)
		}
	}
	for key := range x.unaryAdmissions {
		if !keep[key] {
			x.discardPrepared(key)
		}
	}
}

// activatePreparation runs only after slot admission and final authority checks.
// Unused plans cannot remain reachable for the lifetime of a slow stream.
func (x *execution) activatePreparation(p *runtime.Provider, model string) {
	key := encodedKey(p, model)
	for other := range x.encoded {
		if other != key {
			delete(x.encoded, other)
		}
	}
	for other := range x.preparedProviders {
		if other != key {
			x.discardPrepared(other)
		}
	}
	for other := range x.unaryAdmissions {
		if other != key {
			x.discardPrepared(other)
		}
	}
}
