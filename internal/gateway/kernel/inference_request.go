package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/usage"
)

const RequestBodyTimeout = 15 * time.Second

type inferenceRequest struct {
	server    *Server
	w         http.ResponseWriter
	r         *http.Request
	control   *http.ResponseController
	family    wire.Format
	dialect   string
	x         *Call
	token     string
	authority keyauthority.Authority
	body      []byte
	parsed    *wire.Request
	out       *Outcome
	status    int
}

func (s *Server) inference(family wire.Format) http.HandlerFunc {
	return s.inferenceOperation(family, "")
}

func (s *Server) inferenceOperation(family wire.Format, dialect string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := s.newInferenceRequest(w, r, family, dialect)
		defer q.finish()
		q.serve()
	}
}

func (s *Server) newInferenceRequest(w http.ResponseWriter, r *http.Request, family wire.Format, dialect string) *inferenceRequest {
	x := &Call{request: s.begin(w, r), family: family, actor: "api_key"}
	if dialect == "" && fault.RequestSurface(r) == "gemini" {
		// Gemini embeddings match OpenAI embedding capabilities but
		// speak Gemini on the wire.
		x.ingress = "gemini"
	}
	x.semanticHeaders = semanticHeaders(r.Header)
	query, err := url.ParseQuery(r.URL.RawQuery)
	x.semanticQuery, x.semanticQueryInvalid = query, err != nil
	return &inferenceRequest{
		server: s, w: w, r: r, control: http.NewResponseController(w),
		family: family, dialect: dialect, x: x, status: http.StatusInternalServerError,
	}
}

func (q *inferenceRequest) finish() {
	q.server.Finish(q.x, q.out, q.status)
	// The budgets this request reserved are settled even when the
	// client is gone: a concurrency slot nobody releases is a slot
	// every replica keeps counting.
	q.server.settleAdmission(q.r.Context(), q.x)
	// Mirrors start once the caller's response is complete, so they
	// never add to its latency.
	if q.out != nil {
		q.server.mirror(q.x)
	}
}

func (q *inferenceRequest) writeError(e *fault.Error) {
	if q.x.Strict() && q.x.dispatched && e.Status >= 500 {
		copy := *e
		copy.NoRetry = true
		e = &copy
	}
	fault.WriteSurfaceError(q.w, e, q.x.clientSurface())
}

func (q *inferenceRequest) refuse(e *fault.Error) {
	q.x.Failure, q.status = e, e.Status
	q.writeError(e)
}

func (q *inferenceRequest) fail(e *fault.Error) {
	q.out.Err, q.status = e, e.Status
	q.writeError(e)
}

func (q *inferenceRequest) serve() {
	s := q.server
	if e := q.enterGate(); e != nil {
		q.refuse(e)
		return
	}
	defer s.Gate.Release(q.r.Context())
	if e := q.read(); e != nil {
		q.refuse(e)
		return
	}
	unary, err := s.selectUnary(q.x, q.family, q.dialect, q.body, q.r.PathValue("model"), q.r.Header.Get(StateRouteHeader))
	if err != nil {
		q.refuse(RequestError(err))
		return
	}
	if unary {
		q.out, q.status = s.serveUnary(q.w, q.r, q.x, q.body)
		return
	}
	if e := q.parse(); e != nil {
		q.refuse(e)
		return
	}
	if e := s.prepareContinuation(q.r.Context(), q.x); e != nil {
		q.refuse(e)
		return
	}
	if continuation := q.x.generation.continuation; continuation != nil && continuation.replay != nil {
		q.replay()
		return
	}
	if e := q.plan(); e != nil {
		q.refuse(e)
		return
	}
	q.run()
}

func (q *inferenceRequest) enterGate() *fault.Error {
	// A context timeout alone cannot interrupt a blocked socket read.
	if err := q.control.SetReadDeadline(time.Now().Add(RequestBodyTimeout)); err != nil {
		return fault.ServerError(http.StatusInternalServerError, "internal_error", "The request could not be read.")
	}
	if !q.server.Gate.AdmitLater(q.r.Context()) {
		return fault.Overloaded
	}
	return nil
}

func (q *inferenceRequest) read() *fault.Error {
	s, x := q.server, q.x
	token, e := ingress.PresentedKey(q.r)
	if e != nil {
		return e
	}
	authority, e := s.Gate.AuthenticateRequest(q.r, token, "inference")
	if e != nil {
		return e
	}
	q.token, q.authority = token, authority
	s.bindAuthority(x, authority)
	x.mirrors = true
	if x.attribution, e = ingress.ParseAttribution(q.r, authority); e != nil {
		return e
	}
	body, e := s.Gate.ReadBody(q.r)
	if e != nil {
		return e
	}
	q.body = body
	// Keep the deadline on failed reads so HTTP/1 body draining stays
	// bounded; successful uploads must not limit the inference stream.
	q.control.SetReadDeadline(time.Time{})
	return nil
}

func (q *inferenceRequest) parse() *fault.Error {
	x := q.x
	parsed, e := q.decode()
	if e != nil {
		return e
	}
	if err := protocols.ValidateInlineMedia(parsed, q.server.Config.InlineMedia); err != nil {
		return RequestError(err)
	}
	var err error
	x.generation.parsed, x.generation.approval, err = stripWorkflowApproval(parsed)
	if err != nil {
		return fault.InvalidRequest("invalid_request", "The workflow approval could not be removed from the request.", nil)
	}
	q.parsed = parsed
	return nil
}

func (q *inferenceRequest) decode() (*wire.Request, *fault.Error) {
	s, x, model := q.server, q.x, q.r.PathValue("model")
	switch q.family {
	case wire.Chat, wire.Responses, wire.Anthropic, wire.Gemini, wire.GeminiStream:
		prompt, e := s.parsePrompt(x, q.family, q.body, model, s.keyAuthorizer(x.authority))
		x.prompt = prompt.Reference
		if x.request.body != nil {
			x.request.body.Decoded = max(x.request.body.Decoded, prompt.Decoded)
		}
		return prompt.Request, e
	case wire.Bedrock:
		parsed, err := protocols.ParseBedrockRequest(q.body, model, strings.HasSuffix(q.r.URL.Path, "/converse-stream"))
		if err != nil {
			return nil, RequestError(err)
		}
		return parsed, nil
	}
	parsed, err := protocols.Parse(q.family, q.body, model)
	if err != nil {
		return nil, RequestError(err)
	}
	return parsed, nil
}

func (q *inferenceRequest) replay() {
	status, err := q.server.replayContinuation(q.w, q.x)
	q.status, q.out = status, continuationDeliveryOutcome(err)
	// Replaying a committed response never starts shadow inference.
	q.x.mirrors = false
}

func (q *inferenceRequest) plan() *fault.Error {
	s, x, ctx := q.server, q.x, q.r.Context()
	if q.family == wire.Responses {
		if e := s.responsesStateGate(ctx, x, q.authority, q.parsed); e != nil {
			return e
		}
	}
	if e := s.bindGenerationReferences(ctx, x); e != nil {
		return e
	}
	var e *fault.Error
	if x.preferences, x.priority, e = routingControls(q.r, q.authority.Policy); e != nil {
		return e
	}
	if e := s.prepare(ctx, x, s.keyAuthorizer(q.authority)); e != nil {
		return e
	}
	if e := q.prepareWorkflow(); e != nil {
		return e
	}
	if s.Deps.Capture != nil && x.prompt == nil && x.workflow == nil && x.origin == "" && x.operationName() == "generation" {
		x.captureContext = ctx
		x.capture = s.Deps.Capture.Resolve(x.captureProject(), x.named().Slug, x.keyID, x.endUserDigest, x.request.accountingID())
	}
	return nil
}

func (q *inferenceRequest) prepareWorkflow() *fault.Error {
	s, x, ctx := q.server, q.x, q.r.Context()
	if x.named().ToolExecution != "gateway" {
		return nil
	}
	if e := s.prepareWorkflow(ctx, x, q.token); e != nil {
		return e
	}
	return s.prepareWorkflowResume(ctx, x)
}

func (q *inferenceRequest) run() {
	s, x := q.server, q.x
	// Admission happens once the request is understood and before any
	// provider is called, so a rejected request costs an upstream nothing.
	// The lease outlives the route deadline it is sized against: it is the
	// backstop for a replica that dies mid-request, not the deadline.
	x.estimate = requestEstimate(x)
	overall := time.Duration(x.named().OverallTimeout) * time.Millisecond
	// Admission is part of the same deadline as execution; starting a new
	// full deadline afterwards could outlive the concurrency reservation.
	ctx, cancel := x.routeContext(q.r.Context())
	defer cancel()
	if e := q.admit(ctx, overall); e != nil {
		q.refuse(e)
		return
	}
	lease, e := s.reserveWorkflowRun(ctx, x)
	if e != nil {
		q.refuse(e)
		return
	}
	defer func() {
		if err := lease.Settle(ctx, nil); err != nil {
			s.Deps.Admission.Logger().Error("release workflow concurrency", "error", err)
		}
	}()
	if e := q.startRun(ctx); e != nil {
		q.refuse(e)
		return
	}
	q.respond(ctx)
}

func (q *inferenceRequest) admit(ctx context.Context, overall time.Duration) *fault.Error {
	s, x := q.server, q.x
	if e := s.Gate.AwaitAdmission(ctx, x.priority); e != nil {
		return e
	}
	estimate := quota.KeyReservationEstimate(x.estimate, s.dispatchableAttempts(x))
	admitted := x.AdmissionAuthority(q.authority)
	return s.admitCacheAware(ctx, x, &admitted, estimate, overall, s.costReservation(x, q.authority))
}

func (q *inferenceRequest) startRun(ctx context.Context) *fault.Error {
	x := q.x
	if x.workflow == nil || x.workflow.runID != "" {
		return nil
	}
	runID, err := q.server.Deps.WorkflowRuns.Start(ctx, q.authority, x.named().ID, x.named().RevisionID)
	if err != nil {
		return fault.ServerError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The workflow run could not be reserved.")
	}
	x.workflow.runID = runID
	return nil
}

func (q *inferenceRequest) respond(ctx context.Context) {
	s, x := q.server, q.x
	if q.parsed.Stream && x.workflow == nil {
		q.stream(ctx)
		return
	}
	if x.workflow != nil {
		q.out = s.executeToolLoop(ctx, x)
	} else {
		q.out = s.executeModelStep(ctx, x, x.generation.parsed, x.authority)
	}
	if q.out.Err != nil {
		q.status = q.out.Err.Status
		if q.out.Err.Status > 0 {
			q.writeError(q.out.Err)
		}
		return
	}
	if q.out.approval != nil {
		q.pause()
		return
	}
	if q.parsed.Stream && x.workflow != nil {
		q.streamWorkflow()
		return
	}
	q.deliver()
}

func (q *inferenceRequest) stream(ctx context.Context) {
	s, x := q.server, q.x
	sw := &fault.StreamWriter{W: q.w, Family: q.family, Headers: streamedHeaders{x}}
	x.generation.emit = func(frame []byte) error {
		err := sw.Emit(frame)
		if err == nil {
			x.captureCacheFrame(frame)
			x.Delivered(s.Now())
		}
		return err
	}
	q.out = s.executeModelStep(ctx, x, x.generation.parsed, x.authority)
	q.status = sw.Finish(q.out.Err, q.out.Cancelled)
}

func (q *inferenceRequest) pause() {
	s, x := q.server, q.x
	if !q.parsed.Stream {
		q.w.Header().Set("Content-Type", "application/json")
		q.status = http.StatusOK
		if err := json.NewEncoder(q.w).Encode(q.out.approval); err != nil {
			q.out.Cancelled = true
		}
		return
	}
	frames, err := workflowPauseFrames(q.family, x.named().Slug, x.request.accountingID(), q.out.approval, int(s.Config.MaxEventBytes), int(s.Config.MaxResponseBytes))
	if err != nil {
		q.fail(fault.ServerError(http.StatusBadGateway, "invalid_tool_response", "The approval response could not be prepared."))
		return
	}
	q.w.Header().Set("Content-Type", "text/event-stream")
	q.status = http.StatusOK
	if _, err := q.w.Write(frames); err != nil {
		q.out.Cancelled = true
	}
}

func (q *inferenceRequest) streamWorkflow() {
	x := q.x
	sw := &fault.StreamWriter{W: q.w, Family: q.family, Headers: streamedHeaders{x}}
	if err := sw.Emit(x.workflow.finalFrames); err != nil {
		q.out.Err = (&AttemptFailure{class: ClassCancelled}).Fault()
		q.out.Cancelled = true
	} else {
		q.out.Committed = true
		x.facts[len(x.facts)-1].Committed = true
		x.Delivered(q.server.Now())
	}
	q.status = sw.Finish(q.out.Err, q.out.Cancelled)
}

func (q *inferenceRequest) deliver() {
	if err := q.control.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
		q.fail(fault.ServerError(http.StatusInternalServerError, "internal_error", "The response could not be written."))
		return
	}
	q.w.Header().Set("Content-Type", "application/json")
	if e := q.project(); e != nil {
		q.fail(e)
		return
	}
	q.write()
}

func (q *inferenceRequest) project() *fault.Error {
	s, x, out, ctx := q.server, q.x, q.out, q.r.Context()
	if q.family == wire.Responses && x.providerState {
		mapped, e := s.mapStoredResponse(ctx, x, q.authority, out.Completion.Body)
		if e != nil {
			return e
		}
		out.Completion.Body = mapped
	}
	if q.family == wire.Responses && len(x.facts) > 0 {
		identity := responseNativeIdentity(x, &x.facts[len(x.facts)-1], "")
		mapped, err := s.mapGenerationResultReferences(ctx, x, identity, out.Completion.Body)
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "resource_affinity", "The provider returned an unowned result reference.")
		}
		out.Completion.Body = mapped
	}
	enforced, e := s.enforceContentOutput(x, out.Completion.Body)
	if e != nil {
		return e
	}
	out.Completion.Body = enforced
	return nil
}

func (q *inferenceRequest) write() {
	x, out := q.x, q.out
	q.status = http.StatusOK
	x.ResponseHeaders(q.w.Header(), false)
	q.w.WriteHeader(http.StatusOK)
	out.Committed = true
	x.facts[len(x.facts)-1].Committed = true
	if fact := &x.facts[len(x.facts)-1]; fact.Interaction != nil {
		fact.Interaction.ClientState = usage.ClientPartial
	}
	_, err := q.w.Write(out.Completion.Body)
	if err == nil {
		// Flush buffered responses before recording successful delivery.
		err = q.control.Flush()
	}
	if err != nil {
		out.Err = (&AttemptFailure{class: ClassCancelled}).Fault()
		out.Cancelled = true
		return
	}
	x.Delivered(q.server.Now())
	if fact := &x.facts[len(x.facts)-1]; fact.Interaction != nil {
		fact.Interaction.ClientState = usage.ClientTerminal
	}
}
