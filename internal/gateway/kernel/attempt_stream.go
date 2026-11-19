package kernel

import (
	"bytes"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/guardstream"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/interaction"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

func (run *attemptRun) stream() (*attemptEnd, error) {
	s, x := run.server, run.x
	if err := run.reader.ValidateStream(run.response.Header); err != nil {
		return ended(ClassProtocol, &AttemptFailure{Status: run.response.StatusCode}), nil
	}
	run.streamCap = time.AfterFunc(maxStreamDuration, func() { run.state.reason.CompareAndSwap(0, 3); run.cancel() })
	if x.guard.engine.HasPhase(guardrails.OutputStream) || x.guard.engine.HasAction(guardrails.Input, guardrails.Mask) {
		guarded, err := x.guardrailStream()
		if err != nil {
			return ended(classPolicy, &AttemptFailure{policyCode: guardrailError(err).Code, overall: true, noRetry: true}), nil
		}
		run.guarded = guarded
	}
	var err error
	if run.contract != nil && run.contract.ToolContinuation() {
		err = run.streamContinuation()
	} else {
		run.completion, err = run.codec.Stream(run.reader.Stream(run.response.Body), int(s.Config.MaxEventBytes), run.emit, run.observer())
	}
	if run.guarded != nil {
		if err == nil {
			err = run.flushGuarded()
		}
		x.guard.record(run.guarded.Decisions())
	}
	if err == nil && run.incomplete {
		run.state.settled = true
		err = &wire.ProtocolError{Detail: "strict Responses stream ended incomplete"}
	}
	return nil, err
}

func (run *attemptRun) emit(frame []byte) error {
	if run.guarded == nil {
		return run.deliver(frame)
	}
	frames, err := run.guarded.Push(run.sending, frame)
	if err != nil {
		return err
	}
	for _, released := range frames {
		if err := run.deliver(released); err != nil {
			return err
		}
	}
	return nil
}

func (run *attemptRun) flushGuarded() error {
	frames, err := run.guarded.Finish(run.sending)
	if err != nil {
		return err
	}
	for _, frame := range frames {
		if err := run.deliver(frame); err != nil {
			return err
		}
	}
	return nil
}

func (run *attemptRun) deliver(provided []byte) error {
	s, x := run.server, run.x
	if err := run.parallel.join(x, guardstream.Terminal(x.family, provided)); err != nil {
		return err
	}
	frame, err := run.clientFrame(provided)
	if err != nil {
		return err
	}
	run.heard()
	if len(frame) > int(s.Config.MaxEventBytes) {
		return wire.ErrEventTooLarge
	}
	err = x.generation.emit(frame)
	if err == nil && x.capture != nil {
		x.capture.CaptureStreamFrame(x.family, frame)
	}
	if run.fact.Interaction != nil {
		if run.fact.Interaction.ClientState == usage.ClientUnobserved {
			run.fact.Interaction.ClientState = usage.ClientPartial
		}
		if run.actionable {
			// A failed write can have exposed a complete call before losing
			// the remaining frame. Do not infer non-actionability from error.
			run.fact.Interaction.ClientState = usage.ClientActionable
		}
	}
	if err == nil && run.fact.FirstOutput == nil && protocols.MeaningfulFrame(x.family, frame) {
		elapsed := s.Now().Sub(run.fact.StartedAt)
		run.fact.FirstOutput = &elapsed
	}
	return err
}

func (run *attemptRun) clientFrame(provided []byte) ([]byte, error) {
	s, x := run.server, run.x
	frame := provided
	if x.providerState {
		mapped, err := s.mapStreamResponseFrame(run.ctx, x, &run.fact, frame, run.format != wire.Responses)
		if err != nil {
			return nil, err
		}
		frame = mapped
	}
	if x.family == wire.Responses {
		mapped, err := s.mapGenerationReferenceFrame(run.ctx, x, responseNativeIdentity(x, &run.fact, ""), frame)
		if err != nil {
			return nil, &responseLocalRefusal{cause: err}
		}
		frame = mapped
	}
	if run.format == wire.Responses && (bytes.Contains(frame, []byte("event: response.failed\n")) || bytes.Contains(frame, []byte("event: error\n"))) {
		return redactFailedResponseFrame(frame, x.sensitive)
	}
	return frame, nil
}

func (run *attemptRun) heard() {
	if run.committed {
		run.watchdog.Reset(run.target.Timeout)
		return
	}
	run.committed = true
	run.firstByte.Stop()
	run.watchdog = time.AfterFunc(run.target.Timeout, func() { run.state.reason.CompareAndSwap(0, 2); run.cancel() })
}

func (run *attemptRun) observer() func(oif.Event) error {
	stateful := run.format == wire.Responses && run.x.providerState
	if run.contract == nil && !stateful {
		return nil
	}
	return func(event oif.Event) error {
		if stateful {
			if err := run.server.observeStreamResponseEvent(run.ctx, run.x, &run.fact, event); err != nil {
				return err
			}
		}
		if run.contract == nil {
			return nil
		}
		return run.observeContract(event)
	}
}

func (run *attemptRun) observeContract(event oif.Event) error {
	if err := run.contract.ValidateEvent(event); err != nil {
		return err
	}
	if run.x.Strict() && run.x.family == wire.Responses {
		if kind, present := event.Source().Root().Lookup("type"); present {
			if text, ok := kind.Text(); ok && text == "response.incomplete" {
				run.incomplete = true
			}
		}
	}
	run.actionable = run.actionable || eventActionable(event)
	return nil
}

func (run *attemptRun) streamContinuation() error {
	s, x := run.server, run.x
	projection, err := run.contract.NewToolProjection(min(resources.MaxContinuationBytes, int(s.Config.MaxResponseBytes)))
	if err != nil {
		return err
	}
	run.completion, err = run.codec.StreamNative(run.reader.Stream(run.response.Body), int(s.Config.MaxEventBytes), func([]byte) error { return nil }, func(event oif.Event) error {
		frames, err := projection.Observe(event)
		if err != nil {
			return err
		}
		if run.watchdog != nil {
			run.watchdog.Reset(run.target.Timeout)
		}
		for _, frame := range frames {
			if err := run.emit(interaction.ContinuationFrame(frame)); err != nil {
				return err
			}
			x.generation.continuation.emitted++
		}
		return nil
	})
	if err != nil {
		return err
	}
	run.state.settled = true
	state, delivery, err := projection.Complete(run.completion, x.generation.continuation.resource.ID)
	if err != nil {
		return err
	}
	if err := s.validateToolDelivery(delivery); err != nil {
		return err
	}
	if err := s.commitToolDelivery(run.ctx, x, state, delivery); err != nil {
		return err
	}
	for _, frame := range delivery.Frames[x.generation.continuation.emitted:] {
		run.actionable = run.actionable || interaction.ContinuationActionable(frame)
		if err := run.emit(interaction.ContinuationFrame(frame)); err != nil {
			return err
		}
	}
	return run.emit([]byte("data: [DONE]\n\n"))
}
