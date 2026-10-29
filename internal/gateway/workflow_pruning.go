package gateway

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations/decision"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
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

func (s *Server) pruneWorkflow(ctx context.Context, x *call, request *wire.Request) (*wire.Request, *Error) {
	if x.guardrailEngine == nil || x.workflow == nil {
		return request, nil
	}
	return s.pruneWorkflowPolicies(ctx, x, request, x.guardrailEngine.TypeSafePolicies())
}

func (s *Server) pruneWorkflowPolicies(ctx context.Context, x *call, request *wire.Request, policies []guardrails.TypeSafePolicy) (*wire.Request, *Error) {
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

func (x *call) pruningInput(ctx context.Context, request *wire.Request) (*wire.Request, []string, *Error) {
	document := request.OIF().Document()
	if interaction.CheckGuardrailCoverage(request.Family, document) != nil && !x.guardrailEngine.OnlyTypeSafe() {
		result, err := x.guardrailEngine.Unavailable("")
		x.recordGuardrails(result.Decisions)
		return nil, nil, guardrailError(err)
	}
	surface, err := guardrails.NativeSurface(document, false)
	if err != nil {
		result, failure := x.guardrailEngine.Unavailable("")
		x.recordGuardrails(result.Decisions)
		return nil, nil, guardrailError(failure)
	}
	segments := slices.Clone(surface.Segments)
	for index, segment := range segments {
		if segment.Kind == "tool_result" && slices.Contains(x.workflowRecoveryCalls, segment.ToolCallID) {
			segments[index].Text = surface.Replacement(index, "[recovered content omitted]")
		}
	}
	compiled, problem := compiledPolicy(x.route)
	if problem != nil {
		return nil, nil, problem
	}
	if compiled != nil {
		for _, rule := range compiled.Input {
			matched := false
			for index, segment := range segments {
				if !rule.Re.MatchString(segment.Text) {
					continue
				}
				matched = true
				if rule.Action == contentpolicy.ActionBlock {
					recordDecision(x, contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionBlock, Outcome: contentpolicy.OutcomeBlocked})
					return nil, nil, invalidRequest("content_policy_blocked", "The request was blocked by the route's content policy.", nil)
				}
				segments[index].Text = rule.Re.ReplaceAllLiteralString(segment.Text, rule.Replacement)
			}
			if matched {
				recordDecision(x, contentpolicy.Decision{RuleID: rule.ID, Phase: contentpolicy.PhaseInput, Action: contentpolicy.ActionRedact, Outcome: contentpolicy.OutcomeRedacted})
			}
		}
	}
	document, err = surface.Apply(segments)
	if err != nil {
		return nil, nil, guardrailError(err)
	}
	document, segments, err = x.inspectGuardrailInputWithRecovery(context.WithValue(ctx, guardrailCallerKey{}, x), document, false)
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

func (s *Server) typeSafeRelevance(ctx context.Context, parent *call, policy guardrails.TypeSafePolicy, request *wire.Request, system []string, candidates []workflowRelevanceExchange) ([]int, *Error) {
	route, ok := parent.request.release.Snapshot.Routes[policy.Config.Route]
	if !ok || !slices.Contains(route.Operations, "decision") {
		if policy.Revision.OnError == guardrails.FailOpen {
			return nil, nil
		}
		return nil, guardrailError(guardrails.ErrSurface)
	}
	current, problem := s.workflowAuthority(ctx, parent, &route, "")
	if problem != nil {
		return nil, problem
	}
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
		return nil, guardrailError(err)
	}
	questions := maps.Collect(func(yield func(string, pruningQuestion) bool) {
		for index := range candidates {
			name := protocols.WorkflowExchangeName(index)
			if !yield(name, pruningQuestion{Type: "noul", Instructions: "Is the completed tool exchange " + name + " relevant to the current task?"}) {
				return
			}
		}
	})
	body, err := json.Marshal(pruningRequest{Model: route.Slug, State: string(state), Questions: questions})
	if err != nil {
		return nil, guardrailError(err)
	}
	if len(body) > int(s.cfg.MaxBodyBytes) && len(candidates) > 1 {
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
	codec, ok := operationregistry.Lookup(decision.SystemOne)
	if !ok {
		return nil, guardrailError(guardrails.ErrSurface)
	}
	source, err := operationplan.Parse(decision.SystemOne, body, int(s.cfg.MaxBodyBytes))
	if err != nil {
		if policy.Revision.OnError == guardrails.FailOpen {
			return nil, nil
		}
		return nil, guardrailError(err)
	}
	accountingID := parent.request.accountingID()
	x := &call{
		request: requestMetadataForPruning(parent, body), family: wire.SystemOne, mode: "unary",
		actor: parent.actor, keyID: parent.keyID, budgetGroupID: parent.budgetGroupID,
		authority: *current, attribution: parent.attribution, endUserDigest: parent.endUserDigest,
		userID: parent.userID, origin: usage.OriginClassifier, parent: accountingID,
		priority: parent.priority, internalGuardrail: true, authorize: parent.authorize,
		deadline: parent.deadline, spent: parent.spent, facts: slices.Clone(parent.facts),
		routeLeg: routeLeg{route: &route}, workflow: parent.workflow,
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
	authority := cacheAuthority(x.admissionAuthority(x.authority), false, true, false)
	lease, problem := s.Admission.reserveKeyCosted(stepCtx, &authority, x.clientSurface(), keyReservationEstimate(x.estimate, s.dispatchableAttempts(x)), time.Duration(x.named().OverallTimeout)*time.Millisecond, costReservation{}, x.limitRoute())
	if problem != nil {
		return nil, problem
	}
	result := runAttempts(stepCtx, s, x, attemptAdapter[operationplan.Result]{estimate: x.attemptReservation, dispatch: func(attemptCtx context.Context, attempt routing.Attempt, provider *routing.Provider, slot routing.Slot, fact AttemptFact) (AttemptFact, operationplan.Result, *attemptFailure) {
		return s.unaryAttempt(attemptCtx, x, attempt, provider, slot, fact)
	}})
	settleKey(ctx, lease, x.stepDispatched, x.settledTokensFor(x.facts[priorFacts:], x.estimate), s.log)
	// Later steps must include this independently routed attempt in the same run.
	parent.facts, parent.spent = x.facts, x.spent
	if result.err != nil {
		if policy.Revision.OnError == guardrails.FailOpen && (x.sessionDebit == nil || !x.sessionDebit.dispatched) && (result.err.Code == "upstream_unavailable" || result.err.Code == "gateway_timeout") {
			return nil, nil
		}
		return nil, result.err
	}
	var decoded pruningResponse
	if json.Unmarshal(result.result.Body, &decoded) != nil || len(decoded.Answers) != len(candidates) {
		if policy.Revision.OnError == guardrails.FailOpen {
			return nil, nil
		}
		return nil, guardrailError(guardrails.ErrSurface)
	}
	for index := range candidates {
		answer, exists := decoded.Answers[protocols.WorkflowExchangeName(index)]
		if !exists || answer.Type != "noul" || answer.Noul == nil || *answer.Noul < 0 || *answer.Noul > 1 {
			if policy.Revision.OnError == guardrails.FailOpen {
				return nil, nil
			}
			return nil, guardrailError(guardrails.ErrSurface)
		}
	}
	return slices.Collect(func(yield func(int) bool) {
		for index := range candidates {
			if *decoded.Answers[protocols.WorkflowExchangeName(index)].Noul < 0.2 && !yield(index) {
				return
			}
		}
	}), nil
}

func requestMetadataForPruning(parent *call, body []byte) request {
	return request{id: parent.request.id, durableID: parent.request.durableID, minted: parent.request.minted,
		clientIP: parent.request.clientIP, startedAt: parent.request.startedAt, release: parent.request.release,
		delegationAssertion: parent.request.delegationAssertion, callerCredential: parent.request.callerCredential,
		body: &measuredBody{decoded: int64(len(body))}}
}
