package gateway

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations/decision"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

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
	pruneCtx, cancel := context.WithTimeout(ctx, time.Duration(policies[0].Revision.TimeoutMS)*time.Millisecond)
	defer cancel()
	var selected []int
	for start := 0; start < len(candidates); start += 256 {
		batch, problem := s.typeSafeRelevance(pruneCtx, x, policies[0], request, candidates[start:min(start+256, len(candidates))])
		if problem != nil {
			return nil, problem
		}
		for _, index := range batch {
			selected = append(selected, start+index)
		}
	}
	pruned, err := plan.Apply(selected)
	if err != nil {
		return nil, guardrailError(err)
	}
	return s.pruneWorkflowPolicies(ctx, x, pruned, policies[1:])
}

func (s *Server) typeSafeRelevance(ctx context.Context, parent *call, policy guardrails.TypeSafePolicy, request *wire.Request, candidates []protocols.WorkflowExchange) ([]int, *Error) {
	if parent.generation.runLimits.MaxRunCost != nil {
		return nil, workflowUnboundedCost()
	}
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
		Task      string                                `json:"task"`
		System    []string                              `json:"system,omitempty"`
		Exchanges map[string]protocols.WorkflowExchange `json:"exchanges"`
	}{Task: protocols.WorkflowPruningTask(request, guardrails.MaxContentBytes), System: slices.Collect(func(yield func(string) bool) {
		for _, segment := range parent.guardrailInput {
			if segment.Role == "system" && !yield(segment.Text) {
				return
			}
		}
	}), Exchanges: maps.Collect(func(yield func(string, protocols.WorkflowExchange) bool) {
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
		first, problem := s.typeSafeRelevance(ctx, parent, policy, request, candidates[:middle])
		if problem != nil {
			return nil, problem
		}
		second, problem := s.typeSafeRelevance(ctx, parent, policy, request, candidates[middle:])
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
		unary: &unaryCall{source: source, dialect: codec, route: route.Slug, surface: wire.SystemOne.Surface()},
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
