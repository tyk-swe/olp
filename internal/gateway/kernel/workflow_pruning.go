package kernel

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/domain/operations/decision"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type workflowRelevanceExchange struct {
	Calls   [][]string `json:"calls"`
	Results [][]string `json:"results"`
}

type pruningQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
}

type pruningRequest struct {
	Model     string                     `json:"model"`
	State     string                     `json:"state"`
	Questions map[string]pruningQuestion `json:"questions"`
}

type pruningResponse struct {
	Answers map[string]struct {
		Type string   `json:"type"`
		Noul *float64 `json:"noul"`
	} `json:"answers"`
}

func (s *Server) pruneWorkflow(ctx context.Context, x *Call, request *wire.Request) (*wire.Request, *fault.Error) {
	if x.guard.engine == nil || x.workflow == nil {
		return request, nil
	}
	return s.pruneWorkflowPolicies(ctx, x, request, x.guard.engine.TypeSafePolicies())
}

func (s *Server) pruneWorkflowPolicies(ctx context.Context, x *Call, request *wire.Request, policies []guardrails.TypeSafePolicy) (*wire.Request, *fault.Error) {
	if len(policies) == 0 {
		return request, nil
	}
	plan, err := protocols.PlanWorkflowPruning(request)
	if err != nil {
		return nil, guardrailError(err)
	}
	candidates := plan.Candidates()
	if len(candidates) == 0 {
		return request, nil
	}
	processed, system, problem := x.pruningInput(ctx, request)
	if problem != nil {
		return nil, problem
	}
	if processed == nil {
		return request, nil
	}
	processedPlan, err := protocols.PlanWorkflowPruning(processed)
	if err != nil {
		return nil, guardrailError(err)
	}
	processedCandidates := processedPlan.Candidates()
	if len(processedCandidates) != len(candidates) {
		return nil, guardrailError(guardrails.ErrSurface)
	}
	surface, err := guardrails.NativeSurface(processed.OIF().Document(), false)
	if err != nil {
		return nil, guardrailError(err)
	}
	indexes := slices.Collect(func(yield func(int) bool) {
		for index, candidate := range candidates {
			if !candidate.ContainsCallID(x.workflowRecoveryCalls) && !yield(index) {
				return
			}
		}
	})
	eligible := slices.Collect(func(yield func(workflowRelevanceExchange) bool) {
		for _, index := range indexes {
			var exchange workflowRelevanceExchange
			for _, piece := range processedCandidates[index].PieceLocations(request.Family) {
				if piece.Call {
					exchange.Calls = append(exchange.Calls, surface.TextAt(piece.Pointer))
				} else {
					exchange.Results = append(exchange.Results, surface.TextAt(piece.Pointer))
				}
			}
			if !yield(exchange) {
				return
			}
		}
	})
	pruneCtx, cancel := context.WithTimeout(ctx, time.Duration(policies[0].Revision.TimeoutMS)*time.Millisecond)
	defer cancel()
	var selected []int
	for start := 0; start < len(eligible); start += 256 {
		batch, problem := s.typeSafeRelevance(pruneCtx, x, policies[0], processed, system, eligible[start:min(start+256, len(eligible))])
		if problem != nil {
			return nil, problem
		}
		for _, index := range batch {
			selected = append(selected, indexes[start+index])
		}
	}
	pruned, err := plan.Apply(selected)
	if err != nil {
		return nil, guardrailError(err)
	}
	return s.pruneWorkflowPolicies(ctx, x, pruned, policies[1:])
}

func (x *Call) pruningInput(ctx context.Context, request *wire.Request) (*wire.Request, []string, *fault.Error) {
	document := request.OIF().Document()
	if interaction.CheckGuardrailCoverage(request.Family, document) != nil && !x.guard.engine.OnlyTypeSafe() {
		result, err := x.guard.engine.Unavailable()
		x.guard.record(result.Decisions)
		return nil, nil, guardrailError(err)
	}
	surface, err := guardrails.NativeSurface(document, false)
	if err != nil {
		result, failure := x.guard.engine.Unavailable()
		x.guard.record(result.Decisions)
		return nil, nil, guardrailError(failure)
	}
	segments := slices.Clone(surface.Segments)
	for index, segment := range segments {
		if segment.Kind == guardrails.ToolResult && slices.Contains(x.workflowRecoveryCalls, segment.ToolCallID) {
			segments[index].Text = surface.Replacement(index, "[recovered content omitted]")
		}
	}
	document, err = surface.Apply(segments)
	if err != nil {
		return nil, nil, guardrailError(err)
	}
	document, segments, err = x.inspectGuardrailInputWithRecovery(ctx, document, false)
	if err != nil {
		return nil, nil, guardrailError(err)
	}
	processed, err := wire.NewRequest(request.Family, request.Route, request.Stream, document)
	if err != nil {
		return nil, nil, guardrailError(err)
	}
	system := slices.Collect(func(yield func(string) bool) {
		for _, segment := range segments {
			if segment.Role == "system" && !yield(segment.Text) {
				return
			}
		}
	})
	return processed, system, nil
}

func (s *Server) typeSafeRelevance(ctx context.Context, parent *Call, policy guardrails.TypeSafePolicy, request *wire.Request, system []string, candidates []workflowRelevanceExchange) ([]int, *fault.Error) {
	route, ok := parent.request.release.Snapshot().FindRoute(policy.Config.Route)
	if !ok || !slices.Contains(route.Operations, "decision") {
		return pruningSurfaceError(policy, guardrails.ErrSurface)
	}
	current, problem := s.workflowAuthority(ctx, parent, &route, "")
	if problem != nil {
		return nil, problem
	}
	body, err := buildPruningBody(route.Slug, request, system, candidates)
	if err != nil {
		return nil, guardrailError(err)
	}
	if len(body) > int(s.Config.MaxBodyBytes) && len(candidates) > 1 {
		middle := len(candidates) / 2
		first, problem := s.typeSafeRelevance(ctx, parent, policy, request, system, candidates[:middle])
		if problem != nil {
			return nil, problem
		}
		second, problem := s.typeSafeRelevance(ctx, parent, policy, request, system, candidates[middle:])
		if problem != nil {
			return nil, problem
		}
		for _, index := range second {
			first = append(first, middle+index)
		}
		return first, nil
	}
	raw, problem := s.runPruningDecision(ctx, parent, &route, *current, policy, body)
	if problem != nil || raw == nil {
		return nil, problem
	}
	return parsePruningSelection(raw, len(candidates), policy)
}

func pruningSurfaceError(policy guardrails.TypeSafePolicy, err error) ([]int, *fault.Error) {
	if policy.Revision.OnError == guardrails.FailOpen {
		return nil, nil
	}
	return nil, guardrailError(err)
}

func buildPruningBody(slug string, request *wire.Request, system []string, candidates []workflowRelevanceExchange) ([]byte, error) {
	state, err := json.Marshal(struct {
		Task      string                               `json:"task"`
		System    []string                             `json:"system,omitempty"`
		Exchanges map[string]workflowRelevanceExchange `json:"exchanges"`
	}{Task: protocols.WorkflowPruningTask(request, guardrails.MaxContentBytes), System: system, Exchanges: maps.Collect(func(yield func(string, workflowRelevanceExchange) bool) {
		for index, candidate := range candidates {
			if !yield(protocols.WorkflowExchangeName(index), candidate) {
				return
			}
		}
	})})
	if err != nil {
		return nil, err
	}
	questions := maps.Collect(func(yield func(string, pruningQuestion) bool) {
		for index := range candidates {
			name := protocols.WorkflowExchangeName(index)
			if !yield(name, pruningQuestion{Type: "noul", Instructions: "Is the completed tool exchange " + name + " relevant to the current task?"}) {
				return
			}
		}
	})
	return json.Marshal(pruningRequest{Model: slug, State: string(state), Questions: questions})
}

func (s *Server) runPruningDecision(ctx context.Context, parent *Call, route *routing.Route, current keyauthority.Authority, policy guardrails.TypeSafePolicy, body []byte) ([]byte, *fault.Error) {
	codec, ok := operationregistry.Lookup(decision.SystemOne)
	if !ok {
		return nil, guardrailError(guardrails.ErrSurface)
	}
	source, err := operationplan.Parse(decision.SystemOne, body, int(s.Config.MaxBodyBytes))
	if err != nil {
		if policy.Revision.OnError == guardrails.FailOpen {
			return nil, nil
		}
		return nil, guardrailError(err)
	}
	x := &Call{
		request: requestMetadataForPruning(parent, body), family: wire.SystemOne, mode: "unary",
		actor: parent.actor, keyID: parent.keyID, budgetGroupID: parent.budgetGroupID,
		authority: current, attribution: parent.attribution, endUserDigest: parent.endUserDigest,
		userID: parent.userID, origin: usage.OriginClassifier, parent: parent.request.accountingID(),
		priority: parent.priority, guard: guardrailCall{internal: true}, authorize: parent.authorize,
		deadline: parent.deadline, spent: parent.spent, facts: slices.Clone(parent.facts),
		routeLeg: routeLeg{Route: route}, workflow: parent.workflow,
		generation: generationCall{runLimits: parent.generation.runLimits},
		unary:      &unaryCall{source: source, dialect: codec, route: route.Slug, surface: wire.SystemOne.Surface()},
	}
	if problem := s.prepareUnary(ctx, x); problem != nil {
		return nil, problem
	}
	x.primary = parent.named()
	priorFacts := len(x.facts)
	stepCtx, cancel := x.routeContext(ctx)
	defer cancel()
	x.estimate = requestEstimate(x)
	authority := cacheAuthority(x.AdmissionAuthority(x.authority), false, true, false)
	lease, problem := s.Deps.Admission.ReserveKeyCosted(stepCtx, &authority, x.clientSurface(), quota.KeyReservationEstimate(x.estimate, s.dispatchableAttempts(x)), time.Duration(x.named().OverallTimeout)*time.Millisecond, quota.CostReservation{}, x.limitRoute())
	if problem != nil {
		return nil, problem
	}
	result := runAttempts(stepCtx, s, x, attemptAdapter[operationplan.Result]{demand: x.attemptDemand, dispatch: func(attemptCtx context.Context, attempt routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
		return s.unaryAttempt(attemptCtx, x, attempt, provider, slot, fact)
	}})
	quota.SettleKey(ctx, lease, x.stepDispatched, x.settledTokensFor(x.facts[priorFacts:], x.estimate), s.Log)
	// Later steps must include this independently routed attempt in the same run.
	parent.facts, parent.spent = x.facts, x.spent
	if result.err != nil {
		if policy.Revision.OnError == guardrails.FailOpen && (x.sessionDebit == nil || !x.sessionDebit.dispatched) && (result.err.Code == "upstream_unavailable" || result.err.Code == "gateway_timeout") {
			return nil, nil
		}
		return nil, result.err
	}
	return result.result.Body, nil
}

func parsePruningSelection(raw []byte, count int, policy guardrails.TypeSafePolicy) ([]int, *fault.Error) {
	var decoded pruningResponse
	if json.Unmarshal(raw, &decoded) != nil || len(decoded.Answers) != count {
		return pruningSurfaceError(policy, guardrails.ErrSurface)
	}
	for index := range count {
		answer, exists := decoded.Answers[protocols.WorkflowExchangeName(index)]
		if !exists || answer.Type != "noul" || answer.Noul == nil || *answer.Noul < 0 || *answer.Noul > 1 {
			return pruningSurfaceError(policy, guardrails.ErrSurface)
		}
	}
	return slices.Collect(func(yield func(int) bool) {
		for index := range count {
			if *decoded.Answers[protocols.WorkflowExchangeName(index)].Noul < 0.2 && !yield(index) {
				return
			}
		}
	}), nil
}

func requestMetadataForPruning(parent *Call, body []byte) request {
	return request{id: parent.request.id, durableID: parent.request.durableID, minted: parent.request.minted,
		clientIP: parent.request.clientIP, startedAt: parent.request.startedAt, release: parent.request.release,
		delegationAssertion: parent.request.delegationAssertion, callerCredential: parent.request.callerCredential,
		body: &ingress.MeasuredBody{Decoded: int64(len(body))}}
}
