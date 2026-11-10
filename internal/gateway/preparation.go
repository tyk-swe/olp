package gateway

import (
	"slices"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
)

type requestPreparation struct {
	source             oif.Document
	route              *routing.Route
	targets            map[preparationKey]targetPreparation
	sourceSummary      *requestSummary
	requiresParameters bool
}

type preparationKey struct{ target, provider, revision, model string }

func providerPreparationKey(provider *routing.Provider, targetID, model string) preparationKey {
	return preparationKey{target: targetID, provider: provider.ID, revision: provider.RevisionID, model: model}
}

func attemptPreparationKey(attempt routing.Attempt) preparationKey {
	return preparationKey{target: attempt.TargetID, provider: attempt.ProviderID, revision: attempt.ProviderRevisionID, model: attempt.UpstreamModel}
}

type targetPreparation struct {
	guardrailDecisions []guardrails.Evidence
	provider           preparedProvider
	unary              unaryAdmission
	encoding           encodedRequest
	output             effectiveOutput
	hasOutput          bool
}

type unaryAdmission struct {
	plan               *operationplan.Plan
	estimate           int64
	parameters         routing.Names
	receipt            oif.Receipt
	guardrailDecisions []guardrails.Evidence
}

type preparationEvidence struct {
	guardrails []guardrails.Evidence
	planClass  string
	output     *int64
	document   oif.Document
}

func (x *call) requestDocument() oif.Document {
	if x.unary != nil {
		return x.unary.source.Document()
	}
	if x.generation.parsed != nil {
		return x.generation.parsed.OIF().Document()
	}
	return oif.Document{}
}

func (x *call) preparation() *requestPreparation {
	source := x.requestDocument()
	if x.prepared == nil || x.prepared.source != source || x.prepared.route != x.route {
		x.prepared = &requestPreparation{source: source, route: x.route, requiresParameters: x.requiresParameters()}
	}
	return x.prepared
}

func (p *requestPreparation) summary(request *wire.Request, build func() requestSummary) requestSummary {
	if p.sourceSummary == nil || p.sourceSummary.request != request {
		summary := build()
		p.sourceSummary = &summary
	}
	return *p.sourceSummary
}

func (p *requestPreparation) provider(key preparationKey, payload bool, build func() (preparedProvider, error)) (preparedProvider, error) {
	record := p.targets[key]
	if record.provider.invocation.Prepared.Document().Valid() || !payload && record.provider.demand != nil {
		return record.provider, nil
	}
	prepared, err := build()
	evidence := guardrails.MergeEvidence(record.guardrailDecisions, prepared.guardrailDecisions)
	if err != nil {
		p.remember(key, record.withEvidence(evidence))
		return prepared, err
	}
	p.remember(key, targetPreparation{
		guardrailDecisions: evidence, provider: prepared, unary: record.unary,
		encoding: record.encoding, output: record.output, hasOutput: record.hasOutput,
	})
	return prepared, nil
}

func (p *requestPreparation) operation(key preparationKey, payload bool, build func() (unaryAdmission, error)) (unaryAdmission, error) {
	record := p.targets[key]
	if record.unary.plan != nil || !payload && record.unary.receipt.Operation != "" {
		return record.unary, nil
	}
	prepared, err := build()
	evidence := guardrails.MergeEvidence(record.guardrailDecisions, prepared.guardrailDecisions)
	if err != nil {
		p.remember(key, record.withEvidence(evidence))
		return prepared, err
	}
	p.remember(key, targetPreparation{
		guardrailDecisions: evidence, unary: prepared, provider: record.provider,
		encoding: record.encoding, output: record.output, hasOutput: record.hasOutput,
	})
	return prepared, nil
}

func (p *requestPreparation) encoded(key preparationKey, build func() (encodedRequest, error)) (encodedRequest, error) {
	record := p.targets[key]
	if record.encoding.document.Valid() {
		return record.encoding, nil
	}
	encoded, err := build()
	if err != nil {
		return encodedRequest{}, err
	}
	p.remember(key, targetPreparation{
		guardrailDecisions: record.guardrailDecisions, provider: record.provider, unary: record.unary,
		encoding: encoded, output: encoded.output, hasOutput: true,
	})
	return encoded, nil
}

func (p *requestPreparation) output(key preparationKey, build func() (encodedRequest, error)) (effectiveOutput, error) {
	if record := p.targets[key]; record.hasOutput {
		return record.output, nil
	}
	encoded, err := p.encoded(key, build)
	return encoded.output, err
}

func (p *requestPreparation) attemptEvidence(attempt routing.Attempt) preparationEvidence {
	return p.evidence(attemptPreparationKey(attempt))
}

func (p *requestPreparation) factEvidence(fact *AttemptFact) preparationEvidence {
	return p.evidence(preparationKey{target: fact.TargetID, provider: fact.ProviderID, revision: fact.ProviderRevisionID, model: fact.UpstreamModel})
}

func (p *requestPreparation) evidence(key preparationKey) preparationEvidence {
	record := p.targets[key]
	receipt := record.provider.receipt
	if record.unary.receipt.Operation != "" {
		receipt = &record.unary.receipt
	}
	output := record.output.output
	if !record.hasOutput && record.provider.demand != nil {
		output = record.provider.demand.MaxOutputTokens
	}
	planClass := ""
	if receipt != nil {
		planClass = receipt.Class
	}
	return preparationEvidence{
		guardrails: slices.Clone(record.guardrailDecisions), planClass: planClass, output: copiedOutput(output),
		document: record.provider.invocation.Prepared.Document(),
	}
}

func (r targetPreparation) withEvidence(evidence []guardrails.Evidence) targetPreparation {
	return targetPreparation{
		guardrailDecisions: evidence, provider: r.provider, unary: r.unary,
		encoding: r.encoding, output: r.output, hasOutput: r.hasOutput,
	}
}

func (r targetPreparation) heavy() bool {
	return r.encoding.document.Valid() || r.provider.plan != nil || r.provider.invocation.Prepared.Document().Valid() || r.unary.plan != nil
}

func (r targetPreparation) discard(planning bool) targetPreparation {
	return targetPreparation{
		guardrailDecisions: r.guardrailDecisions,
		provider: preparedProvider{
			admitted: r.provider.admitted, parameters: retainedParameters(r.provider.parameters, planning),
			demand: r.provider.demand, receipt: r.provider.receipt, policyDecisions: r.provider.policyDecisions,
			guardrailSegments: r.provider.guardrailSegments, guardrailDecisions: r.provider.guardrailDecisions,
		},
		unary: unaryAdmission{
			estimate: r.unary.estimate, parameters: retainedParameters(r.unary.parameters, planning),
			receipt: r.unary.receipt, guardrailDecisions: r.unary.guardrailDecisions,
		},
		output: r.output, hasOutput: r.hasOutput,
	}
}

func retainedParameters(parameters routing.Names, planning bool) routing.Names {
	if !planning || parameters == nil {
		return nil
	}
	return routing.Listed(parameters())
}

func copiedOutput(output *int64) *int64 {
	if output == nil {
		return nil
	}
	return new(*output)
}

func (p *requestPreparation) remember(key preparationKey, record targetPreparation) {
	if p.targets == nil {
		p.targets = make(map[preparationKey]targetPreparation)
	}
	p.targets[key] = p.retained(key, record)
}

func (p *requestPreparation) retained(key preparationKey, record targetPreparation) targetPreparation {
	count := 0
	for otherKey, other := range p.targets {
		if otherKey != key && other.heavy() {
			count++
		}
	}
	if count >= keptEncodings {
		return record.discard(p.requiresParameters)
	}
	return record
}

func (p *requestPreparation) finishPlanning(attempts []routing.Attempt) {
	keep := make(map[preparationKey]bool, keptEncodings)
	for _, attempt := range attempts {
		if len(keep) >= keptEncodings {
			break
		}
		keep[attemptPreparationKey(attempt)] = true
	}
	for key, record := range p.targets {
		if !keep[key] {
			p.targets[key] = record.discard(false)
		}
	}
}

func (p *requestPreparation) activate(attempt routing.Attempt) {
	key := attemptPreparationKey(attempt)
	for other, record := range p.targets {
		if other != key {
			p.targets[other] = record.discard(false)
		}
	}
}

func (p *requestPreparation) hasProviders() bool {
	for _, record := range p.targets {
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
	var snapshot *routing.View
	if x.historicalSnapshot != nil {
		snapshot = x.historicalSnapshot
	} else if x.request.release != nil {
		snapshot = x.request.release.Snapshot()
	}
	return snapshot != nil && (requires(snapshot.InstallationPolicy()) || requires(snapshot.KeyPolicy(x.keyID)))
}
