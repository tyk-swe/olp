package kernel

import (
	"context"
	"net/http"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/telemetry"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage"
)

type mediaRun struct {
	server   *Server
	ctx      context.Context
	w        http.ResponseWriter
	x        *Call
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

func (s *Server) mediaAttempt(ctx context.Context, w http.ResponseWriter, x *Call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, *transport.Result, *AttemptFailure) {
	traced, trace := x.request.trace.Attempt(ctx, string(provider.Kind), a.ProviderRevisionID, a.UpstreamModel)
	run := mediaRun{server: s, ctx: ctx, w: w, x: x, target: a, provider: provider, slot: slot, traced: traced, trace: trace}
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

func (run mediaRun) send(fact events.AttemptFact) (mediaSent, *attemptEnd) {
	s, x := run.server, run.x
	encoded, err := x.encodeMedia(x.request.release.Snapshot(), *run.provider, run.target.TargetID, run.target.UpstreamModel)
	if err != nil {
		return mediaSent{fact: fact}, ended(ClassProtocol, nil)
	}
	cfg := run.provider.Connector()
	deadline, _ := run.ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return mediaSent{fact: fact, encoded: encoded}, ended(ClassTimeout, &AttemptFailure{overall: true})
	}
	sending, cancel := context.WithTimeout(run.traced, min(run.target.Timeout, remaining))
	unsent := mediaSent{fact: fact, encoded: encoded, sending: sending, cancel: cancel}

	secret, err := s.slotSecret(sending, x, run.slot, run.provider, run.target.UpstreamModel)
	if err != nil {
		return unsent, ended(classCredential, nil)
	}
	headers := http.Header{}
	run.trace.InjectUpstream(headers)
	networkSecret, err := s.providerNetworkSecret(sending, x.request.release, run.provider)
	if err != nil {
		return unsent, ended(classCredential, nil)
	}
	target := transport.Target{Sensitive: &x.sensitive, CallerCredential: run.provider.CredentialSource == "caller" && x.origin != "probe", Config: cfg, Model: cfg.Model(run.target.UpstreamModel), Secret: secret, NetworkSecret: networkSecret, ConnectionScope: providerConnectionScope(run.provider, run.slot)}
	result, failure := s.Deps.Media.Transport.Do(sending, target, encoded.call, transport.Options{Strict: encoded.template != nil, Headers: headers})
	answered := fact
	if failure != nil {
		answered.Status = failure.Status
		return mediaSent{fact: answered, encoded: encoded, sending: sending, cancel: cancel}, ended(mediaClass(failure), &AttemptFailure{
			Status:     failure.Status,
			retryAfter: failure.RetryAfter,
			upstream:   failure.Upstream,
			dispatched: failure.Dispatched,
		})
	}
	answered.Status = result.Status
	return mediaSent{fact: answered, encoded: encoded, result: result, sending: sending, cancel: cancel}, nil
}

func (run mediaRun) verify(sent mediaSent) *attemptEnd {
	strict, result := sent.encoded.template, sent.result
	if artifact := mediaArtifact(result.Payload); strict != nil && artifact != nil {
		if _, err := artifact.BlobReference(); err != nil {
			return ended(ClassProtocol, &AttemptFailure{dispatched: true})
		}
	}
	if strict == nil || !result.Source.Valid() {
		return nil
	}
	if _, err := strict.JSONResult(sent.encoded.bound.Descriptor, result.Source); err != nil {
		if images, ok := result.Payload.(transport.Images); ok {
			run.server.removeImages(images.ImageResult)
		}
		if sent.encoded.call.Ambiguous {
			return ended(classAmbiguous, &AttemptFailure{dispatched: true})
		}
		return ended(ClassProtocol, &AttemptFailure{dispatched: true})
	}
	return nil
}

func (run mediaRun) relay(sent mediaSent) (events.AttemptFact, *attemptEnd) {
	result := sent.result
	first := result.FirstByte
	relayed := sent.fact
	relayed.FirstByte = &first
	relayed.Class = "success"
	relayed.Usage = mediaUsage(sent.encoded.effective, result)
	if relayed.Interaction != nil {
		relayed.Interaction.UpstreamState = upstream.Terminal
	}
	stream, ok := result.Payload.(transport.Events)
	if !ok {
		return relayed, nil
	}
	streamed, committed, failure := run.server.streamMediaEvents(sent.sending, run.w, run.x, stream.Body, sent.encoded.template, sent.encoded.bound)
	relayed.Usage, relayed.Committed = streamed, committed
	if failure == nil {
		if relayed.Interaction != nil {
			relayed.Interaction.ClientState = usage.ClientTerminal
		}
		return relayed, nil
	}
	if sent.encoded.call.Ambiguous && !failure.committed && failure.class == ClassTimeout {
		return relayed, ended(classAmbiguous, failure)
	}
	return relayed, ended(failure.class, failure)
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

func (run mediaRun) fail(fact events.AttemptFact, end *attemptEnd) (events.AttemptFact, *transport.Result, *AttemptFailure) {
	f := AttemptFailure{}
	if end.failure != nil {
		f = *end.failure
	}
	f.class = end.class
	f.accepted = fact.Status == http.StatusOK
	f.Acceptance = upstream.Evidence{Reached: f.dispatched, Status: f.Status, Accepted: f.accepted}.Acceptance()
	failed := fact
	failed.Class = end.class
	failed.Committed = f.committed
	if failed.Interaction != nil {
		failed.Interaction.UpstreamState = f.Acceptance
		if f.committed {
			failed.Interaction.ClientState = usage.ClientPartial
		}
	}
	failed.Duration = run.server.Now().Sub(fact.StartedAt)
	if f.retryAfter > 0 {
		retry := f.retryAfter
		failed.RetryAfter = &retry
	}
	failed.RecordEvidence(f.billingUncertain())
	run.finishTrace(failed)
	return failed, nil, &f
}

func (run mediaRun) succeed(fact events.AttemptFact, result *transport.Result) (events.AttemptFact, *transport.Result, *AttemptFailure) {
	done := fact
	done.Duration = run.server.Now().Sub(fact.StartedAt)
	done.RecordEvidence(true)
	run.finishTrace(done)
	return done, result, nil
}
