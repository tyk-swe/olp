package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/domain/contentpolicy"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operationregistry"
	"github.com/tyk-swe/olp/internal/domain/operations"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/guardstream"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/plugin/abi"
)

var errGuardrailCredentials = errors.New("guardrail credentials unavailable")

func guardrailRunner(deps Dependencies) func(guardrails.Caller) guardrails.Evaluator {
	if deps.Guardrails != nil {
		return deps.Guardrails
	}
	runner := &guardrails.ExternalRunner{Egress: *deps.Egress, Clients: egress.NewConnectionClientCache(128)}
	return runner.For
}

func (s *Server) boundGuardrails(x *Call, engine *guardrails.Engine) *guardrails.Engine {
	return engine.Bind(s.Deps.Guardrails(guardrails.Caller{
		Credential: func(_ context.Context, id string) ([]byte, error) {
			if x.request.release == nil {
				return nil, errGuardrailCredentials
			}
			secret, ok := x.request.release.Credential(id)
			if !ok {
				return nil, errGuardrailCredentials
			}
			return secret, nil
		},
		Model: func(ctx context.Context, d guardrails.Revision, spec guardrails.ModelSpec, e guardrails.Evaluation) (guardrails.Verdict, error) {
			return s.modelGuardrail(ctx, x, d, spec, e)
		},
		Plugin: s.pluginGuardrail,
	}))
}

const errContentPolicyBlocked guardrails.Failure = "content_policy_blocked"

func guardrailError(err error) *fault.Error {
	if err == nil {
		return nil
	}
	var existing *fault.Error
	if errors.As(err, &existing) {
		return existing
	}
	var failure guardrails.Failure
	if errors.As(err, &failure) {
		return &fault.Error{Status: guardrailStatus(failure), Type: "invalid_request_error", Code: string(failure), Message: "The request could not satisfy the configured guardrails."}
	}
	return fault.ServerError(500, "guardrail_unavailable", "The guardrail could not be evaluated.")
}

func guardrailStatus(failure guardrails.Failure) int {
	if failure == guardrails.ErrSurface {
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadRequest
}

func (s *Server) bindGuardrails(ctx context.Context, x *Call) *fault.Error {
	x.guard.metadata = x.request.guardrailMetadata
	x.guard.engine = s.boundGuardrails(x, x.Snapshot().GuardrailEngine(x.Route.Slug, x.keyID))
	if x.guard.internal {
		x.guard.engine = x.guard.engine.WithoutModelGuardrails()
	}
	if x.guard.engine == nil {
		return nil
	}
	// Unary operation codecs expose inspection text but cannot apply rewritten
	// segments to their typed request and result documents.
	if x.unary != nil {
		for _, phase := range []guardrails.Phase{guardrails.Input, guardrails.Output, guardrails.OutputStream} {
			if x.guard.engine.HasAction(phase, guardrails.Redact) || x.guard.engine.HasAction(phase, guardrails.Mask) {
				return guardrailError(guardrails.ErrSurface)
			}
		}
	}
	x.guard.ctx = ctx
	if x.guard.masks == nil {
		x.guard.masks = &guardrails.Masking{}
	}
	// Retained content is read directly from the provider, outside this
	// request's output inspection and masking lifetime.
	retainsOutput := x.providerState && (responseStoreRequested(x.generation.parsed) || backgroundResponseRequested(x.generation.parsed))
	opaqueInput := false
	if x.generation.resources != nil {
		for _, resource := range x.generation.resources.bound {
			if resource == nil {
				continue
			}
			if resource.Kind == "conversation" {
				retainsOutput = true
			}
			// Prior Responses retain their existing continuation contract.
			// Newly supplied provider resources have no content inspection path.
			if resource.Kind != resources.KindResponse && resource.Kind != resources.KindStrictResponse {
				opaqueInput = true
			}
		}
	}
	if opaqueInput {
		for _, phase := range []guardrails.Phase{guardrails.Input, guardrails.InputParallel} {
			result, err := x.guard.engine.Unavailable(phase)
			x.guard.record(result.Decisions)
			if err != nil {
				return guardrailError(err)
			}
		}
	}
	if retainsOutput && (x.guard.engine.HasPhase(guardrails.Output) || x.guard.engine.HasPhase(guardrails.OutputStream) || x.guard.engine.HasAction(guardrails.Input, guardrails.Mask)) {
		return guardrailError(guardrails.ErrSurface)
	}
	if x.generation.parsed != nil && x.generation.parsed.Stream {
		if err := x.guardrailStreamSurface(x.generation.parsed.OIF().Document()); err != nil {
			return guardrailError(err)
		}
		if err := x.guard.engine.ValidateStream(x.Snapshot().GuardrailWindow()); err != nil {
			if x.Route.ContentPolicy != nil {
				for _, rule := range x.Route.ContentPolicy.Rules {
					if rule.Phase == contentpolicy.PhaseOutput {
						return PolicyUnavailable("content_policy_streaming_requires_unary", "This migrated output policy requires a buffered unary response.")
					}
				}
			}
			return guardrailError(err)
		}
	}
	return nil
}

func (x *Call) guardrailStreamSurface(document oif.Document) error {
	if x.family == wire.Completions && (x.guard.engine.HasAction(guardrails.Output, guardrails.Block) || x.guard.engine.HasAction(guardrails.Output, guardrails.Redact) || x.guard.engine.HasAction(guardrails.Input, guardrails.Mask)) {
		if value, ok := document.Root().Lookup("logprobs"); ok && value.Kind() != oif.Null {
			return guardrails.ErrSurface
		}
	}
	if (x.family == wire.Chat || x.family == wire.Completions) && x.generation.parsed != nil && x.generation.parsed.Stream &&
		(x.guard.engine.HasPhase(guardrails.OutputStream) || x.guard.engine.HasAction(guardrails.Input, guardrails.Mask)) {
		// Log-probability alternatives and their byte encodings are separate
		// output surfaces that cannot share the content delta's stream window.
		if value, ok := document.Root().Lookup("logprobs"); ok && (value.Raw() == "true" || x.family == wire.Completions && value.Kind() != oif.Null) {
			return guardrails.ErrSurface
		}
	}
	return nil
}

func (x *Call) guardrailStream() (*guardstream.Stream, error) {
	return guardstream.New(guardstream.Config{
		Engine: x.guard.engine, Masks: x.guard.masks, Input: x.guard.input,
		RequestID: x.request.accountingID(), Route: x.Route.Slug, InternalModel: x.guard.internal,
		Family: x.family, Window: x.Snapshot().GuardrailWindow(), Strict: x.Strict(),
		ToolContext: x.guard.ctx, Record: x.guard.record,
	})
}

func (x *Call) inspectGuardrailInput(document oif.Document) (oif.Document, []guardrails.Segment, error) {
	return x.inspectGuardrailInputWithRecovery(x.guard.ctx, document, true)
}

type guardrailInput struct {
	document  oif.Document
	segments  []guardrails.Segment
	decisions []guardrails.Evidence
}

func (x *Call) inspectGuardrailInputWithRecovery(ctx context.Context, document oif.Document, recoverable bool) (oif.Document, []guardrails.Segment, error) {
	result, err := x.evaluateGuardrailInput(ctx, document, recoverable)
	x.guard.record(result.decisions)
	return result.document, result.segments, err
}

func (x *Call) evaluateGuardrailInput(ctx context.Context, document oif.Document, recoverable bool) (guardrailInput, error) {
	// Effective provider defaults may enable an unsupported stream surface.
	if err := x.guardrailStreamSurface(document); err != nil {
		return guardrailInput{document: document}, err
	}
	surface, err := guardrails.NativeSurface(document, false)
	if err != nil {
		result, e := x.guard.engine.Unavailable()
		return guardrailInput{document: document, decisions: result.Decisions}, e
	}
	evaluation := guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.Route.Slug, Phase: guardrails.Input, Segments: surface.Segments, InternalModel: x.guard.internal, WorkflowRecovery: recoverable && x.workflow != nil}
	evaluation.CompressionExemptIDs = slices.Collect(func(yield func(string) bool) {
		for _, segment := range surface.Segments {
			if segment.Kind == guardrails.ToolResult && slices.Contains(x.workflowRecoveryCalls, segment.ToolCallID) && !yield(segment.ID) {
				return
			}
		}
	})
	if x.Strict() && x.guard.engine.HasLegacyInput() {
		texts := contentpolicy.InputTexts(document)
		evaluation.LegacyInput = make([]guardrails.Segment, len(texts))
		for i, text := range texts {
			evaluation.LegacyInput[i] = guardrails.Segment{ID: "legacy_" + strconv.Itoa(i), Text: text}
		}
	}
	result, err := x.guard.engine.Evaluate(ctx, evaluation, x.guard.masks)
	if recoverable && x.workflow != nil && len(result.Recovery) > 0 {
		x.workflowRecovery = result.Recovery
	}
	err = x.legacyGuardrailError(result, err)
	if err != nil {
		return guardrailInput{document: document, segments: result.Segments, decisions: result.Decisions}, err
	}
	if result.Changed {
		document, err = surface.Apply(result.Segments)
	}
	return guardrailInput{document: document, segments: result.Segments, decisions: result.Decisions}, err
}
func (x *Call) inspectOperationInput(texts []operations.Text) ([]guardrails.Evidence, error) {
	segments := make([]guardrails.Segment, len(texts))
	for i, t := range texts {
		segments[i] = guardrails.Segment{ID: "s" + strconv.Itoa(i), Text: t.Value}
	}
	result, err := x.guard.engine.Evaluate(x.guard.ctx, guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.Route.Slug, Phase: guardrails.Input, Segments: segments, InternalModel: x.guard.internal}, x.guard.masks)
	x.guard.input = segments
	return result.Decisions, x.legacyGuardrailError(result, err)
}
func (x *Call) inspectOperationOutput(texts []operations.Text) error {
	segments := make([]guardrails.Segment, len(texts))
	for i, text := range texts {
		segments[i] = guardrails.Segment{ID: "s" + strconv.Itoa(i), Text: text.Value}
	}
	result, err := x.guard.engine.Evaluate(x.guard.ctx, guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.Route.Slug, Phase: guardrails.Output, Segments: segments, InternalModel: x.guard.internal}, nil)
	x.guard.record(result.Decisions)
	return x.legacyGuardrailError(result, err)
}
func (x *Call) inspectGuardrailOutput(body []byte) ([]byte, error) {
	if !x.guard.engine.HasPhase(guardrails.Output) && !x.guard.engine.HasAction(guardrails.Input, guardrails.Mask) {
		return body, nil
	}
	doc, err := oif.ParseJSON(body, oif.Limits{MaxBytes: guardrails.MaxContentBytes})
	if err != nil {
		return body, guardrails.ErrSurface
	}
	if x.family == wire.Completions {
		choices, _ := doc.Root().Lookup("choices")
		for _, choice := range choices.Elements() {
			if value, ok := choice.Lookup("logprobs"); ok && value.Kind() != oif.Null {
				result, err := x.guard.engine.Unavailable(guardrails.Output)
				x.guard.record(result.Decisions)
				if err != nil {
					return body, err
				}
				if x.guard.engine.HasAction(guardrails.Input, guardrails.Mask) {
					return body, guardrails.ErrSurface
				}
			}
		}
	}
	if x.Strict() && x.guard.engine.HasLegacyOutput() {
		if err := interaction.CheckLegacyOutputCoverage(x.family, doc); err != nil {
			result, _ := x.guard.engine.Unavailable(guardrails.Output)
			x.guard.record(result.Decisions)
			return body, &responseLocalRefusal{cause: err}
		}
	}
	surface, err := guardrails.NativeSurface(doc, true)
	if err != nil {
		result, e := x.guard.engine.Unavailable(guardrails.Output)
		x.guard.record(result.Decisions)
		return body, e
	}
	changed := false
	segments := slices.Clone(surface.Segments)
	for i := range segments {
		value := x.guard.masks.Restore(segments[i].Text)
		if value != segments[i].Text {
			segments[i].Text = value
			changed = true
		}
	}
	result, err := x.guard.engine.Evaluate(x.guard.ctx, guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.Route.Slug, Phase: guardrails.Output, Segments: slices.Concat(segments, guardrails.InputProvenance(x.guard.input)), InternalModel: x.guard.internal}, x.guard.masks)
	x.guard.record(result.Decisions)
	err = x.legacyGuardrailError(result, err)
	if err != nil {
		return body, err
	}
	if result.Changed || changed {
		out, err := surface.Apply(result.Segments[:len(surface.Segments)])
		if err != nil {
			return body, err
		}
		return out.Bytes(), nil
	}
	return body, nil
}

// parallelGuardrail owns its result channel until it joins, including when an
// upstream returns before the detector. No goroutine mutates request evidence.
type parallelGuardrail struct {
	result   chan guardrailResult
	cancel   context.CancelFunc
	finished bool
	err      error
}
type guardrailResult struct {
	result guardrails.Result
	err    error
}

func (s *Server) startParallel(ctx context.Context, x *Call, cancelUpstream context.CancelFunc) *parallelGuardrail {
	if !x.guard.engine.HasPhase(guardrails.InputParallel) {
		return nil
	}
	if x.cache != nil && x.cache.parallelEngine == x.guard.engine && slices.Equal(x.cache.parallelInput, x.guard.input) {
		return nil
	}
	evalctx, cancel := context.WithCancel(ctx)
	p := &parallelGuardrail{result: make(chan guardrailResult, 1), cancel: cancel}
	request := guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.Route.Slug, Phase: guardrails.InputParallel, Segments: slices.Clone(x.guard.input), InternalModel: x.guard.internal}
	go func() {
		defer panics.Recover(s.Log, "parallel guardrail", func(err error) {
			p.result <- guardrailResult{err: err}
			cancelUpstream()
		})
		result, err := x.guard.engine.Evaluate(evalctx, request, nil)
		p.result <- guardrailResult{result, err}
		if err != nil {
			cancelUpstream()
		}
	}()
	return p
}
func (p *parallelGuardrail) join(x *Call, wait bool) error {
	if p == nil || p.finished {
		return nilIfParallel(p)
	}
	var result guardrailResult
	if wait {
		result = <-p.result
	} else {
		select {
		case result = <-p.result:
		default:
			return nil
		}
	}
	p.finished = true
	p.err = result.err
	x.guard.record(result.result.Decisions)
	return p.err
}
func nilIfParallel(p *parallelGuardrail) error {
	if p == nil {
		return nil
	}
	return p.err
}

func (s *Server) applyGuardrails(w http.ResponseWriter, r *http.Request) {
	req := s.begin(w, r)
	s.cors(w, r)
	if !s.Gate.Admit(r.Context()) {
		fault.WriteError(w, fault.Overloaded)
		return
	}
	defer s.Gate.Release(r.Context())
	authority, e := s.Gate.Authenticate(r, "guardrails")
	if e != nil {
		fault.WriteError(w, e)
		return
	}
	var in guardrails.Application
	if err := httpkit.DecodeUnique(r, &in, guardrails.MaxContentBytes); err != nil {
		fault.WriteError(w, fault.InvalidRequest("invalid_request", "Supply a policy revision, phase and bounded text segments.", nil))
		return
	}
	x := &Call{request: req, actor: "api_key", keyID: authority.ID, authority: authority, budgetGroupID: authority.BudgetGroupID, attribution: authority.Attribution, family: wire.Chat, operationOverride: "guardrail"}
	result, err := s.evaluateGuardrails(r.Context(), x, in, func(view *routing.View) (guardrails.PolicyRevision, *guardrails.Engine, error) {
		return view.CompileGuardrailPolicy(in.PolicyRevisionID)
	})
	status := 200
	out := &Outcome{}
	if err != nil && !errors.Is(err, guardrails.ErrBlocked) {
		out.Err = guardrailError(err)
		status = out.Err.Status
		fault.WriteError(w, out.Err)
	} else {
		x.guard.metadata = req.guardrailMetadata
		x.guardrailHeaders(w.Header(), false, false)
		fault.WriteJSON(w, guardrails.Applied(result, err))
	}
	s.settleAdmission(context.WithoutCancel(r.Context()), x)
	s.Finish(x, out, status)
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

func (s *Server) EvaluateGuardrails(r *http.Request, p authz.Principal, pinned guardrails.Pinned, in guardrails.Application) (guardrails.Result, error) {
	if s == nil || s.Deps.Runtime == nil {
		return guardrails.Result{}, httpkit.Fail(503, "guardrails_unavailable", "The evaluator is unavailable.")
	}
	if !s.Gate.Admit(r.Context()) {
		return guardrails.Result{}, httpkit.Fail(fault.Overloaded.Status, fault.Overloaded.Code, fault.Overloaded.Message)
	}
	defer s.Gate.Release(r.Context())
	x := &Call{request: request{id: uuid.Must(uuid.NewV7()).String(), minted: true, startedAt: s.Now(), release: s.Deps.Runtime.Release()}, actor: "playground", origin: usage.OriginPlayground, userID: p.UserID(), family: wire.Chat, operationOverride: "guardrail"}
	x.authorize = func(route *routing.Route) *fault.Error {
		if err := p.Authorize(authz.Playground); err != nil {
			return fault.PermissionError("guardrail_forbidden", "Playground access is required.")
		}
		if err := p.Project(route.ProjectID, authz.View); err != nil {
			return fault.ModelNotFound(route.Slug)
		}
		return nil
	}
	result, err := s.evaluateGuardrails(r.Context(), x, in, func(*routing.View) (guardrails.PolicyRevision, *guardrails.Engine, error) {
		return pinned.Compile()
	})
	s.settleAdmission(context.WithoutCancel(r.Context()), x)
	if err == nil || errors.Is(err, guardrails.ErrBlocked) {
		s.Finish(x, &Outcome{}, 200)
		return result, err
	}
	failure := guardrailError(err)
	s.Finish(x, &Outcome{Err: failure}, failure.Status)
	return result, httpkit.Fail(failure.Status, failure.Code, "The guardrail could not be evaluated.")
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
	similarity, semantic := spec.Judge.(guardrails.SemanticRule)
	rule, _ := spec.Judge.(guardrails.ModelRule)
	// A retained serving snapshot contains only its pinned route/provider.
	// Evaluators belong to the full release pinned at request admission.
	route, ok := parent.request.release.Snapshot().FindRoute(spec.Route)
	if !ok {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	if parent.authorize != nil && parent.authorize(&route) != nil {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	if parent.keyID != "" && !parent.authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	var text string
	var embeddingInputs []string
	if semantic {
		if !slices.Contains(route.Operations, "embeddings") {
			return guardrails.Verdict{}, guardrails.ErrSurface
		}
		embeddingInputs, err = guardrails.SemanticInputs(e, similarity)
		if err != nil {
			return guardrails.Verdict{}, err
		}
		if len(embeddingInputs) == 0 {
			return guardrails.Verdict{}, nil
		}
	} else {
		segments, _ := json.Marshal(e.Segments)
		text = rule.Prompt + "\n" + string(segments)
	}
	x := &Call{
		request:              request{id: uuid.Must(uuid.NewV7()).String(), minted: true, clientIP: parent.request.clientIP, startedAt: s.Now(), release: parent.request.release},
		actor:                parent.actor,
		keyID:                parent.keyID,
		budgetGroupID:        parent.budgetGroupID,
		authority:            parent.authority,
		attribution:          parent.attribution,
		endUserDigest:        parent.endUserDigest,
		userID:               parent.userID,
		origin:               usage.OriginClassifier,
		parent:               parent.request.accountingID(),
		priority:             parent.priority,
		guard:                guardrailCall{internal: true},
		evaluatorGuardrailID: d.GuardrailID,
		authorize:            parent.authorize,
		routeLeg:             routeLeg{Route: &route},
	}
	if parent.keyID == "" {
		x.origin = usage.OriginPlayground
	}
	defer s.settleAdmission(context.WithoutCancel(ctx), x)
	var raw []byte
	var out *Outcome
	if semantic {
		body, _ := json.Marshal(map[string]any{"model": route.Slug, "input": embeddingInputs, "encoding_format": "float"})
		x.request.body = &ingress.MeasuredBody{Decoded: int64(len(body))}
		x.family, x.mode = wire.Embeddings, "unary"
		if route.Fidelity.Strict() {
			codec, _ := operationregistry.Lookup("openai-embeddings")
			source, parseErr := operationplan.Parse("openai-embeddings", body, int(s.Config.MaxBodyBytes))
			if parseErr != nil {
				return guardrails.Verdict{}, guardrails.ErrSurface
			}
			x.unary = &unaryCall{source: source, dialect: codec, route: route.Slug, surface: "openai"}
			if prepareErr := s.prepareUnary(ctx, x); prepareErr != nil {
				out = &Outcome{Err: prepareErr}
			} else {
				embeddingCtx, cancel := x.routeContext(ctx)
				defer cancel()
				if admissionErr := s.admitClassifier(embeddingCtx, x); admissionErr != nil {
					out = &Outcome{Err: admissionErr}
				} else {
					result := runAttempts(embeddingCtx, s, x, attemptAdapter[operationplan.Result]{demand: x.attemptDemand, dispatch: func(attemptCtx context.Context, a routing.Attempt, p *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
						return s.unaryAttempt(attemptCtx, x, a, p, slot, fact)
					}})
					raw = result.result.Body
					out = &Outcome{Err: result.err, Committed: result.committed, Cancelled: result.cancelled}
				}
			}
		} else {
			parsed, parseErr := protocols.Parse(wire.Embeddings, body, "")
			if parseErr != nil {
				return guardrails.Verdict{}, guardrails.ErrSurface
			}
			x.generation.parsed = parsed
			if prepareErr := s.prepare(ctx, x, guardrailAuthorizer(s, x)); prepareErr != nil {
				out = &Outcome{Err: prepareErr}
			} else if admissionErr := s.admitClassifier(ctx, x); admissionErr != nil {
				out = &Outcome{Err: admissionErr}
			} else {
				out = s.execute(ctx, x)
				if out.Completion != nil {
					raw = out.Completion.Body
				}
			}
		}
	} else if slices.Contains(route.Operations, "classification") || slices.Contains(route.Operations, "moderation") {
		dialect := "tei-classification"
		body, _ := json.Marshal(map[string]string{"inputs": text})
		if slices.Contains(route.Operations, "moderation") {
			dialect = "openai-moderation"
			body, _ = json.Marshal(map[string]string{"model": route.Slug, "input": text})
		}
		codec, _ := operationregistry.Lookup(dialect)
		source, parseErr := operationplan.Parse(dialect, body, int(s.Config.MaxBodyBytes))
		if parseErr != nil {
			return guardrails.Verdict{}, guardrails.ErrSurface
		}
		x.unary = &unaryCall{source: source, dialect: codec, route: route.Slug, surface: "native"}
		x.mode = "unary"
		if e := s.prepareUnary(ctx, x); e != nil {
			out = &Outcome{Err: e}
		} else if e := s.admitClassifier(ctx, x); e != nil {
			out = &Outcome{Err: e}
		} else {
			result := runAttempts(ctx, s, x, attemptAdapter[operationplan.Result]{demand: x.attemptDemand, dispatch: func(ctx context.Context, a routing.Attempt, p *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, operationplan.Result, *AttemptFailure) {
				return s.unaryAttempt(ctx, x, a, p, slot, fact)
			}})
			raw = result.result.Body
			out = &Outcome{Err: result.err, Committed: result.committed, Cancelled: result.cancelled}
		}
	} else if slices.Contains(route.Operations, "generation") {
		body, _ := json.Marshal(map[string]any{"model": route.Slug, "messages": []map[string]string{{"role": "user", "content": text}}})
		parsed, err := protocols.Parse(wire.Chat, body, "")
		if err != nil {
			return guardrails.Verdict{}, err
		}
		x.family, x.generation.parsed = wire.Chat, parsed
		if e := s.prepare(ctx, x, guardrailAuthorizer(s, x)); e != nil {
			out = &Outcome{Err: e}
		} else if e := s.admitClassifier(ctx, x); e != nil {
			out = &Outcome{Err: e}
		} else {
			out = s.execute(ctx, x)
			if out.Completion != nil {
				raw = []byte(strings.TrimSpace(out.Completion.OutputText))
			}
		}
	} else {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	if out == nil {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	s.Finish(x, out, max(200, errorStatus(out.Err)))
	if out.Err != nil {
		return guardrails.Verdict{}, out.Err
	}
	if semantic {
		return guardrails.SemanticVerdict(raw, similarity, len(embeddingInputs)-len(similarity.Utterances))
	}
	return guardrails.ModelVerdict(raw, rule)
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

func (x *Call) guardrailHeaders(h http.Header, streamed, trailers bool) {
	if x.guard.engine == nil {
		return
	}
	if !x.guard.metadata && !x.responseMetadata {
		return
	}
	if streamed && !trailers {
		h.Add("Trailer", "X-OLP-Guardrails")
	}
	decisions := x.guard.evidence()
	values := []string{}
	for _, d := range decisions {
		if d.Outcome != guardrails.Passed && d.Outcome != guardrails.Skipped {
			values = append(values, d.GuardrailID+"@"+d.Revision+":"+string(d.Outcome))
		}
	}
	if len(values) > 0 {
		value := strings.Join(values, ",")
		if len(value) > 8192 {
			for len(values) > 0 && len(strings.Join(values, ",")) > 8192 {
				values = values[:len(values)-1]
			}
			value = strings.Join(values, ",")
		}
		h.Set("X-OLP-Guardrails", value)
	}
}

func (x *Call) GuardrailSurfaceGate(route *routing.Route) *fault.Error {
	if engine := x.Snapshot().GuardrailEngine(route.Slug, x.keyID); engine != nil {
		x.guard.engine = engine
		x.guard.metadata = x.request.guardrailMetadata
		result, err := engine.Unavailable()
		x.guard.record(result.Decisions)
		if err != nil {
			if legacy := policySurfaceGate(route); legacy != nil {
				return legacy
			}
			return guardrailError(err)
		}
	}
	return policySurfaceGate(route)
}

func (x *Call) legacyGuardrailError(result guardrails.Result, err error) error {
	if !x.Snapshot().HasGuardrails() {
		return err
	}
	for _, evidence := range result.Decisions {
		d := x.Snapshot().GuardrailDefinition(evidence.Revision)
		if d.LegacyRuleID == "" {
			continue
		}
		if evidence.Outcome == guardrails.Blocked || evidence.Outcome == guardrails.Redacted {
			recordDecision(x, contentpolicy.Decision{RuleID: d.LegacyRuleID, Phase: string(evidence.Phase), Action: string(evidence.Action), Outcome: string(evidence.Outcome)})
		}
	}
	if errors.Is(err, guardrails.ErrBlocked) && len(result.Decisions) > 0 {
		last := result.Decisions[len(result.Decisions)-1]
		if x.Snapshot().GuardrailDefinition(last.Revision).LegacyRuleID != "" {
			return errContentPolicyBlocked
		}
	}
	return err
}

func guardrailAuthorizer(s *Server, x *Call) func(*routing.Route) *fault.Error {
	if x.authorize != nil {
		return x.authorize
	}
	return s.keyAuthorizer(x.authority)
}
