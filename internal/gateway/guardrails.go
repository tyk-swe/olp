package gateway

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
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/operations"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type guardrailCallerKey struct{}

func (s *Server) initGuardrails() {
	runner := &guardrails.ExternalRunner{Egress: *s.egress, Clients: egress.NewConnectionClientCache(128)}
	credential := func(ctx context.Context, id string) ([]byte, error) {
		x, _ := ctx.Value(guardrailCallerKey{}).(*call)
		if x == nil || x.request.release == nil {
			return nil, errors.New("guardrail credentials unavailable")
		}
		secret, ok := x.request.release.Credential(id)
		if !ok {
			return nil, errors.New("guardrail credentials unavailable")
		}
		return secret, nil
	}
	runner.Credentials, runner.NetworkCredentials = credential, credential
	runner.Model = guardrails.EvaluatorFunc(s.modelGuardrail)
	runner.Plugin = guardrails.EvaluatorFunc(s.pluginGuardrail)
	s.GuardrailEvaluator = runner
}
func guardrailError(err error) *Error {
	if err == nil {
		return nil
	}
	var existing *Error
	if errors.As(err, &existing) {
		return existing
	}
	var e *guardrails.Error
	if errors.As(err, &e) {
		return &Error{Status: e.Status, Type: "invalid_request_error", Code: e.Code, Message: "The request could not satisfy the configured guardrails."}
	}
	return serverError(500, "guardrail_unavailable", "The guardrail could not be evaluated.")
}
func (s *Server) bindGuardrails(ctx context.Context, x *call) *Error {
	x.guardrailMetadata = x.request.guardrailMetadata
	x.guardrailEngine = x.snapshot().GuardrailEngine(x.route.Slug, x.keyID).WithEvaluator(s.GuardrailEvaluator)
	if x.internalGuardrail {
		x.guardrailEngine = x.guardrailEngine.WithoutModelGuardrails()
	}
	if x.guardrailEngine == nil {
		return nil
	}
	// Unary operation codecs expose inspection text but cannot apply rewritten
	// segments to their typed request and result documents.
	if x.unary != nil {
		for _, phase := range []string{guardrails.Input, guardrails.Output, guardrails.OutputStream} {
			if x.guardrailEngine.HasAction(phase, guardrails.Redact) || x.guardrailEngine.HasAction(phase, guardrails.Mask) {
				return guardrailError(guardrails.ErrSurface)
			}
		}
	}
	x.guardrailCtx = context.WithValue(ctx, guardrailCallerKey{}, x)
	if x.guardrailMasks == nil {
		x.guardrailMasks = &guardrails.Masking{}
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
		for _, phase := range []string{guardrails.Input, guardrails.InputParallel} {
			result, err := x.guardrailEngine.Unavailable(phase)
			x.recordGuardrails(result.Decisions)
			if err != nil {
				return guardrailError(err)
			}
		}
	}
	if retainsOutput && (x.guardrailEngine.HasPhase(guardrails.Output) || x.guardrailEngine.HasPhase(guardrails.OutputStream) || x.guardrailEngine.HasAction(guardrails.Input, guardrails.Mask)) {
		return guardrailError(guardrails.ErrSurface)
	}
	if x.generation.parsed != nil && x.generation.parsed.Stream {
		if err := x.guardrailStreamSurface(x.generation.parsed.OIF().Document()); err != nil {
			return guardrailError(err)
		}
		if err := x.guardrailEngine.ValidateStream(x.snapshot().Guardrails.Window()); err != nil {
			if x.route.ContentPolicy != nil {
				for _, rule := range x.route.ContentPolicy.Rules {
					if rule.Phase == contentpolicy.PhaseOutput {
						return policyUnavailable("content_policy_streaming_requires_unary", "This migrated output policy requires a buffered unary response.")
					}
				}
			}
			return guardrailError(err)
		}
	}
	return nil
}

func (x *call) guardrailStreamSurface(document oif.Document) error {
	if x.family == wire.Completions && (x.guardrailEngine.HasAction(guardrails.Output, guardrails.Block) || x.guardrailEngine.HasAction(guardrails.Output, guardrails.Redact) || x.guardrailEngine.HasAction(guardrails.Input, guardrails.Mask)) {
		if value, ok := document.Root().Lookup("logprobs"); ok && value.Kind() != oif.Null {
			return guardrails.ErrSurface
		}
	}
	if (x.family == wire.Chat || x.family == wire.Completions) && x.generation.parsed != nil && x.generation.parsed.Stream &&
		(x.guardrailEngine.HasPhase(guardrails.OutputStream) || x.guardrailEngine.HasAction(guardrails.Input, guardrails.Mask)) {
		// Log-probability alternatives and their byte encodings are separate
		// output surfaces that cannot share the content delta's stream window.
		if value, ok := document.Root().Lookup("logprobs"); ok && (value.Raw() == "true" || x.family == wire.Completions && value.Kind() != oif.Null) {
			return guardrails.ErrSurface
		}
	}
	return nil
}

func (x *call) recordGuardrails(values []guardrails.Evidence) {
	if len(values) == 0 {
		return
	}
	x.guardrailMu.Lock()
	defer x.guardrailMu.Unlock()
	x.guardrailDecisions = appendGuardrailEvidence(x.guardrailDecisions, values)
	if x.guardrailPreparation != nil {
		x.guardrailPreparation.guardrailDecisions = appendGuardrailEvidence(x.guardrailPreparation.guardrailDecisions, values)
	}
	if x.guardrailAttemptActive {
		x.guardrailAttemptDecisions = appendGuardrailEvidence(x.guardrailAttemptDecisions, values)
	}
}

func appendGuardrailEvidence(decisions, values []guardrails.Evidence) []guardrails.Evidence {
	size := 2
	for _, d := range decisions {
		size += guardrails.EvidenceSize(d)
	}
	for _, v := range values {
		seen := false
		for _, d := range decisions {
			if d.GuardrailID == v.GuardrailID && d.Revision == v.Revision && d.Phase == v.Phase && d.Action == v.Action && d.Outcome == v.Outcome && slices.Equal(d.Categories, v.Categories) {
				seen = true
				break
			}
		}
		if !seen && len(decisions) < guardrails.MaxEntries*4 && size+guardrails.EvidenceSize(v) <= guardrails.MaxEvidenceBytes {
			decisions = append(decisions, v)
			size += guardrails.EvidenceSize(v)
		}
	}
	return decisions
}

func (x *call) startGuardrailAttempt(input []guardrails.Evidence) {
	x.guardrailMu.Lock()
	defer x.guardrailMu.Unlock()
	x.guardrailAttemptActive = true
	x.guardrailAttemptDecisions = slices.Clone(input)
}

func (x *call) finishGuardrailAttempt() []guardrails.Evidence {
	x.guardrailMu.Lock()
	defer x.guardrailMu.Unlock()
	decisions := x.guardrailAttemptDecisions
	x.guardrailAttemptActive = false
	x.guardrailAttemptDecisions = nil
	return decisions
}

func (x *call) attemptGuardrailEvidence() []guardrails.Evidence {
	x.guardrailMu.Lock()
	defer x.guardrailMu.Unlock()
	return slices.Clone(x.guardrailAttemptDecisions)
}
func (x *call) guardrailEvidence() []guardrails.Evidence {
	x.guardrailMu.Lock()
	defer x.guardrailMu.Unlock()
	return slices.Clone(x.guardrailDecisions)
}
func (x *call) inspectGuardrailInput(document oif.Document) (oif.Document, []guardrails.Segment, error) {
	return x.inspectGuardrailInputWithRecovery(x.guardrailCtx, document, true)
}

func (x *call) inspectGuardrailInputWithRecovery(ctx context.Context, document oif.Document, recoverable bool) (oif.Document, []guardrails.Segment, error) {
	// Effective provider defaults may enable an unsupported stream surface.
	if err := x.guardrailStreamSurface(document); err != nil {
		return document, nil, err
	}
	surface, err := guardrails.NativeSurface(document, false)
	if err != nil {
		result, e := x.guardrailEngine.Unavailable("")
		x.recordGuardrails(result.Decisions)
		return document, nil, e
	}
	evaluation := guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.route.Slug, Phase: guardrails.Input, Segments: surface.Segments, InternalModel: x.internalGuardrail, WorkflowRecovery: recoverable && x.workflow != nil}
	evaluation.CompressionExemptIDs = slices.Collect(func(yield func(string) bool) {
		for _, segment := range surface.Segments {
			if segment.Kind == "tool_result" && slices.Contains(x.workflowRecoveryCalls, segment.ToolCallID) && !yield(segment.ID) {
				return
			}
		}
	})
	if x.strict() && x.guardrailEngine.HasLegacyInput() {
		texts := contentpolicy.InputTexts(document)
		evaluation.LegacyInput = make([]guardrails.Segment, len(texts))
		for i, text := range texts {
			evaluation.LegacyInput[i] = guardrails.Segment{ID: "legacy_" + strconv.Itoa(i), Text: text}
		}
	}
	result, err := x.guardrailEngine.Evaluate(ctx, evaluation, x.guardrailMasks)
	if recoverable && x.workflow != nil && len(result.Recovery) > 0 {
		x.workflowRecovery = result.Recovery
	}
	x.recordGuardrails(result.Decisions)
	err = x.legacyGuardrailError(result, err)
	if err != nil {
		return document, result.Segments, err
	}
	if result.Changed {
		document, err = surface.Apply(result.Segments)
	}
	return document, result.Segments, err
}
func (x *call) inspectOperationInput(texts []operations.Text) error {
	segments := make([]guardrails.Segment, len(texts))
	for i, t := range texts {
		segments[i] = guardrails.Segment{ID: "s" + strconv.Itoa(i), Text: t.Value}
	}
	result, err := x.guardrailEngine.Evaluate(x.guardrailCtx, guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.route.Slug, Phase: guardrails.Input, Segments: segments, InternalModel: x.internalGuardrail}, x.guardrailMasks)
	x.recordGuardrails(result.Decisions)
	x.guardrailInput = segments
	return x.legacyGuardrailError(result, err)
}
func (x *call) inspectOperationOutput(texts []operations.Text) error {
	segments := make([]guardrails.Segment, len(texts))
	for i, text := range texts {
		segments[i] = guardrails.Segment{ID: "s" + strconv.Itoa(i), Text: text.Value}
	}
	result, err := x.guardrailEngine.Evaluate(x.guardrailCtx, guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.route.Slug, Phase: guardrails.Output, Segments: segments, InternalModel: x.internalGuardrail}, nil)
	x.recordGuardrails(result.Decisions)
	return x.legacyGuardrailError(result, err)
}
func (x *call) inspectGuardrailOutput(body []byte) ([]byte, error) {
	if !x.guardrailEngine.HasPhase(guardrails.Output) && !x.guardrailEngine.HasAction(guardrails.Input, guardrails.Mask) {
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
				result, err := x.guardrailEngine.Unavailable(guardrails.Output)
				x.recordGuardrails(result.Decisions)
				if err != nil {
					return body, err
				}
				if x.guardrailEngine.HasAction(guardrails.Input, guardrails.Mask) {
					return body, guardrails.ErrSurface
				}
			}
		}
	}
	if x.strict() && x.guardrailEngine.HasLegacyOutput() {
		if err := interaction.CheckLegacyOutputCoverage(x.family, doc); err != nil {
			result, _ := x.guardrailEngine.Unavailable(guardrails.Output)
			x.recordGuardrails(result.Decisions)
			return body, &responseLocalRefusal{cause: err}
		}
	}
	surface, err := guardrails.NativeSurface(doc, true)
	if err != nil {
		result, e := x.guardrailEngine.Unavailable(guardrails.Output)
		x.recordGuardrails(result.Decisions)
		return body, e
	}
	changed := false
	segments := slices.Clone(surface.Segments)
	for i := range segments {
		value := x.guardrailMasks.Restore(segments[i].Text)
		if value != segments[i].Text {
			segments[i].Text = value
			changed = true
		}
	}
	// Tool-result provenance from input remains visible during output governance.
	for _, s := range x.guardrailInput {
		if s.Kind == "tool_result" {
			s.ID = "input_" + s.ID
			segments = append(segments, s)
		}
	}
	result, err := x.guardrailEngine.Evaluate(x.guardrailCtx, guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.route.Slug, Phase: guardrails.Output, Segments: segments, InternalModel: x.internalGuardrail}, x.guardrailMasks)
	x.recordGuardrails(result.Decisions)
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

func (x *call) startParallel(ctx context.Context, cancelUpstream context.CancelFunc) *parallelGuardrail {
	if !x.guardrailEngine.HasPhase(guardrails.InputParallel) {
		return nil
	}
	if x.cache != nil && x.cache.parallelEngine == x.guardrailEngine && slices.Equal(x.cache.parallelInput, x.guardrailInput) {
		return nil
	}
	evalctx, cancel := context.WithCancel(context.WithValue(ctx, guardrailCallerKey{}, x))
	p := &parallelGuardrail{result: make(chan guardrailResult, 1), cancel: cancel}
	request := guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.route.Slug, Phase: guardrails.InputParallel, Segments: slices.Clone(x.guardrailInput), InternalModel: x.internalGuardrail}
	go func() {
		result, err := x.guardrailEngine.Evaluate(evalctx, request, nil)
		p.result <- guardrailResult{result, err}
		if err != nil {
			cancelUpstream()
		}
	}()
	return p
}
func (p *parallelGuardrail) join(x *call, wait bool) error {
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
	x.recordGuardrails(result.result.Decisions)
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
	if !s.admit(r.Context()) {
		writeError(w, overloaded)
		return
	}
	defer s.release(r.Context())
	authority, e := s.authenticate(r, "guardrails")
	if e != nil {
		writeError(w, e)
		return
	}
	var in guardrails.ApplyInput
	if err := access.DecodeUnique(r, &in, guardrails.MaxContentBytes); err != nil {
		writeError(w, invalidRequest("invalid_request", "Supply a policy revision, phase and bounded text segments.", nil))
		return
	}
	x := &call{request: req, actor: "api_key", keyID: authority.ID, authority: authority, budgetGroupID: authority.BudgetGroupID, attribution: authority.Attribution, family: wire.Chat, operationOverride: "guardrail"}
	result, err := s.evaluateGuardrails(r.Context(), x, in)
	status := 200
	out := &outcome{}
	if err != nil {
		out.err = guardrailError(err)
		status = out.err.Status
		writeError(w, out.err)
	} else {
		x.guardrailMetadata = req.guardrailMetadata
		x.guardrailHeaders(w.Header(), false, false)
		writeJSON(w, result)
	}
	s.settleAdmission(context.WithoutCancel(r.Context()), x)
	s.finish(x, out, status)
}
func (s *Server) EvaluateToolGuardrails(ctx context.Context, authority access.Authority, release *runtime.Release, engine *guardrails.Engine, requestID, phase string, segments []guardrails.Segment, masks *guardrails.Masking) (guardrails.Result, error) {
	if s == nil || release == nil || release.Snapshot == nil {
		return guardrails.Result{Segments: segments}, guardrails.ErrSurface
	}
	parent := &call{
		request:       request{id: requestID, minted: true, startedAt: s.now(), release: release},
		actor:         "api_key",
		keyID:         authority.ID,
		budgetGroupID: authority.BudgetGroupID,
		authority:     authority,
		attribution:   authority.Attribution,
		endUserDigest: authority.EndUserDigest,
		family:        wire.Chat,
	}
	evaluation := guardrails.Evaluation{RequestID: parent.request.accountingID(), Phase: phase, Segments: segments}
	return engine.WithEvaluator(s.GuardrailEvaluator).Evaluate(context.WithValue(ctx, guardrailCallerKey{}, parent), evaluation, masks)
}

func (s *Server) EvaluateGuardrails(r *http.Request, p access.Principal, in guardrails.ApplyInput) (guardrails.Result, error) {
	if s == nil || s.Runtime == nil {
		return guardrails.Result{}, access.Fail(503, "guardrails_unavailable", "The evaluator is unavailable.")
	}
	if !s.admit(r.Context()) {
		return guardrails.Result{}, access.Fail(overloaded.Status, overloaded.Code, overloaded.Message)
	}
	defer s.release(r.Context())
	x := &call{request: request{id: uuid.Must(uuid.NewV7()).String(), minted: true, startedAt: s.now(), release: s.Runtime.Release()}, actor: "playground", origin: usage.OriginPlayground, userID: p.UserID(), family: wire.Chat, operationOverride: "guardrail"}
	x.authorize = func(route *routing.Route) *Error {
		if err := p.Authorize(access.Playground); err != nil {
			return permissionError("guardrail_forbidden", "Playground access is required.")
		}
		if err := p.Project(route.ProjectID, access.View); err != nil {
			return modelNotFound(route.Slug)
		}
		return nil
	}
	result, err := s.evaluateGuardrails(r.Context(), x, in)
	status := 200
	if err != nil {
		status = guardrailError(err).Status
	}
	s.settleAdmission(context.WithoutCancel(r.Context()), x)
	s.finish(x, &outcome{err: guardrailError(err)}, status)
	if err != nil {
		return result, access.Fail(status, guardrailError(err).Code, "The guardrail could not be evaluated.")
	}
	return result, nil
}
func (s *Server) evaluateGuardrails(ctx context.Context, x *call, in guardrails.ApplyInput) (guardrails.Result, error) {
	x.route = &routing.Route{Slug: "guardrails", OverallTimeout: 30000, MaxAttempts: 1}
	x.primary = x.route
	x.mode = "unary"
	if x.request.release == nil || (x.snapshot().Guardrails == nil && in.Policy == nil) {
		return guardrails.Result{}, guardrails.ErrSurface
	}
	registry := x.snapshot().Guardrails
	var p guardrails.PolicyRevision
	var ok bool
	var definitions map[string]guardrails.Revision
	if registry != nil {
		p, ok = registry.Policies[in.PolicyRevisionID]
		definitions = registry.Definitions
	}
	if in.Policy != nil {
		p = *in.Policy
		ok = true
		definitions = in.Definitions
	}
	if !ok {
		return guardrails.Result{}, guardrails.ErrSurface
	}
	if x.keyID != "" && (x.authority.ProjectID == nil || *x.authority.ProjectID != p.ProjectID) {
		return guardrails.Result{}, guardrails.ErrSurface
	}
	engine, err := guardrails.Compile(p.Entries, definitions, s.GuardrailEvaluator)
	if err != nil {
		return guardrails.Result{}, err
	}
	x.guardrailEngine = engine
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, guardrailCallerKey{}, x)
	if x.keyID != "" {
		var n int64
		for _, segment := range in.Segments {
			n += int64(len(segment.Text))
		}
		x.estimate = max(1, (n+3)/4)
		lease, e := s.Admission.reserveKey(ctx, x.authority, "openai", x.estimate, 30*time.Second, "guardrails")
		if e != nil {
			return guardrails.Result{}, e
		}
		x.lease = lease
	}
	x.dispatched = true
	result, err := engine.Evaluate(ctx, guardrails.Evaluation{RequestID: x.request.accountingID(), Phase: in.Phase, Segments: in.Segments}, &guardrails.Masking{})
	x.recordGuardrails(result.Decisions)
	if errors.Is(err, guardrails.ErrBlocked) {
		return result, nil
	}
	return result, err
}

func (s *Server) modelGuardrail(ctx context.Context, d guardrails.Revision, e guardrails.Evaluation) (verdict guardrails.Verdict, err error) {
	parent, _ := ctx.Value(guardrailCallerKey{}).(*call)
	if parent == nil || parent.internalGuardrail || parent.request.release == nil {
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
		if value, ok := parent.guardrailModels[key]; ok {
			return value, nil
		}
		defer func() {
			if err == nil {
				if parent.guardrailModels == nil {
					parent.guardrailModels = map[[32]byte]guardrails.Verdict{}
				}
				parent.guardrailModels[key] = verdict
			}
		}()
	}
	var config guardrails.ExternalConfig
	semantic := d.Type == "semantic_guard"
	if json.Unmarshal(d.Configuration, &config) != nil || (!semantic && config.Rule == nil) || (semantic && guardrails.ValidateSemantic(config.Semantic) != nil) {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	// A retained serving snapshot contains only its pinned route/provider.
	// Evaluators belong to the full release pinned at request admission.
	route, ok := parent.request.release.Snapshot.Routes[config.Route]
	if !ok {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	if parent.authorize != nil && parent.authorize(&route) != nil {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	if parent.keyID != "" && !parent.authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	var text string
	var embeddingInputs []string
	if semantic {
		if !slices.Contains(route.Operations, "embeddings") {
			return guardrails.Verdict{}, guardrails.ErrSurface
		}
		embeddingInputs, err = guardrails.SemanticInputs(e, *config.Semantic)
		if err != nil {
			return guardrails.Verdict{}, err
		}
		if len(embeddingInputs) == 0 {
			return guardrails.Verdict{Action: "pass"}, nil
		}
	} else {
		segments, _ := json.Marshal(e.Segments)
		text = config.Rule.Prompt + "\n" + string(segments)
	}
	x := &call{
		request:              request{id: uuid.Must(uuid.NewV7()).String(), minted: true, clientIP: parent.request.clientIP, startedAt: s.now(), release: parent.request.release},
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
		internalGuardrail:    true,
		evaluatorGuardrailID: d.GuardrailID,
		authorize:            parent.authorize,
		routeLeg:             routeLeg{route: &route},
	}
	if parent.keyID == "" {
		x.origin = usage.OriginPlayground
	}
	defer s.settleAdmission(context.WithoutCancel(ctx), x)
	var raw []byte
	var out *outcome
	if semantic {
		body, _ := json.Marshal(map[string]any{"model": route.Slug, "input": embeddingInputs, "encoding_format": "float"})
		x.request.body = &measuredBody{decoded: int64(len(body))}
		x.family, x.mode = wire.Embeddings, "unary"
		if route.Fidelity.Strict() {
			codec, _ := operationregistry.Lookup("openai-embeddings")
			source, parseErr := operationplan.Parse("openai-embeddings", body, int(s.cfg.MaxBodyBytes))
			if parseErr != nil {
				return guardrails.Verdict{}, guardrails.ErrSurface
			}
			x.unary = &unaryCall{source: source, dialect: codec, route: route.Slug, surface: "openai"}
			if prepareErr := s.prepareUnary(ctx, x); prepareErr != nil {
				out = &outcome{err: prepareErr}
			} else {
				embeddingCtx, cancel := x.routeContext(ctx)
				defer cancel()
				if admissionErr := s.admitClassifier(embeddingCtx, x); admissionErr != nil {
					out = &outcome{err: admissionErr}
				} else {
					result := runAttempts(embeddingCtx, s, x, attemptAdapter[operationplan.Result]{estimate: x.attemptReservation, dispatch: func(attemptCtx context.Context, a routing.Attempt, p *routing.Provider, slot routing.Slot, fact AttemptFact) (AttemptFact, operationplan.Result, *attemptFailure) {
						return s.unaryAttempt(attemptCtx, x, a, p, slot, fact)
					}})
					raw = result.result.Body
					out = &outcome{err: result.err, committed: result.committed, cancelled: result.cancelled}
				}
			}
		} else {
			parsed, parseErr := protocols.Parse(wire.Embeddings, body, "")
			if parseErr != nil {
				return guardrails.Verdict{}, guardrails.ErrSurface
			}
			x.generation.parsed = parsed
			if prepareErr := s.prepare(ctx, x, guardrailAuthorizer(s, x)); prepareErr != nil {
				out = &outcome{err: prepareErr}
			} else if admissionErr := s.admitClassifier(ctx, x); admissionErr != nil {
				out = &outcome{err: admissionErr}
			} else {
				out = s.execute(ctx, x)
				if out.completion != nil {
					raw = out.completion.Body
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
		source, parseErr := operationplan.Parse(dialect, body, int(s.cfg.MaxBodyBytes))
		if parseErr != nil {
			return guardrails.Verdict{}, guardrails.ErrSurface
		}
		x.unary = &unaryCall{source: source, dialect: codec, route: route.Slug, surface: "native"}
		x.mode = "unary"
		if e := s.prepareUnary(ctx, x); e != nil {
			out = &outcome{err: e}
		} else if e := s.admitClassifier(ctx, x); e != nil {
			out = &outcome{err: e}
		} else {
			result := runAttempts(ctx, s, x, attemptAdapter[operationplan.Result]{estimate: x.attemptReservation, dispatch: func(ctx context.Context, a routing.Attempt, p *routing.Provider, slot routing.Slot, fact AttemptFact) (AttemptFact, operationplan.Result, *attemptFailure) {
				return s.unaryAttempt(ctx, x, a, p, slot, fact)
			}})
			raw = result.result.Body
			out = &outcome{err: result.err, committed: result.committed, cancelled: result.cancelled}
		}
	} else if slices.Contains(route.Operations, "generation") {
		body, _ := json.Marshal(map[string]any{"model": route.Slug, "messages": []map[string]string{{"role": "user", "content": text}}})
		parsed, err := protocols.Parse(wire.Chat, body, "")
		if err != nil {
			return guardrails.Verdict{}, err
		}
		x.family, x.generation.parsed = wire.Chat, parsed
		if e := s.prepare(ctx, x, guardrailAuthorizer(s, x)); e != nil {
			out = &outcome{err: e}
		} else if e := s.admitClassifier(ctx, x); e != nil {
			out = &outcome{err: e}
		} else {
			out = s.execute(ctx, x)
			if out.completion != nil {
				raw = []byte(strings.TrimSpace(out.completion.OutputText))
			}
		}
	} else {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	if out == nil {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	s.finish(x, out, max(200, errorStatus(out.err)))
	if out.err != nil {
		return guardrails.Verdict{}, out.err
	}
	if semantic {
		return guardrails.SemanticVerdict(raw, *config.Semantic, len(embeddingInputs)-len(config.Semantic.Utterances))
	}
	return guardrails.ModelVerdict(raw, *config.Rule)
}

func (s *Server) pluginGuardrail(ctx context.Context, d guardrails.Revision, e guardrails.Evaluation) (guardrails.Verdict, error) {
	host, ok := s.cfg.Predicates.(interface {
		Guardrail(context.Context, string, abi.GuardrailRequest) (abi.GuardrailVerdict, error)
	})
	if !ok {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	var c guardrails.ExternalConfig
	if json.Unmarshal(d.Configuration, &c) != nil {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	raw, _ := json.Marshal(e)
	var in abi.GuardrailRequest
	_ = json.Unmarshal(raw, &in)
	in.Guardrail = d.Name
	verdict, err := host.Guardrail(ctx, c.PluginVersion, in)
	if err != nil {
		return guardrails.Verdict{}, guardrails.ErrSurface
	}
	raw, _ = json.Marshal(verdict)
	var out guardrails.Verdict
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (x *call) guardrailHeaders(h http.Header, streamed, trailers bool) {
	if x.guardrailEngine == nil {
		return
	}
	if !x.guardrailMetadata && !x.responseMetadata {
		return
	}
	if streamed && !trailers {
		h.Add("Trailer", "X-OLP-Guardrails")
	}
	decisions := x.guardrailEvidence()
	values := []string{}
	for _, d := range decisions {
		if d.Outcome != "passed" && d.Outcome != "skipped" {
			values = append(values, d.GuardrailID+"@"+d.Revision+":"+d.Outcome)
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

func (x *call) guardrailSurfaceGate(route *routing.Route) *Error {
	if engine := x.snapshot().GuardrailEngine(route.Slug, x.keyID); engine != nil {
		x.guardrailEngine = engine
		x.guardrailMetadata = x.request.guardrailMetadata
		result, err := engine.Unavailable("")
		x.recordGuardrails(result.Decisions)
		if err != nil {
			if legacy := policySurfaceGate(route); legacy != nil {
				return legacy
			}
			return guardrailError(err)
		}
	}
	return policySurfaceGate(route)
}

func (x *call) legacyGuardrailError(result guardrails.Result, err error) error {
	if x.snapshot().Guardrails == nil {
		return err
	}
	for _, evidence := range result.Decisions {
		d := x.snapshot().Guardrails.Definitions[evidence.Revision]
		if d.LegacyRuleID == "" {
			continue
		}
		if evidence.Outcome == "blocked" || evidence.Outcome == "redacted" {
			recordDecision(x, contentpolicy.Decision{RuleID: d.LegacyRuleID, Phase: evidence.Phase, Action: evidence.Action, Outcome: evidence.Outcome})
		}
	}
	if errors.Is(err, guardrails.ErrBlocked) && len(result.Decisions) > 0 {
		last := result.Decisions[len(result.Decisions)-1]
		if x.snapshot().Guardrails.Definitions[last.Revision].LegacyRuleID != "" {
			return &guardrails.Error{Code: "content_policy_blocked", Status: 400}
		}
	}
	return err
}

func guardrailAuthorizer(s *Server, x *call) func(*routing.Route) *Error {
	if x.authorize != nil {
		return x.authorize
	}
	return s.keyAuthorizer(x.authority)
}
