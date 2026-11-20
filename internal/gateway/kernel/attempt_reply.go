package kernel

import (
	"encoding/json"
	"errors"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/upstream"
)

func (run attemptRun) read(sent attemptSent) (attemptReply, error) {
	plan := sent.plan
	read, err := plan.reader.Read(sent.response)
	fact := sent.fact
	if plan.reader.Aggregated() {
		fact.ObserveUsage(read.Usage, read.UsageComplete)
	}
	if err != nil {
		return attemptReply{fact: fact}, err
	}
	native, err := plan.codec.Native(read.Body)
	if native != nil {
		if plan.format == wire.Responses {
			fact.ObserveUsage(native.Usage, native.UsageComplete)
		} else if !plan.reader.Aggregated() {
			fact.Usage = native.Usage
		}
	}
	if err != nil {
		return attemptReply{fact: fact}, err
	}
	if plan.contract != nil {
		completion, err := run.readContract(plan, native)
		if err != nil || completion != nil {
			return attemptReply{fact: fact, completion: completion}, err
		}
	}
	completion, err := plan.codec.ProjectDecoded(read.Body, native)
	return attemptReply{fact: fact, completion: completion}, err
}

func (run attemptRun) readContract(plan attemptPlan, native *wire.Completion) (*wire.Completion, error) {
	x := run.x
	run.state.settled = true
	if err := plan.contract.ValidateResult(native.Native); err != nil {
		return nil, err
	}
	continued, err := run.continued(plan, native)
	if err != nil {
		return nil, err
	}
	if plan.format != x.family {
		return continued, nil
	}
	if !x.Strict() || x.family != wire.Responses {
		return native, nil
	}
	source := native.Native.Source()
	if _, present := source.Lookup("/model"); !present {
		native.Body = source.Bytes()
		return native, nil
	}
	model, _ := json.Marshal(x.named().Slug)
	published, err := oif.Apply(source, []oif.Change{{Pointer: "/model", Value: string(model), Origin: oif.IdentityBinding, Reason: "published response model"}})
	if err != nil {
		return native, &responseLocalRefusal{cause: err}
	}
	native.Body = published.Bytes()
	return native, nil
}

func (run attemptRun) continued(plan attemptPlan, native *wire.Completion) (*wire.Completion, error) {
	s, x := run.server, run.x
	if !plan.contract.ToolContinuation() {
		return nil, nil
	}
	state, delivery, err := plan.contract.ProjectUnary(native, x.generation.continuation.resource.ID, min(resources.MaxContinuationBytes, int(s.Config.MaxResponseBytes)))
	if err != nil {
		return nil, err
	}
	if err := s.validateToolDelivery(delivery); err != nil {
		return nil, err
	}
	if err := s.commitToolDelivery(run.ctx, x, state, delivery); err != nil {
		return nil, err
	}
	native.Body = delivery.Body
	return native, nil
}

func (run attemptRun) inspect(sent attemptSent, reply attemptReply, received error) (attemptReply, error) {
	x := run.x
	err := received
	if guardErr := sent.parallel.join(x, true); guardErr != nil {
		err = guardErr
	}
	if reply.completion == nil {
		return reply, err
	}
	if err == nil && !x.generation.parsed.Stream && x.guard.engine != nil {
		reply.completion.Body, err = x.inspectGuardrailOutput(reply.completion.Body)
	}
	fact, tooLarge := run.recordCompletion(sent.plan, reply)
	recorded := attemptReply{fact: fact, completion: reply.completion, committed: reply.committed}
	if tooLarge {
		return recorded, ErrResponseTooLarge
	}
	return recorded, err
}

func (run attemptRun) recordCompletion(plan attemptPlan, reply attemptReply) (events.AttemptFact, bool) {
	x, completion := run.x, reply.completion
	fact := reply.fact
	if x.generation.parsed.Stream || plan.format == wire.Responses {
		fact.ObserveUsage(completion.Usage, completion.UsageComplete)
	} else {
		fact.Usage = completion.Usage
	}
	fact.ResponseModel = completion.ProviderModel
	if completion.FinishReason != "" {
		fact.FinishReasons = []string{completion.FinishReason}
	}
	projected := plan.format != x.family || x.family == wire.Embeddings || x.family == wire.Rerank
	return fact, !x.generation.parsed.Stream && projected && int64(len(completion.Body)) > run.server.Config.MaxResponseBytes
}

func (run attemptRun) failed(reply attemptReply, status int, err error) (attemptReply, *attemptEnd) {
	if err == nil {
		return reply, nil
	}
	x, st := run.x, run.state
	f := &AttemptFailure{Status: status, committed: reply.committed}
	if errors.Is(err, fault.ErrClientWrite) {
		if terminal, nativeFailure := reply.completion.NativeResponseOutcome(); terminal {
			// The native result arrived before its client write failed.
			// Keep that provider outcome independent of the interruption
			// returned to the caller, including a native failure payload.
			st.settled = true
			// A retained create still owns accepted work even when its
			// terminal result never reached the original caller.
			f.noRetry = run.retained && st.dispatched.Load()
			delivered := ClassSuccess
			if nativeFailure != nil {
				delivered = string(st.classifier.Classify(upstream.Evidence{Settled: true, Error: nativeFailure}).Class)
			}
			return reply, &attemptEnd{class: st.classify(err, reply.committed), failure: f, delivered: delivered}
		}
	}
	_, localMapping := errors.AsType[*responseLocalRefusal](err)
	if localMapping || openai.IsResponseProjectionError(err) {
		return run.refusedLocally(reply, f)
	}
	var guardFailure guardrails.Failure
	var ue *wire.UpstreamError
	var violation *oif.Incompatibility
	switch {
	case errors.As(err, &guardFailure):
		f.policyCode = string(guardFailure)
		f.overall = true
		f.noRetry = true
		return reply, ended(classPolicy, f)
	case errors.As(err, &violation):
		f.contractCode = "fidelity_protocol_violation"
		return reply, ended(ClassProtocol, f)
	case errors.Is(err, ErrResponseTooLarge):
		return reply, ended(ClassProtocol, f)
	case errors.Is(err, wire.ErrAggregateTooLarge):
		f.aggregateTooLarge = true
		return reply, ended(ClassProtocol, f)
	case errors.As(err, &ue):
		f.upstream = x.redacted(ue)
		class := st.classify(err, reply.committed)
		f.Status = inBandStatus(class)
		return reply, ended(class, f)
	}
	return reply, ended(st.classify(err, reply.committed), f)
}

func (run attemptRun) refusedLocally(reply attemptReply, f *AttemptFailure) (attemptReply, *attemptEnd) {
	st := run.state
	terminal, nativeFailure := reply.completion.NativeResponseOutcome()
	st.settled = st.settled || terminal
	refused := reply.fact
	refused.LocalRefusal = nativeFailure == nil
	f.overall, f.noRetry = true, st.dispatched.Load()
	if nativeFailure == nil {
		return attemptReply{fact: refused, completion: reply.completion, committed: reply.committed}, ended(ClassAmbiguous, f)
	}
	// A known failed stateless generation remains retryable. An
	// accepted retained create keeps its original resource owner.
	f.overall = run.retained || refused.GenerationUsageDeferred
	f.noRetry = f.overall && st.dispatched.Load()
	f.upstream = run.x.redacted(nativeFailure)
	class := st.classify(nativeFailure, reply.committed)
	f.Status = inBandStatus(class)
	return attemptReply{fact: refused, completion: reply.completion, committed: reply.committed}, ended(class, f)
}
