package kernel

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/operations"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/guardstream"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/panics"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
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
		if err := x.guard.engine.ValidateRewrites(); err != nil {
			return guardrailError(err)
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
	if retainsOutput {
		if err := x.guard.engine.ValidateRetainedOutput(); err != nil {
			return guardrailError(err)
		}
	}
	if x.generation.parsed != nil && x.generation.parsed.Stream {
		if err := x.guardrailStreamSurface(x.generation.parsed.OIF().Document()); err != nil {
			return guardrailError(err)
		}
		if err := x.guard.engine.ValidateStream(x.Snapshot().GuardrailWindow()); err != nil {
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
	result, err := x.guard.engine.Evaluate(ctx, evaluation, x.guard.masks)
	if recoverable && x.workflow != nil && len(result.Recovery) > 0 {
		x.workflowRecovery = result.Recovery
	}
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
	return result.Decisions, err
}

func (x *Call) inspectOperationOutput(texts []operations.Text) error {
	segments := make([]guardrails.Segment, len(texts))
	for i, text := range texts {
		segments[i] = guardrails.Segment{ID: "s" + strconv.Itoa(i), Text: text.Value}
	}
	result, err := x.guard.engine.Evaluate(x.guard.ctx, guardrails.Evaluation{RequestID: x.request.accountingID(), Route: x.Route.Slug, Phase: guardrails.Output, Segments: segments, InternalModel: x.guard.internal}, nil)
	x.guard.record(result.Decisions)
	return err
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
			return guardrailError(err)
		}
	}
	return nil
}

func guardrailAuthorizer(s *Server, x *Call) func(*routing.Route) *fault.Error {
	if x.authorize != nil {
		return x.authorize
	}
	return s.keyAuthorizer(x.authority)
}
