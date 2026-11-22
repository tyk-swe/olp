package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/plugin/abi"
)

func (s *Server) EnterGuardrails(w http.ResponseWriter, r *http.Request) (*Call, bool) {
	req := s.begin(w, r)
	s.cors(w, r)
	if !s.Gate.Admit(r.Context()) {
		fault.WriteError(w, fault.Overloaded)
		return nil, false
	}
	authority, e := s.Gate.Authenticate(r, "guardrails")
	if e != nil {
		fault.WriteError(w, e)
		s.Gate.Release(r.Context())
		return nil, false
	}
	return &Call{request: req, actor: "api_key", keyID: authority.ID, authority: authority, budgetGroupID: authority.BudgetGroupID, attribution: authority.Attribution, family: wire.Chat, operationOverride: "guardrail"}, true
}

func (s *Server) ApplyGuardrails(ctx context.Context, h http.Header, x *Call, in guardrails.Application) (guardrails.AppliedResult, *fault.Error) {
	result, err := s.evaluateGuardrails(ctx, x, in, func(view *routing.View) (guardrails.PolicyRevision, *guardrails.Engine, error) {
		return view.CompileGuardrailPolicy(in.PolicyRevisionID)
	})
	if err != nil && !errors.Is(err, guardrails.ErrBlocked) {
		return guardrails.AppliedResult{}, guardrailError(err)
	}
	x.guard.metadata = x.request.guardrailMetadata
	x.guardrailHeaders(h, false, false)
	return guardrails.Applied(result, err), nil
}

func (s *Server) EvaluateToolGuardrails(ctx context.Context, authority keyauthority.Authority, release *release.Release, engine *guardrails.Engine, requestID string, phase guardrails.Phase, segments []guardrails.Segment, masks *guardrails.Masking) (guardrails.Result, error) {
	if s == nil || release == nil || release.Snapshot() == nil {
		return guardrails.Result{Segments: segments}, guardrails.ErrSurface
	}
	parent := &Call{
		request:       request{id: requestID, minted: true, startedAt: s.Now(), release: release},
		actor:         "api_key",
		keyID:         authority.ID,
		budgetGroupID: authority.BudgetGroupID,
		authority:     authority,
		attribution:   authority.Attribution,
		endUserDigest: authority.EndUserDigest,
		family:        wire.Chat,
	}
	evaluation := guardrails.Evaluation{RequestID: parent.request.accountingID(), Phase: phase, Segments: segments}
	return s.boundGuardrails(parent, engine).Evaluate(ctx, evaluation, masks)
}

func (s *Server) EvaluateGuardrails(r *http.Request, member Member, pinned guardrails.Pinned, in guardrails.Application) (guardrails.Result, error) {
	if err := s.admitGuardrailEvaluation(r); err != nil {
		return guardrails.Result{}, err
	}
	defer s.Gate.Release(r.Context())
	return s.evaluatePinnedGuardrails(r, member, pinned, in)
}

func (s *Server) EvaluateGuardrailsBatch(r *http.Request, member Member, batch []guardrails.PinnedApplication) ([]guardrails.Result, error) {
	if err := s.admitGuardrailEvaluation(r); err != nil {
		return nil, err
	}
	defer s.Gate.Release(r.Context())
	results := make([]guardrails.Result, len(batch))
	for i, item := range batch {
		result, err := s.evaluatePinnedGuardrails(r, member, item.Pinned, item.Application)
		if err != nil && !errors.Is(err, guardrails.ErrBlocked) {
			return nil, err
		}
		results[i] = result
	}
	return results, nil
}

func (s *Server) admitGuardrailEvaluation(r *http.Request) error {
	if s == nil || s.Deps.Runtime == nil {
		return fault.ServerError(http.StatusServiceUnavailable, "guardrails_unavailable", "The evaluator is unavailable.")
	}
	if !s.Gate.Admit(r.Context()) {
		return fault.Overloaded
	}
	return nil
}

func (s *Server) evaluatePinnedGuardrails(r *http.Request, member Member, pinned guardrails.Pinned, in guardrails.Application) (guardrails.Result, error) {
	x := &Call{request: request{id: uuid.Must(uuid.NewV7()).String(), minted: true, startedAt: s.Now(), release: s.Deps.Runtime.Release()}, actor: "playground", origin: usage.OriginPlayground, userID: member.UserID, family: wire.Chat, operationOverride: "guardrail"}
	x.authorize = member.Admit
	result, err := s.evaluateGuardrails(r.Context(), x, in, func(*routing.View) (guardrails.PolicyRevision, *guardrails.Engine, error) {
		return pinned.Compile()
	})
	s.SettleAdmission(context.WithoutCancel(r.Context()), x)
	if err == nil || errors.Is(err, guardrails.ErrBlocked) {
		s.Finish(x, &Outcome{}, 200)
		return result, err
	}
	failure := guardrailError(err)
	s.Finish(x, &Outcome{Err: failure}, failure.Status)
	return result, failure
}

func (s *Server) evaluateGuardrails(ctx context.Context, x *Call, in guardrails.Application, policy func(*routing.View) (guardrails.PolicyRevision, *guardrails.Engine, error)) (guardrails.Result, error) {
	x.Route = &routing.Route{Slug: "guardrails", OverallTimeout: 30000, MaxAttempts: 1}
	x.primary = x.Route
	x.mode = "unary"
	if x.request.release == nil {
		return guardrails.Result{}, guardrails.ErrSurface
	}
	p, compiled, err := policy(x.Snapshot())
	if err != nil {
		return guardrails.Result{}, err
	}
	if x.keyID != "" && (x.authority.ProjectID == nil || *x.authority.ProjectID != p.ProjectID) {
		return guardrails.Result{}, guardrails.ErrSurface
	}
	engine := s.boundGuardrails(x, compiled)
	x.guard.engine = engine
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if x.keyID != "" {
		var n int64
		for _, segment := range in.Segments {
			n += int64(len(segment.Text))
		}
		x.estimate = max(1, (n+3)/4)
		lease, e := s.Deps.Admission.ReserveKey(ctx, x.authority, "openai", x.estimate, 30*time.Second, "guardrails")
		if e != nil {
			return guardrails.Result{}, e
		}
		x.lease = lease
	}
	x.dispatched = true
	result, err := engine.Evaluate(ctx, guardrails.Evaluation{RequestID: x.request.accountingID(), Phase: in.Phase, Segments: in.Segments}, &guardrails.Masking{})
	x.guard.record(result.Decisions)
	return result, err
}

func (s *Server) modelGuardrail(ctx context.Context, parent *Call, d guardrails.Revision, spec guardrails.ModelSpec, e guardrails.Evaluation) (verdict guardrails.Verdict, err error) {
	if parent.guard.internal || parent.request.release == nil {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	// Candidate preparation can see identical effective input for several
	// targets. Charge one evaluator request for each distinct synchronous input.
	if e.Phase == guardrails.Input {
		raw, _ := json.Marshal(struct {
			Revision string
			Segments []guardrails.Segment
		}{d.ID, e.Segments})
		key := sha256.Sum256(raw)
		if value, ok := parent.guard.models[key]; ok {
			return value, nil
		}
		defer func() {
			if err == nil {
				if parent.guard.models == nil {
					parent.guard.models = map[[32]byte]guardrails.Verdict{}
				}
				parent.guard.models[key] = verdict
			}
		}()
	}
	route, ok := s.authorizeModelEvaluatorRoute(parent, spec.Route)
	if !ok {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	similarity, semantic := spec.Judge.(guardrails.SemanticRule)
	rule, _ := spec.Judge.(guardrails.ModelRule)
	if semantic {
		return s.runSemanticGuardrail(ctx, parent, d, &route, similarity, e)
	}
	segments, _ := json.Marshal(e.Segments)
	text := rule.Prompt + "\n" + string(segments)
	return s.runRuleModelGuardrail(ctx, parent, d, &route, rule, text)
}

func (s *Server) authorizeModelEvaluatorRoute(parent *Call, slug string) (routing.Route, bool) {
	// A retained serving snapshot contains only its pinned route/provider.
	// Evaluators belong to the full release pinned at request admission.
	route, ok := parent.request.release.Snapshot().FindRoute(slug)
	if !ok {
		return routing.Route{}, false
	}
	if parent.authorize != nil && parent.authorize(&route) != nil {
		return routing.Route{}, false
	}
	if parent.keyID != "" && !parent.authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) {
		return routing.Route{}, false
	}
	return route, true
}

func (s *Server) newEvaluatorCall(parent *Call, d guardrails.Revision, route *routing.Route) *Call {
	origin := usage.OriginClassifier
	if parent.keyID == "" {
		origin = usage.OriginPlayground
	}
	return &Call{
		request:              request{id: uuid.Must(uuid.NewV7()).String(), minted: true, clientIP: parent.request.clientIP, startedAt: s.Now(), release: parent.request.release},
		actor:                parent.actor,
		keyID:                parent.keyID,
		budgetGroupID:        parent.budgetGroupID,
		authority:            parent.authority,
		attribution:          parent.attribution,
		endUserDigest:        parent.endUserDigest,
		userID:               parent.userID,
		origin:               origin,
		parent:               parent.request.accountingID(),
		priority:             parent.priority,
		guard:                guardrailCall{internal: true},
		evaluatorGuardrailID: d.GuardrailID,
		authorize:            parent.authorize,
		routeLeg:             routeLeg{Route: route},
	}
}

func (s *Server) runSemanticGuardrail(ctx context.Context, parent *Call, d guardrails.Revision, route *routing.Route, similarity guardrails.SemanticRule, e guardrails.Evaluation) (guardrails.Verdict, error) {
	if !slices.Contains(route.Operations, "embeddings") {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	embeddingInputs, err := guardrails.SemanticInputs(e, similarity)
	if err != nil || len(embeddingInputs) == 0 {
		return guardrails.Verdict{}, err
	}
	x := s.newEvaluatorCall(parent, d, route)
	defer s.SettleAdmission(context.WithoutCancel(ctx), x)
	body, _ := json.Marshal(map[string]any{"model": route.Slug, "input": embeddingInputs, "encoding_format": "float"})
	x.request.body = &ingress.MeasuredBody{Decoded: int64(len(body))}
	x.family, x.mode = wire.Embeddings, "unary"
	raw, out, surfaceErr := s.executeSemanticEmbeddings(ctx, x, route, body)
	if surfaceErr != nil {
		return guardrails.Verdict{}, surfaceErr
	}
	s.Finish(x, out, max(200, errorStatus(out.Err)))
	if out.Err != nil {
		return guardrails.Verdict{}, out.Err
	}
	return guardrails.SemanticVerdict(raw, similarity, len(embeddingInputs)-len(similarity.Utterances))
}

func (s *Server) executeSemanticEmbeddings(ctx context.Context, x *Call, route *routing.Route, body []byte) ([]byte, *Outcome, error) {
	if route.Fidelity.Strict() {
		codec, _ := operationregistry.Lookup("openai-embeddings")
		source, parseErr := operationplan.Parse("openai-embeddings", body, int(s.Config.MaxBodyBytes))
		if parseErr != nil {
			return nil, nil, guardrails.ErrSurface
		}
		x.unary = &unaryCall{source: source, dialect: codec, route: route.Slug, surface: "openai"}
		if prepareErr := s.prepareUnary(ctx, x); prepareErr != nil {
			return nil, &Outcome{Err: prepareErr}, nil
		}
		embeddingCtx, cancel := x.routeContext(ctx)
		defer cancel()
		if admissionErr := s.admitClassifier(embeddingCtx, x); admissionErr != nil {
			return nil, &Outcome{Err: admissionErr}, nil
		}
		result := runAttempts(embeddingCtx, s, x, attemptAdapter[operationplan.Result]{demand: x.attemptDemand, dispatch: func(attemptCtx context.Context, a routing.Attempt, p *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
			return s.unaryAttempt(attemptCtx, x, a, p, slot, fact)
		}})
		return result.result.Body, &Outcome{Err: result.err, Committed: result.committed, Cancelled: result.cancelled}, nil
	}
	parsed, parseErr := protocols.Parse(wire.Embeddings, body, "")
	if parseErr != nil {
		return nil, nil, guardrails.ErrSurface
	}
	x.generation.parsed = parsed
	if prepareErr := s.prepare(ctx, x, guardrailAuthorizer(s, x)); prepareErr != nil {
		return nil, &Outcome{Err: prepareErr}, nil
	}
	if admissionErr := s.admitClassifier(ctx, x); admissionErr != nil {
		return nil, &Outcome{Err: admissionErr}, nil
	}
	out := s.execute(ctx, x)
	if out.Completion != nil {
		return out.Completion.Body, out, nil
	}
	return nil, out, nil
}

func (s *Server) runRuleModelGuardrail(ctx context.Context, parent *Call, d guardrails.Revision, route *routing.Route, rule guardrails.ModelRule, text string) (guardrails.Verdict, error) {
	x := s.newEvaluatorCall(parent, d, route)
	defer s.SettleAdmission(context.WithoutCancel(ctx), x)
	raw, out, err := s.executeRuleEvaluator(ctx, x, route, text)
	if err != nil {
		return guardrails.Verdict{}, err
	}
	s.Finish(x, out, max(200, errorStatus(out.Err)))
	if out.Err != nil {
		return guardrails.Verdict{}, out.Err
	}
	return guardrails.ModelVerdict(raw, rule)
}

func (s *Server) executeRuleEvaluator(ctx context.Context, x *Call, route *routing.Route, text string) ([]byte, *Outcome, error) {
	if slices.Contains(route.Operations, "classification") || slices.Contains(route.Operations, "moderation") {
		dialect := "tei-classification"
		body, _ := json.Marshal(map[string]string{"inputs": text})
		if slices.Contains(route.Operations, "moderation") {
			dialect = "openai-moderation"
			body, _ = json.Marshal(map[string]string{"model": route.Slug, "input": text})
		}
		codec, _ := operationregistry.Lookup(dialect)
		source, parseErr := operationplan.Parse(dialect, body, int(s.Config.MaxBodyBytes))
		if parseErr != nil {
			return nil, nil, guardrails.ErrSurface
		}
		x.unary = &unaryCall{source: source, dialect: codec, route: route.Slug, surface: codec.Surface}
		x.mode = "unary"
		if e := s.prepareUnary(ctx, x); e != nil {
			return nil, &Outcome{Err: e}, nil
		}
		if e := s.admitClassifier(ctx, x); e != nil {
			return nil, &Outcome{Err: e}, nil
		}
		result := runAttempts(ctx, s, x, attemptAdapter[operationplan.Result]{demand: x.attemptDemand, dispatch: func(ctx context.Context, a routing.Attempt, p *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
			return s.unaryAttempt(ctx, x, a, p, slot, fact)
		}})
		return result.result.Body, &Outcome{Err: result.err, Committed: result.committed, Cancelled: result.cancelled}, nil
	}
	if !slices.Contains(route.Operations, "generation") {
		return nil, nil, guardrails.ErrSurface
	}
	body, _ := json.Marshal(map[string]any{"model": route.Slug, "messages": []map[string]string{{"role": "user", "content": text}}})
	parsed, err := protocols.Parse(wire.Chat, body, "")
	if err != nil {
		return nil, nil, err
	}
	x.family, x.generation.parsed = wire.Chat, parsed
	if e := s.prepare(ctx, x, guardrailAuthorizer(s, x)); e != nil {
		return nil, &Outcome{Err: e}, nil
	}
	if e := s.admitClassifier(ctx, x); e != nil {
		return nil, &Outcome{Err: e}, nil
	}
	out := s.execute(ctx, x)
	if out.Completion != nil {
		return []byte(strings.TrimSpace(out.Completion.OutputText)), out, nil
	}
	return nil, out, nil
}

func (s *Server) pluginGuardrail(ctx context.Context, d guardrails.Revision, spec guardrails.PluginSpec, e guardrails.Evaluation) (guardrails.Verdict, error) {
	host, ok := s.Config.Predicates.(interface {
		Guardrail(context.Context, string, abi.GuardrailRequest) (abi.GuardrailVerdict, error)
	})
	if !ok {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	in := abi.GuardrailRequest{Guardrail: d.Name, Phase: string(e.Phase), RequestID: e.RequestID, Route: e.Route, Segments: make([]abi.GuardrailSegment, len(e.Segments))}
	for i, segment := range e.Segments {
		in.Segments[i] = abi.GuardrailSegment{ID: segment.ID, Role: string(segment.Role), Text: segment.Text, Kind: string(segment.Kind), ToolName: segment.ToolName, Untrusted: segment.Untrusted}
	}
	verdict, err := host.Guardrail(ctx, spec.Version, in)
	if err != nil {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	out := guardrails.WireVerdict{Action: verdict.Action, Categories: verdict.Categories, Redactions: make([]guardrails.Redaction, len(verdict.Redactions))}
	for i, r := range verdict.Redactions {
		out.Redactions[i] = guardrails.Redaction{Segment: r.Segment, Start: r.Start, End: r.End, Replacement: r.Replacement}
	}
	return out.Verdict()
}
