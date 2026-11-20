package kernel

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/guardstream"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

type attemptClock struct{ idle, limit *time.Timer }

func (run attemptRun) streamClock(plan attemptPlan) attemptClock {
	if !run.x.generation.parsed.Stream {
		return attemptClock{}
	}
	return attemptClock{idle: run.unarmedTimer(plan, 2), limit: run.unarmedTimer(plan, 3)}
}

func (run attemptRun) unarmedTimer(plan attemptPlan, reason int32) *time.Timer {
	state, cancel := run.state, plan.cancel
	timer := time.AfterFunc(maxStreamDuration, func() { state.reason.CompareAndSwap(0, reason); cancel() })
	timer.Stop()
	return timer
}

func (clock attemptClock) stop() {
	if clock.idle == nil {
		return
	}
	clock.idle.Stop()
	clock.limit.Stop()
}

type attemptStream struct {
	attemptRun
	sent    attemptSent
	clock   attemptClock
	guarded *guardstream.Stream
}

type attemptStreamed struct {
	fact       events.AttemptFact
	committed  bool
	actionable bool
	incomplete bool
}

func (seen attemptStreamed) with(fact events.AttemptFact) attemptStreamed {
	return attemptStreamed{fact: fact, committed: seen.committed, actionable: seen.actionable, incomplete: seen.incomplete}
}

func (run attemptRun) stream(sent attemptSent, clock attemptClock) (attemptReply, *attemptEnd, error) {
	x, plan := run.x, sent.plan
	if err := plan.reader.ValidateStream(sent.response.Header); err != nil {
		return attemptReply{fact: sent.fact}, ended(ClassProtocol, &AttemptFailure{Status: sent.response.StatusCode}), nil
	}
	clock.limit.Reset(maxStreamDuration)
	guarded, err := run.guardedStream()
	if err != nil {
		return attemptReply{fact: sent.fact}, ended(classPolicy, &AttemptFailure{policyCode: guardrailError(err).Code, overall: true, noRetry: true}), nil
	}
	stream := attemptStream{attemptRun: run, sent: sent, clock: clock, guarded: guarded}
	completion, seen, err := stream.relay()
	if guarded != nil {
		flushed, flushErr := stream.flushGuarded(seen, err)
		x.guard.record(guarded.Decisions())
		return stream.conclude(completion, flushed, flushErr)
	}
	return stream.conclude(completion, seen, err)
}

func (run attemptRun) guardedStream() (*guardstream.Stream, error) {
	x := run.x
	if !x.guard.engine.HasPhase(guardrails.OutputStream) && !x.guard.engine.HasAction(guardrails.Input, guardrails.Mask) {
		return nil, nil
	}
	return x.guardrailStream()
}

func (stream attemptStream) relay() (*wire.Completion, attemptStreamed, error) {
	plan := stream.sent.plan
	if plan.contract != nil && plan.contract.ToolContinuation() {
		return stream.continuation()
	}
	seen := attemptStreamed{fact: stream.sent.fact}
	completion, err := plan.codec.Stream(plan.reader.Stream(stream.sent.response.Body), int(stream.server.Config.MaxEventBytes), func(frame []byte) error {
		next, err := stream.emit(frame, seen)
		seen = next
		return err
	}, stream.observer(func(event oif.Event) error {
		next, err := stream.observe(event, seen)
		seen = next
		return err
	}))
	return completion, seen, err
}

func (stream attemptStream) conclude(completion *wire.Completion, seen attemptStreamed, err error) (attemptReply, *attemptEnd, error) {
	reply := attemptReply{fact: seen.fact, completion: completion, committed: seen.committed}
	if err == nil && seen.incomplete {
		stream.state.settled = true
		return reply, nil, &wire.ProtocolError{Detail: "strict Responses stream ended incomplete"}
	}
	return reply, nil, err
}

func (stream attemptStream) emit(frame []byte, seen attemptStreamed) (attemptStreamed, error) {
	if stream.guarded == nil {
		return stream.deliver(frame, seen)
	}
	frames, err := stream.guarded.Push(stream.sent.plan.sending, frame)
	if err != nil {
		return seen, err
	}
	return stream.deliverAll(frames, seen)
}

func (stream attemptStream) flushGuarded(seen attemptStreamed, relayed error) (attemptStreamed, error) {
	if relayed != nil {
		return seen, relayed
	}
	frames, err := stream.guarded.Finish(stream.sent.plan.sending)
	if err != nil {
		return seen, err
	}
	return stream.deliverAll(frames, seen)
}

func (stream attemptStream) deliverAll(frames [][]byte, seen attemptStreamed) (attemptStreamed, error) {
	delivered := seen
	for _, frame := range frames {
		next, err := stream.deliver(frame, delivered)
		delivered = next
		if err != nil {
			return delivered, err
		}
	}
	return delivered, nil
}

func (stream attemptStream) deliver(provided []byte, seen attemptStreamed) (attemptStreamed, error) {
	s, x := stream.server, stream.x
	if err := stream.sent.parallel.join(x, guardstream.Terminal(x.family, provided)); err != nil {
		return seen, err
	}
	frame, fact, err := stream.clientFrame(provided, seen.fact)
	if err != nil {
		return seen.with(fact), err
	}
	stream.heard(seen.committed)
	if len(frame) > int(s.Config.MaxEventBytes) {
		return attemptStreamed{fact: fact, committed: true, actionable: seen.actionable, incomplete: seen.incomplete}, wire.ErrEventTooLarge
	}
	err = x.generation.emit(frame)
	if err == nil && x.capture != nil {
		x.capture.CaptureStreamFrame(x.family, frame)
	}
	if fact.Interaction != nil {
		if fact.Interaction.ClientState == usage.ClientUnobserved {
			fact.Interaction.ClientState = usage.ClientPartial
		}
		if seen.actionable {
			// A failed write can have exposed a complete call before losing
			// the remaining frame. Do not infer non-actionability from error.
			fact.Interaction.ClientState = usage.ClientActionable
		}
	}
	return attemptStreamed{fact: stream.timed(fact, frame, err), committed: true, actionable: seen.actionable, incomplete: seen.incomplete}, err
}

func (stream attemptStream) timed(fact events.AttemptFact, frame []byte, written error) events.AttemptFact {
	if written != nil || fact.FirstOutput != nil || !protocols.MeaningfulFrame(stream.x.family, frame) {
		return fact
	}
	elapsed := stream.server.Now().Sub(fact.StartedAt)
	timed := fact
	timed.FirstOutput = &elapsed
	return timed
}

func (stream attemptStream) clientFrame(provided []byte, seen events.AttemptFact) ([]byte, events.AttemptFact, error) {
	s, x, format := stream.server, stream.x, stream.sent.plan.format
	fact := seen
	stateful, err := stream.statefulFrame(provided, &fact)
	if err != nil {
		return nil, fact, err
	}
	frame := stateful
	if x.family == wire.Responses {
		mapped, err := s.mapGenerationReferenceFrame(stream.ctx, x, responseNativeIdentity(x, &fact, ""), stateful)
		if err != nil {
			return nil, fact, &responseLocalRefusal{cause: err}
		}
		frame = mapped
	}
	if format == wire.Responses && (bytes.Contains(frame, []byte("event: response.failed\n")) || bytes.Contains(frame, []byte("event: error\n"))) {
		redacted, err := redactFailedResponseFrame(frame, x.sensitive)
		return redacted, fact, err
	}
	return frame, fact, nil
}

func (stream attemptStream) statefulFrame(provided []byte, fact *events.AttemptFact) ([]byte, error) {
	if !stream.x.providerState {
		return provided, nil
	}
	return stream.server.mapStreamResponseFrame(stream.ctx, stream.x, fact, provided, stream.sent.plan.format != wire.Responses)
}

func (stream attemptStream) heard(committed bool) {
	if !committed {
		stream.sent.plan.firstByte.Stop()
	}
	stream.clock.idle.Reset(stream.target.Timeout)
}

func (stream attemptStream) observer(observe func(oif.Event) error) func(oif.Event) error {
	if stream.sent.plan.contract == nil && !stream.stateful() {
		return nil
	}
	return observe
}

func (stream attemptStream) stateful() bool {
	return stream.sent.plan.format == wire.Responses && stream.x.providerState
}

func (stream attemptStream) observe(event oif.Event, seen attemptStreamed) (attemptStreamed, error) {
	fact := seen.fact
	if stream.stateful() {
		if err := stream.server.observeStreamResponseEvent(stream.ctx, stream.x, &fact, event); err != nil {
			return seen.with(fact), err
		}
	}
	observed := seen.with(fact)
	contract := stream.sent.plan.contract
	if contract == nil {
		return observed, nil
	}
	if err := contract.ValidateEvent(event); err != nil {
		return observed, err
	}
	return attemptStreamed{
		fact: fact, committed: seen.committed,
		actionable: seen.actionable || eventActionable(event),
		incomplete: seen.incomplete || stream.endsIncomplete(event),
	}, nil
}

func (stream attemptStream) endsIncomplete(event oif.Event) bool {
	if !stream.x.Strict() || stream.x.family != wire.Responses {
		return false
	}
	kind, present := event.Source().Root().Lookup("type")
	if !present {
		return false
	}
	text, ok := kind.Text()
	return ok && text == "response.incomplete"
}

func (stream attemptStream) continuation() (*wire.Completion, attemptStreamed, error) {
	s, x, plan := stream.server, stream.x, stream.sent.plan
	seen := attemptStreamed{fact: stream.sent.fact}
	projection, err := plan.contract.NewToolProjection(min(resources.MaxContinuationBytes, int(s.Config.MaxResponseBytes)))
	if err != nil {
		return nil, seen, err
	}
	completion, err := plan.codec.StreamNative(plan.reader.Stream(stream.sent.response.Body), int(s.Config.MaxEventBytes), func([]byte) error { return nil }, func(event oif.Event) error {
		frames, err := projection.Observe(event)
		if err != nil {
			return err
		}
		if seen.committed {
			stream.clock.idle.Reset(stream.target.Timeout)
		}
		for _, frame := range frames {
			next, err := stream.emit(interaction.ContinuationFrame(frame), seen)
			seen = next
			if err != nil {
				return err
			}
			x.generation.continuation.emitted++
		}
		return nil
	})
	if err != nil {
		return completion, seen, err
	}
	stream.state.settled = true
	state, delivery, err := projection.Complete(completion, x.generation.continuation.resource.ID)
	if err != nil {
		return completion, seen, err
	}
	if err := s.validateToolDelivery(delivery); err != nil {
		return completion, seen, err
	}
	if err := s.commitToolDelivery(stream.ctx, x, state, delivery); err != nil {
		return completion, seen, err
	}
	return stream.deliverContinuation(completion, delivery.Frames[x.generation.continuation.emitted:], seen)
}

func (stream attemptStream) deliverContinuation(completion *wire.Completion, frames []json.RawMessage, seen attemptStreamed) (*wire.Completion, attemptStreamed, error) {
	delivered := seen
	for _, frame := range frames {
		actionable := attemptStreamed{fact: delivered.fact, committed: delivered.committed, incomplete: delivered.incomplete, actionable: delivered.actionable || interaction.ContinuationActionable(frame)}
		next, err := stream.emit(interaction.ContinuationFrame(frame), actionable)
		delivered = next
		if err != nil {
			return completion, delivered, err
		}
	}
	done, err := stream.emit([]byte("data: [DONE]\n\n"), delivered)
	return completion, done, err
}
