package media

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

type mediaEnd struct {
	class   string
	spec    kernel.AttemptFailureSpec
	hasSpec bool
}

func mediaEnded(class string, spec *kernel.AttemptFailureSpec) *mediaEnd {
	if spec == nil {
		return &mediaEnd{class: class}
	}
	return &mediaEnd{class: class, spec: *spec, hasSpec: true}
}

type mediaRun struct {
	surface  surface
	ctx      context.Context
	w        http.ResponseWriter
	x        *kernel.Call
	request  *media.Request
	target   routing.Attempt
	provider *routing.Provider
	slot     routing.Slot
	traced   context.Context
	trace    *telemetry.AttemptTrace
}

type mediaSent struct {
	fact    events.AttemptFact
	encoded encodedMedia
	result  *transport.Result
	sending context.Context
	cancel  context.CancelFunc
}

func (sent mediaSent) release() {
	if sent.cancel != nil {
		sent.cancel()
	}
}

func (s surface) mediaAttempt(ctx context.Context, w http.ResponseWriter, x *kernel.Call, request *media.Request, a routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, *transport.Result, *kernel.AttemptFailure) {
	traced, trace := x.StartAttemptTrace(ctx, string(provider.Kind), a.ProviderRevisionID, a.UpstreamModel)
	run := mediaRun{surface: s, ctx: ctx, w: w, x: x, request: request, target: a, provider: provider, slot: slot, traced: traced, trace: trace}
	sent, end := run.send(fact)
	defer sent.release()
	if end != nil {
		return run.fail(sent.fact, end)
	}
	if end := run.verify(sent); end != nil {
		return run.fail(sent.fact, end)
	}
	relayed, end := run.relay(sent)
	if end != nil {
		return run.fail(relayed, end)
	}
	return run.succeed(relayed, sent.result)
}

func (run mediaRun) send(fact events.AttemptFact) (mediaSent, *mediaEnd) {
	k, x := run.surface.kernel, run.x
	encoded, err := encodeMedia(x, run.request, x.Snapshot(), *run.provider, run.target.TargetID, run.target.UpstreamModel)
	if err != nil {
		return mediaSent{fact: fact}, mediaEnded(kernel.ClassProtocol, nil)
	}
	cfg := run.provider.Connector()
	deadline, _ := run.ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return mediaSent{fact: fact, encoded: encoded}, mediaEnded(kernel.ClassTimeout, &kernel.AttemptFailureSpec{Overall: true})
	}
	sending, cancel := context.WithTimeout(run.traced, min(run.target.Timeout, remaining))
	unsent := mediaSent{fact: fact, encoded: encoded, sending: sending, cancel: cancel}

	secret, networkSecret, scope, err := k.ResolveSlotSecrets(sending, x, run.slot, run.provider, run.target.UpstreamModel)
	if err != nil {
		return unsent, mediaEnded(kernel.ClassCredential, nil)
	}
	headers := http.Header{}
	run.trace.InjectUpstream(headers)
	target := transport.Target{
		Sensitive:        x.SensitiveRef(),
		CallerCredential: x.IsCallerCredential(run.provider),
		Config:           cfg,
		Model:            cfg.Model(run.target.UpstreamModel),
		Secret:           secret,
		NetworkSecret:    networkSecret,
		ConnectionScope:  scope,
	}
	result, failure := run.surface.deps.Transport.Do(sending, target, encoded.call, transport.Options{Strict: encoded.template != nil, Headers: headers})
	answered := fact
	if failure != nil {
		answered.Status = failure.Status
		return mediaSent{fact: answered, encoded: encoded, sending: sending, cancel: cancel}, mediaEnded(mediaClass(failure), &kernel.AttemptFailureSpec{
			Status:     failure.Status,
			RetryAfter: failure.RetryAfter,
			Upstream:   failure.Upstream,
			Dispatched: failure.Dispatched,
		})
	}
	answered.Status = result.Status
	return mediaSent{fact: answered, encoded: encoded, result: result, sending: sending, cancel: cancel}, nil
}

func (run mediaRun) verify(sent mediaSent) *mediaEnd {
	strict, result := sent.encoded.template, sent.result
	if artifact := mediaArtifact(result.Payload); strict != nil && artifact != nil {
		if _, err := artifact.BlobReference(); err != nil {
			return mediaEnded(kernel.ClassProtocol, &kernel.AttemptFailureSpec{Dispatched: true})
		}
	}
	if strict == nil || !result.Source.Valid() {
		return nil
	}
	if _, err := strict.JSONResult(sent.encoded.bound.Descriptor, result.Source); err != nil {
		if images, ok := result.Payload.(transport.Images); ok {
			run.surface.removeImages(images.ImageResult)
		}
		if sent.encoded.call.Ambiguous {
			return mediaEnded(kernel.ClassAmbiguous, &kernel.AttemptFailureSpec{Dispatched: true})
		}
		return mediaEnded(kernel.ClassProtocol, &kernel.AttemptFailureSpec{Dispatched: true})
	}
	return nil
}

func (run mediaRun) relay(sent mediaSent) (events.AttemptFact, *mediaEnd) {
	result := sent.result
	first := result.FirstByte
	relayed := sent.fact
	relayed.FirstByte = &first
	relayed.Class = kernel.ClassSuccess
	relayed.Usage = mediaUsage(sent.encoded.effective, result)
	if relayed.Interaction != nil {
		relayed.Interaction.UpstreamState = upstream.Terminal
	}
	stream, ok := result.Payload.(transport.Events)
	if !ok {
		return relayed, nil
	}
	streamed, committed, failure := run.surface.streamMediaEvents(sent.sending, run.w, run.x, run.request, stream.Body, sent.encoded.template, sent.encoded.bound)
	relayed.Usage, relayed.Committed = streamed, committed
	if failure == nil {
		if relayed.Interaction != nil {
			relayed.Interaction.ClientState = usage.ClientTerminal
		}
		return relayed, nil
	}
	class := failure.Class()
	if sent.encoded.call.Ambiguous && !failure.Committed() && class == kernel.ClassTimeout {
		class = kernel.ClassAmbiguous
	}
	return relayed, mediaEnded(class, &kernel.AttemptFailureSpec{Committed: failure.Committed(), Dispatched: failure.Dispatched()})
}

func (run mediaRun) finishTrace(fact events.AttemptFact) {
	if run.trace == nil {
		return
	}
	if u := fact.Usage; u != nil {
		run.trace.RecordUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
	}
	run.trace.Finish(fact.Class, fact.Status)
}

func (run mediaRun) fail(fact events.AttemptFact, end *mediaEnd) (events.AttemptFact, *transport.Result, *kernel.AttemptFailure) {
	spec := end.spec
	spec.Class = end.class
	spec.Accepted = fact.Status == http.StatusOK
	spec.Acceptance = upstream.Evidence{Reached: spec.Dispatched, Status: spec.Status, Accepted: spec.Accepted}.Acceptance()
	f := kernel.NewAttemptFailure(spec)
	failed := fact
	failed.Class = end.class
	failed.Committed = spec.Committed
	if failed.Interaction != nil {
		failed.Interaction.UpstreamState = spec.Acceptance
		if spec.Committed {
			failed.Interaction.ClientState = usage.ClientPartial
		}
	}
	failed.Duration = run.surface.kernel.Now().Sub(fact.StartedAt)
	if spec.RetryAfter > 0 {
		retry := spec.RetryAfter
		failed.RetryAfter = &retry
	}
	failed.RecordEvidence(f.BillingUncertain())
	run.finishTrace(failed)
	return failed, nil, f
}

func (run mediaRun) succeed(fact events.AttemptFact, result *transport.Result) (events.AttemptFact, *transport.Result, *kernel.AttemptFailure) {
	done := fact
	done.Duration = run.surface.kernel.Now().Sub(fact.StartedAt)
	done.RecordEvidence(true)
	run.finishTrace(done)
	return done, result, nil
}

func mediaClass(f *transport.Failure) string {
	if f.Ambiguous {
		return kernel.ClassAmbiguous
	}
	return string(f.Class)
}
