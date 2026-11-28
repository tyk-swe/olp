package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/usage"
)

const RequestBodyTimeout = 15 * time.Second

type inferenceRequest struct {
	server  *Server
	w       http.ResponseWriter
	r       *http.Request
	control *http.ResponseController
	family  wire.Format
	dialect string
	x       *Call
	end     *inferenceEnd
}

// inferenceEnd is the one thing a request writes after it is built. The
// deferred settlement reads it even when a step panics.
type inferenceEnd struct {
	out    *Outcome
	status int
}

type inferenceCaller struct {
	token     string
	authority keyauthority.Authority
	body      []byte
}

func (s *Server) InferenceHandler(family wire.Format) http.HandlerFunc {
	return s.inference(family)
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

func (s *Server) newInferenceRequest(w http.ResponseWriter, r *http.Request, family wire.Format, dialect string) inferenceRequest {
	x := newCall(s.begin(w, r), family, callIdentity{actor: "api_key"}, callLineage{}, callWork{callSemantics: readSemantics(r)})
	if dialect == "" && fault.RequestSurface(r) == "gemini" {
		// Gemini embeddings match OpenAI embedding capabilities but
		// speak Gemini on the wire.
		x.ingress = "gemini"
	}
	return inferenceRequest{
		server: s, w: w, r: r, control: http.NewResponseController(w),
		family: family, dialect: dialect, x: x, end: &inferenceEnd{status: http.StatusInternalServerError},
	}
}

func (q inferenceRequest) finish() {
	q.server.finishContext(q.r.Context(), q.x, q.end.out, q.end.status)
	// The budgets this request reserved are settled even when the
	// client is gone: a concurrency slot nobody releases is a slot
	// every replica keeps counting.
	q.server.SettleAdmission(q.r.Context(), q.x)
	// Mirrors start once the caller's response is complete, so they
	// never add to its latency.
	if q.end.out != nil {
		q.server.mirror(q.x)
	}
}

func (q inferenceRequest) writeError(e *fault.Error) {
	if q.x.Strict() && q.x.dispatched && e.Status >= 500 {
		copy := *e
		copy.NoRetry = true
		e = &copy
	}
	fault.WriteSurfaceError(q.w, e, q.x.clientSurface())
}

func (q inferenceRequest) refuse(e *fault.Error) {
	q.x.Failure, q.end.status = e, e.Status
	q.writeError(e)
}

func (q inferenceRequest) fail(e *fault.Error) {
	q.end.out.Err, q.end.status = e, e.Status
	q.writeError(e)
}

func (q inferenceRequest) serve() {
	s := q.server
	if e := q.enterGate(); e != nil {
		q.refuse(e)
		return
	}
	defer s.Gate.Release(q.r.Context())
	caller, e := q.read()
	if e != nil {
		q.refuse(e)
		return
	}
	unary, err := s.selectUnary(q.x, q.family, q.dialect, caller.body, q.r.PathValue("model"), q.r.Header.Get(StateRouteHeader))
	if err != nil {
		q.refuse(RequestError(err))
		return
	}
	if unary {
		q.end.out, q.end.status = s.serveUnary(q.w, q.r, q.x)
		return
	}
	parsed, e := q.parse(caller.body)
	if e != nil {
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
	if e := q.plan(caller, parsed); e != nil {
		q.refuse(e)
		return
	}
	q.run(caller.authority, parsed)
}

func (q inferenceRequest) enterGate() *fault.Error {
	// A context timeout alone cannot interrupt a blocked socket read.
	if err := q.control.SetReadDeadline(time.Now().Add(RequestBodyTimeout)); err != nil {
		return fault.ServerError(http.StatusInternalServerError, "internal_error", "The request could not be read.")
	}
	if !q.server.Gate.AdmitLater(q.r.Context()) {
		return fault.Overloaded
	}
	return nil
}

func (q inferenceRequest) read() (inferenceCaller, *fault.Error) {
	s, x := q.server, q.x
	token, e := ingress.PresentedKey(q.r)
	if e != nil {
		return inferenceCaller{}, e
	}
	authority, e := s.Gate.AuthenticateRequest(q.r, token, "inference")
	if e != nil {
		return inferenceCaller{}, e
	}
	s.BindAuthority(x, authority)
	x.mirrors = true
	if x.attribution, e = ingress.ParseAttribution(q.r, authority); e != nil {
		return inferenceCaller{}, e
	}
	body, e := s.Gate.ReadBody(q.r)
	if e != nil {
		return inferenceCaller{}, e
	}
	// Keep the deadline on failed reads so HTTP/1 body draining stays
	// bounded; successful uploads must not limit the inference stream.
	q.control.SetReadDeadline(time.Time{})
	return inferenceCaller{token: token, authority: authority, body: body}, nil
}

func (q inferenceRequest) parse(body []byte) (*wire.Request, *fault.Error) {
	x := q.x
	parsed, e := q.decode(body)
	if e != nil {
		return nil, e
	}
	if err := protocols.ValidateInlineMedia(parsed, q.server.Config.InlineMedia); err != nil {
		return nil, RequestError(err)
	}
	var err error
	x.generation.parsed, x.generation.approval, err = stripWorkflowApproval(parsed)
	if err != nil {
		return nil, fault.InvalidRequest("invalid_request", "The workflow approval could not be removed from the request.", nil)
	}
	return parsed, nil
}

func (q inferenceRequest) decode(body []byte) (*wire.Request, *fault.Error) {
	s, x, model := q.server, q.x, q.r.PathValue("model")
	switch q.family {
	case wire.Chat, wire.Responses, wire.Anthropic, wire.Gemini, wire.GeminiStream:
		prompt, e := s.parsePrompt(x, q.family, body, model, s.keyAuthorizer(x.authority))
		x.prompt = prompt.Reference
		if x.request.body != nil {
			x.request.body.Decoded = max(x.request.body.Decoded, prompt.Decoded)
		}
		return prompt.Request, e
	case wire.Bedrock:
		parsed, err := protocols.ParseBedrockRequest(body, model, strings.HasSuffix(q.r.URL.Path, "/converse-stream"))
		if err != nil {
			return nil, RequestError(err)
		}
		return parsed, nil
	}
	parsed, err := protocols.Parse(q.family, body, model)
	if err != nil {
		return nil, RequestError(err)
	}
	return parsed, nil
}

func (q inferenceRequest) replay() {
	q.end.status, q.end.out = http.StatusOK, ContinuationDeliveryOutcome(q.server.replayContinuation(q.w, q.x))
	// Replaying a committed response never starts shadow inference.
	q.x.mirrors = false
}

func (q inferenceRequest) plan(caller inferenceCaller, parsed *wire.Request) *fault.Error {
	s, x, ctx := q.server, q.x, q.r.Context()
	if q.family == wire.Responses {
		if e := s.responsesStateGate(ctx, x, caller.authority, parsed); e != nil {
			return e
		}
	}
	if e := s.bindGenerationReferences(ctx, x); e != nil {
		return e
	}
	var e *fault.Error
	if x.preferences, x.priority, e = routingControls(q.r, caller.authority.Policy); e != nil {
		return e
	}
	if e := s.prepare(ctx, x, s.keyAuthorizer(caller.authority)); e != nil {
		return e
	}
	if e := q.prepareWorkflow(caller.token); e != nil {
		return e
	}
	if s.Deps.Capture != nil && x.prompt == nil && x.workflow == nil && x.origin == "" && x.OperationName() == "generation" {
		x.capture = s.Deps.Capture.Resolve(x.captureProject(), x.named().Slug, x.keyID, x.endUserDigest, x.request.accountingID())
	}
	return nil
}

func (q inferenceRequest) prepareWorkflow(token string) *fault.Error {
	s, x, ctx := q.server, q.x, q.r.Context()
	if x.named().ToolExecution != "gateway" {
		return nil
	}
	if e := s.prepareWorkflow(ctx, x, token); e != nil {
		return e
	}
	return s.prepareWorkflowResume(ctx, x)
}

func (q inferenceRequest) run(authority keyauthority.Authority, parsed *wire.Request) {
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
	if e := q.admit(ctx, authority, overall); e != nil {
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
	if e := q.startRun(ctx, authority); e != nil {
		q.refuse(e)
		return
	}
	if x.OperationName() == "generation" {
		s.ObserveAdmittedGeneration(x, authority)
	}
	q.respond(ctx, authority, parsed)
}

func (q inferenceRequest) admit(ctx context.Context, authority keyauthority.Authority, overall time.Duration) *fault.Error {
	s, x := q.server, q.x
	if e := s.Gate.AwaitAdmission(ctx, x.priority); e != nil {
		return e
	}
	estimate := quota.KeyReservationEstimate(x.estimate, s.dispatchableAttempts(x))
	admitted := x.AdmissionAuthority(authority)
	return s.admitCacheAware(ctx, x, &admitted, estimate, overall, s.costReservation(x, authority))
}

func (q inferenceRequest) startRun(ctx context.Context, authority keyauthority.Authority) *fault.Error {
	x := q.x
	if x.workflow == nil || x.workflow.runID != "" {
		return nil
	}
	runID, err := q.server.Deps.WorkflowRuns.Start(ctx, authority, x.named().ID, x.named().RevisionID)
	if err != nil {
		return fault.ServerError(http.StatusServiceUnavailable, "tool_execution_unavailable", "The workflow run could not be reserved.")
	}
	x.workflow.runID = runID
	return nil
}

func (q inferenceRequest) respond(ctx context.Context, authority keyauthority.Authority, parsed *wire.Request) {
	x := q.x
	if parsed.Stream && x.workflow == nil {
		q.stream(ctx)
		return
	}
	out := q.execute(ctx)
	q.end.out = out
	if out.Err != nil {
		q.end.status = out.Err.Status
		if out.Err.Status > 0 {
			q.writeError(out.Err)
		}
		return
	}
	if out.approval != nil {
		q.pause(parsed.Stream)
		return
	}
	if parsed.Stream && x.workflow != nil {
		q.streamWorkflow()
		return
	}
	q.deliver(authority)
}

func (q inferenceRequest) execute(ctx context.Context) *Outcome {
	if q.x.workflow != nil {
		return q.server.executeToolLoop(ctx, q.x)
	}
	return q.server.executeModelStep(ctx, q.x, q.x.generation.parsed, q.x.authority)
}

func (q inferenceRequest) stream(ctx context.Context) {
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
	out := s.executeModelStep(ctx, x, x.generation.parsed, x.authority)
	q.end.out = out
	q.end.status = sw.Finish(out.Err, out.Cancelled)
}

func (q inferenceRequest) pause(streams bool) {
	s, x, end := q.server, q.x, q.end
	if !streams {
		q.w.Header().Set("Content-Type", "application/json")
		end.status = http.StatusOK
		if err := json.NewEncoder(q.w).Encode(end.out.approval); err != nil {
			end.out.Cancelled = true
		}
		return
	}
	frames, err := workflowPauseFrames(q.family, x.named().Slug, x.request.accountingID(), end.out.approval, int(s.Config.MaxEventBytes), int(s.Config.MaxResponseBytes))
	if err != nil {
		q.fail(fault.ServerError(http.StatusBadGateway, "invalid_tool_response", "The approval response could not be prepared."))
		return
	}
	q.w.Header().Set("Content-Type", "text/event-stream")
	end.status = http.StatusOK
	if _, err := q.w.Write(frames); err != nil {
		end.out.Cancelled = true
	}
}

func (q inferenceRequest) streamWorkflow() {
	x, out := q.x, q.end.out
	sw := &fault.StreamWriter{W: q.w, Family: q.family, Headers: streamedHeaders{x}}
	if err := sw.Emit(x.workflow.finalFrames); err != nil {
		out.Err = (&AttemptFailure{class: ClassCancelled}).Fault()
		out.Cancelled = true
	} else {
		out.Committed = true
		x.facts[len(x.facts)-1].Committed = true
		x.Delivered(q.server.Now())
	}
	q.end.status = sw.Finish(out.Err, out.Cancelled)
}

func (q inferenceRequest) deliver(authority keyauthority.Authority) {
	if err := q.control.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout)); err != nil {
		q.fail(fault.ServerError(http.StatusInternalServerError, "internal_error", "The response could not be written."))
		return
	}
	q.w.Header().Set("Content-Type", "application/json")
	body, e := q.project(authority, q.end.out.Completion.Body)
	q.end.out.Completion.Body = body
	if e != nil {
		q.fail(e)
		return
	}
	q.write()
}

func (q inferenceRequest) project(authority keyauthority.Authority, body []byte) ([]byte, *fault.Error) {
	stored, e := q.stored(authority, body)
	if e != nil {
		return body, e
	}
	referenced, e := q.referenced(stored)
	if e != nil {
		return stored, e
	}
	return referenced, nil
}

func (q inferenceRequest) stored(authority keyauthority.Authority, body []byte) ([]byte, *fault.Error) {
	if q.family != wire.Responses || !q.x.providerState {
		return body, nil
	}
	return q.server.mapStoredResponse(q.r.Context(), q.x, authority, body)
}

func (q inferenceRequest) referenced(body []byte) ([]byte, *fault.Error) {
	x := q.x
	if q.family != wire.Responses || len(x.facts) == 0 {
		return body, nil
	}
	identity := responseNativeIdentity(x, &x.facts[len(x.facts)-1], "")
	mapped, err := q.server.mapGenerationResultReferences(q.r.Context(), x, liveResult(identity), body)
	if err != nil {
		return nil, fault.ServerError(http.StatusBadGateway, "resource_affinity", "The provider returned an unowned result reference.")
	}
	return mapped, nil
}

func (q inferenceRequest) write() {
	x, out := q.x, q.end.out
	q.end.status = http.StatusOK
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
