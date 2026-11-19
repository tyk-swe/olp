package kernel

import (
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/upstream"
)

func (run *attemptRun) read() error {
	read, err := run.reader.Read(run.response)
	if run.reader.Aggregated() {
		run.fact.ObserveUsage(read.Usage, read.UsageComplete)
	}
	if err != nil {
		return err
	}
	native, err := run.codec.Native(read.Body)
	if native != nil {
		if run.format == wire.Responses {
			run.fact.ObserveUsage(native.Usage, native.UsageComplete)
		} else if !run.reader.Aggregated() {
			run.fact.Usage = native.Usage
		}
	}
	if err != nil {
		return err
	}
	if run.contract != nil {
		if err := run.readContract(native); err != nil {
			return err
		}
	}
	if run.completion != nil {
		return nil
	}
	run.completion, err = run.codec.ProjectDecoded(read.Body, native)
	return err
}

func (run *attemptRun) readContract(native *wire.Completion) error {
	s, x := run.server, run.x
	run.state.settled = true
	if err := run.contract.ValidateResult(native.Native); err != nil {
		return err
	}
	if run.contract.ToolContinuation() {
		state, delivery, err := run.contract.ProjectUnary(native, x.generation.continuation.resource.ID, min(resources.MaxContinuationBytes, int(s.Config.MaxResponseBytes)))
		if err != nil {
			return err
		}
		if err := s.validateToolDelivery(delivery); err != nil {
			return err
		}
		if err := s.commitToolDelivery(run.ctx, x, state, delivery); err != nil {
			return err
		}
		run.completion = native
		run.completion.Body = delivery.Body
	}
	if run.format != x.family {
		return nil
	}
	run.completion = native
	if !x.Strict() || x.family != wire.Responses {
		return nil
	}
	source := native.Native.Source()
	if _, present := source.Lookup("/model"); !present {
		run.completion.Body = source.Bytes()
		return nil
	}
	model, _ := json.Marshal(x.named().Slug)
	published, err := oif.Apply(source, []oif.Change{{Pointer: "/model", Value: string(model), Origin: oif.IdentityBinding, Reason: "published response model"}})
	if err != nil {
		return &responseLocalRefusal{cause: err}
	}
	run.completion.Body = published.Bytes()
	return nil
}

func (run *attemptRun) inspect(received error) error {
	x := run.x
	err := received
	if guardErr := run.parallel.join(x, true); guardErr != nil {
		err = guardErr
	}
	if err == nil && run.completion != nil && !x.generation.parsed.Stream && x.guard.engine != nil {
		run.completion.Body, err = x.inspectGuardrailOutput(run.completion.Body)
	}
	if run.completion != nil && run.recordCompletion() {
		return ErrResponseTooLarge
	}
	return err
}

func (run *attemptRun) recordCompletion() (tooLarge bool) {
	x, completion := run.x, run.completion
	if x.generation.parsed.Stream || run.format == wire.Responses {
		run.fact.ObserveUsage(completion.Usage, completion.UsageComplete)
	} else {
		run.fact.Usage = completion.Usage
	}
	run.fact.ResponseModel = completion.ProviderModel
	if completion.FinishReason != "" {
		run.fact.FinishReasons = []string{completion.FinishReason}
	}
	projected := run.format != x.family || x.family == wire.Embeddings || x.family == wire.Rerank
	return !x.generation.parsed.Stream && projected && int64(len(completion.Body)) > run.server.Config.MaxResponseBytes
}

func (run *attemptRun) failed(err error) *attemptEnd {
	if err == nil {
		return nil
	}
	x, st := run.x, run.state
	f := &AttemptFailure{Status: run.response.StatusCode, committed: run.committed}
	if errors.Is(err, fault.ErrClientWrite) {
		if terminal, nativeFailure := run.completion.NativeResponseOutcome(); terminal {
			// The native result arrived before its client write failed.
			// Keep that provider outcome independent of the interruption
			// returned to the caller, including a native failure payload.
			st.settled = true
			// A retained create still owns accepted work even when its
			// terminal result never reached the original caller.
			f.noRetry = run.retained && st.dispatched.Load()
			run.deliveryClass = ClassSuccess
			if nativeFailure != nil {
				run.deliveryClass = string(st.classifier.Classify(upstream.Evidence{Settled: true, Error: nativeFailure}).Class)
			}
			return ended(st.classify(err, run.committed), f)
		}
	}
	_, localMapping := errors.AsType[*responseLocalRefusal](err)
	if localMapping || openai.IsResponseProjectionError(err) {
		return run.refusedLocally(f)
	}
	var guardFailure guardrails.Failure
	var ue *wire.UpstreamError
	var violation *oif.Incompatibility
	switch {
	case errors.As(err, &guardFailure):
		f.policyCode = string(guardFailure)
		f.overall = true
		f.noRetry = true
		return ended(classPolicy, f)
	case errors.As(err, &violation):
		f.contractCode = "fidelity_protocol_violation"
		return ended(ClassProtocol, f)
	case errors.Is(err, ErrResponseTooLarge):
		return ended(ClassProtocol, f)
	case errors.Is(err, wire.ErrAggregateTooLarge):
		f.aggregateTooLarge = true
		return ended(ClassProtocol, f)
	case errors.As(err, &ue):
		f.upstream = x.redacted(ue)
		class := st.classify(err, run.committed)
		f.Status = inBandStatus(class)
		return ended(class, f)
	}
	return ended(st.classify(err, run.committed), f)
}

func (run *attemptRun) refusedLocally(f *AttemptFailure) *attemptEnd {
	st := run.state
	run.fact.LocalRefusal = true
	f.overall, f.noRetry = true, st.dispatched.Load()
	terminal, nativeFailure := run.completion.NativeResponseOutcome()
	st.settled = st.settled || terminal
	if nativeFailure == nil {
		return ended(classAmbiguous, f)
	}
	run.fact.LocalRefusal = false
	// A known failed stateless generation remains retryable. An
	// accepted retained create keeps its original resource owner.
	f.overall = run.retained || run.fact.GenerationUsageDeferred
	f.noRetry = f.overall && st.dispatched.Load()
	f.upstream = run.x.redacted(nativeFailure)
	class := st.classify(nativeFailure, run.committed)
	f.Status = inBandStatus(class)
	return ended(class, f)
}
