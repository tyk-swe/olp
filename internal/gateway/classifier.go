package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

const (
	// classifierText bounds the request text a classifier predicate sends.
	classifierText = 32 << 10
	// pluginPredicateTimeout bounds one plugin predicate call.
	pluginPredicateTimeout = 250 * time.Millisecond
)

// Outcomes of a classifier or plugin predicate that could not decide. Either
// way the selector does not match and the next one is evaluated.
const (
	classifierFailed    = "classifier_failed"
	classifierForbidden = "classifier_forbidden"
	pluginFailed        = "plugin_failed"
)

// classification is a classifier route's answer to one request.
type classification struct {
	label   string
	score   *float64
	failure string
}

// RoutePredicates runs the route_predicate hooks of confined plugins.
type RoutePredicates interface {
	RoutePredicate(ctx context.Context, digest string, input abi.RoutePredicate) (bool, error)
}

// evaluator answers the dynamic predicates of a request's route selectors,
// over the features the selectors already see. A classifier is asked before a
// plugin, and both must match.
func (s *Server) evaluator(ctx context.Context, x *execution, features *runtime.Features) func(runtime.Selector) runtime.PredicateResult {
	return func(selector runtime.Selector) runtime.PredicateResult {
		var result runtime.PredicateResult
		if c := selector.When.Classifier; c != nil {
			answer := s.classify(ctx, x, c)
			if answer.failure != "" {
				return runtime.PredicateResult{Failure: answer.failure}
			}
			result = runtime.PredicateResult{Matched: c.Accepts(answer.label, answer.score), Label: answer.label}
			if !result.Matched {
				return result
			}
		}
		if p := selector.When.Plugin; p != nil {
			verdict := s.pluginPredicate(ctx, x.route.Slug, selector.ID, p, features)
			verdict.Label = result.Label
			return verdict
		}
		return result
	}
}

// pluginPredicate asks a confined plugin whether the selector applies. A
// failure, including a missing or unconfined plugin, falls through to the
// next selector.
func (s *Server) pluginPredicate(ctx context.Context, route, selector string, p *runtime.PluginPredicate, features *runtime.Features) runtime.PredicateResult {
	if s.cfg.Predicates == nil || features == nil {
		return runtime.PredicateResult{Failure: pluginFailed}
	}
	ctx, cancel := context.WithTimeout(ctx, pluginPredicateTimeout)
	defer cancel()
	match, err := s.cfg.Predicates.RoutePredicate(ctx, p.Digest, abi.RoutePredicate{
		Route: route, Selector: selector, Operation: features.Operation,
		InputTokens: features.InputTokens, OutputTokens: features.OutputTokens,
		Streaming: features.Streaming, Tools: features.Tools, Modalities: features.Modalities,
		StructuredOutput: features.StructuredOutput, ReasoningEffort: features.ReasoningEffort,
	})
	if err != nil {
		return runtime.PredicateResult{Failure: pluginFailed}
	}
	return runtime.PredicateResult{Matched: match}
}

// classify asks a classifier route about the caller's request, once per
// route and request however many selectors consult it.
func (s *Server) classify(ctx context.Context, x *execution, c *runtime.ClassifierPredicate) classification {
	if answer, ok := x.classified[c.Route]; ok {
		return answer
	}
	answer := s.callClassifier(ctx, x, c)
	if x.classified == nil {
		x.classified = map[string]classification{}
	}
	x.classified[c.Route] = answer
	return answer
}

// callClassifier sends the text of the caller's last user turn to the
// classifier route as a request of its own: accounted to the caller's key
// with the classifier origin and its own key reservation, bounded by the
// predicate's deadline. The caller is still being planned and has no lease. A
// classification route answers with its highest-scoring label, a generation
// route with its trimmed reply.
func (s *Server) callClassifier(ctx context.Context, parent *execution, c *runtime.ClassifierPredicate) classification {
	route, ok := parent.snapshot().Routes[c.Route]
	switch {
	case !ok || parent.parsed == nil:
		return classification{failure: classifierFailed}
	case parent.authorize != nil && parent.authorize(&route) != nil:
		return classification{failure: classifierForbidden}
	}
	text := protocols.RequestText(parent.parsed, classifierText)
	if text == "" {
		return classification{failure: classifierFailed}
	}
	timeout := time.Duration(min(c.TimeoutMS, route.OverallTimeout)) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	x := &execution{
		request:       request{id: uuid.Must(uuid.NewV7()).String(), minted: true, clientIP: parent.request.clientIP, startedAt: s.now(), release: parent.request.release},
		actor:         parent.actor,
		keyID:         parent.keyID,
		budgetGroupID: parent.budgetGroupID,
		attribution:   parent.attribution,
		userID:        parent.userID,
		authority:     parent.authority,
		affinity:      []byte(parent.keyID),
		origin:        usage.OriginClassifier,
		parent:        parent.request.accountingID(),
		priority:      parent.priority,
		route:         &route,
	}
	defer s.settleAdmission(ctx, x)
	var answer classification
	var out *outcome
	if slices.Contains(route.Operations, "classification") {
		answer, out = s.predict(ctx, x, route.Slug, text)
	} else {
		answer, out = s.generateLabel(ctx, x, route.Slug, text)
	}
	s.finish(x, out, cmp.Or(errorStatus(out.err), 200))
	return answer
}

// prediction is one label of a TEI classification result.
type prediction struct {
	Label string  `json:"label"`
	Score float64 `json:"score"`
}

// predict asks a strict classification route, in the TEI dialect, for the
// labels of the text.
func (s *Server) predict(ctx context.Context, x *execution, slug, text string) (classification, *outcome) {
	failed := classification{failure: classifierFailed}
	codec, _ := operationregistry.Lookup("tei-classification")
	body, _ := json.Marshal(map[string]string{"inputs": text})
	x.unary = &unaryExecution{dialect: codec, route: slug, surface: "native", plans: map[string]*operationplan.Plan{}}
	x.mode = "unary"
	source, err := operationplan.Parse(codec.Identity.ID, body, int(s.cfg.MaxBodyBytes))
	if err != nil {
		return failed, &outcome{err: requestError(err)}
	}
	x.unary.source = source
	if e := s.prepareUnary(ctx, x); e != nil {
		return failed, &outcome{err: e}
	}
	if e := s.admitClassifier(ctx, x); e != nil {
		return failed, &outcome{err: e}
	}
	result := runAttempts(ctx, s, x, attemptAdapter[operationplan.Result]{estimate: x.attemptReservation, dispatch: func(ctx context.Context, a runtime.Attempt, p *runtime.Provider, slot runtime.Slot, n int) (AttemptFact, operationplan.Result, *attemptFailure) {
		return s.unaryAttempt(ctx, x, a, p, slot, n)
	}})
	if result.err != nil {
		return failed, &outcome{err: result.err, committed: result.committed, cancelled: result.cancelled}
	}
	x.facts[len(x.facts)-1].Committed = true
	var predictions []prediction
	if json.Unmarshal(result.result.Body, &predictions) != nil || len(predictions) == 0 {
		return failed, &outcome{committed: true}
	}
	top := slices.MaxFunc(predictions, func(a, b prediction) int { return cmp.Compare(a.Score, b.Score) })
	return classification{label: top.Label, score: &top.Score}, &outcome{committed: true}
}

// generateLabel asks a generation route for a reply to the text and takes the
// reply as the label.
func (s *Server) generateLabel(ctx context.Context, x *execution, slug, text string) (classification, *outcome) {
	failed := classification{failure: classifierFailed}
	body, _ := json.Marshal(map[string]any{"model": slug, "messages": []map[string]string{{"role": "user", "content": text}}})
	parsed, err := protocols.Parse(openai.FamilyChat, body, "")
	if err != nil {
		return failed, &outcome{err: requestError(err)}
	}
	x.family, x.parsed = openai.FamilyChat, parsed
	if e := s.prepare(ctx, x, s.keyAuthorizer(x.authority)); e != nil {
		return failed, &outcome{err: e}
	}
	if e := s.admitClassifier(ctx, x); e != nil {
		return failed, &outcome{err: e}
	}
	out := s.execute(ctx, x)
	if out.err != nil || out.completion == nil {
		return failed, out
	}
	out.committed = true
	x.facts[len(x.facts)-1].Committed = true
	label := strings.TrimSpace(out.completion.OutputText)
	if label == "" {
		return failed, out
	}
	return classification{label: label}, out
}

// admitClassifier budgets the classifier independently of its caller, which
// cannot finish planning until the label is known. It shares the caller's
// admission permit, awaiting it before any upstream work.
func (s *Server) admitClassifier(ctx context.Context, x *execution) *Error {
	if e := s.awaitAdmission(ctx, x); e != nil {
		return e
	}
	x.estimate = requestEstimate(x)
	ttl := time.Duration(x.named().OverallTimeout) * time.Millisecond
	var e *Error
	x.lease, e = s.Admission.reserveKeyCosted(ctx, x.authority, x.clientSurface(), keyReservationEstimate(x.estimate, s.dispatchableAttempts(x)), ttl, s.costReservation(x, x.authority))
	return e
}
