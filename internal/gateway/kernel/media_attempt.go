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
	fact     events.AttemptFact
	traced   context.Context
	trace    *telemetry.AttemptTrace
	encoded  encodedMedia

	sending context.Context
	cancel  context.CancelFunc
}

func (s *Server) mediaAttempt(ctx context.Context, w http.ResponseWriter, x *Call, a routing.Attempt, provider *routing.Provider, slot routing.Slot, fact events.AttemptFact) (events.AttemptFact, *transport.Result, *AttemptFailure) {
	run := &mediaRun{server: s, ctx: ctx, w: w, x: x, target: a, provider: provider, slot: slot, fact: fact}
	run.traced, run.trace = x.request.trace.Attempt(ctx, string(provider.Kind), a.ProviderRevisionID, a.UpstreamModel)
	defer run.close()
	result, end := run.exchange()
	if end != nil {
		return run.fail(end.class, end.failure)
	}
	return run.succeed(result)
}

func (run *mediaRun) exchange() (*transport.Result, *attemptEnd) {
	result, end := run.send()
	if end != nil {
		return nil, end
	}
	if end := run.verify(result); end != nil {
		return nil, end
	}
	return result, run.relay(result)
}

func (run *mediaRun) close() {
	if run.cancel != nil {
		run.cancel()
	}
}

func (run *mediaRun) send() (*transport.Result, *attemptEnd) {
	s, x := run.server, run.x
	encoded, err := x.encodeMedia(x.request.release.Snapshot(), *run.provider, run.target.TargetID, run.target.UpstreamModel)
	if err != nil {
		return nil, ended(ClassProtocol, nil)
	}
	run.encoded = encoded
	cfg := run.provider.Connector()
	deadline, _ := run.ctx.Deadline()
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil, ended(ClassTimeout, &AttemptFailure{overall: true})
	}
	run.sending, run.cancel = context.WithTimeout(run.traced, min(run.target.Timeout, remaining))

	secret, err := s.slotSecret(run.sending, x, run.slot, run.provider, run.target.UpstreamModel)
	if err != nil {
		return nil, ended(classCredential, nil)
	}
	headers := http.Header{}
	run.trace.InjectUpstream(headers)
	networkSecret, err := s.providerNetworkSecret(run.sending, x.request.release, run.provider)
	if err != nil {
		return nil, ended(classCredential, nil)
	}
	target := transport.Target{Sensitive: &x.sensitive, CallerCredential: run.provider.CredentialSource == "caller" && x.origin != "probe", Config: cfg, Model: cfg.Model(run.target.UpstreamModel), Secret: secret, NetworkSecret: networkSecret, ConnectionScope: providerConnectionScope(run.provider, run.slot)}
	result, failure := s.Deps.Media.Transport.Do(run.sending, target, encoded.call, transport.Options{Strict: encoded.template != nil, Headers: headers})
	if failure != nil {
		run.fact.Status = failure.Status
		return nil, ended(mediaClass(failure), &AttemptFailure{
			Status:     failure.Status,
			retryAfter: failure.RetryAfter,
			upstream:   failure.Upstream,
			dispatched: failure.Dispatched,
		})
	}
	run.fact.Status = result.Status
	return result, nil
}

func (run *mediaRun) verify(result *transport.Result) *attemptEnd {
	strict := run.encoded.template
	if artifact := mediaArtifact(result.Payload); strict != nil && artifact != nil {
		if _, err := artifact.BlobReference(); err != nil {
			return ended(ClassProtocol, &AttemptFailure{dispatched: true})
		}
	}
	if strict == nil || !result.Source.Valid() {
		return nil
	}
	if _, err := strict.JSONResult(run.encoded.bound.Descriptor, result.Source); err != nil {
		if images, ok := result.Payload.(transport.Images); ok {
			run.server.removeImages(images.ImageResult)
		}
		class := ClassProtocol
		if run.encoded.call.Ambiguous {
			class = classAmbiguous
		}
		return ended(class, &AttemptFailure{dispatched: true})
	}
	return nil
}

func (run *mediaRun) relay(result *transport.Result) *attemptEnd {
	fact := &run.fact
	fb := result.FirstByte
	fact.FirstByte = &fb
	fact.Class = "success"
	fact.Usage = mediaUsage(run.encoded.effective, result)
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = upstream.Terminal
	}
	events, ok := result.Payload.(transport.Events)
	if !ok {
		return nil
	}
	var failure *AttemptFailure
	fact.Usage, fact.Committed, failure = run.server.streamMediaEvents(run.sending, run.w, run.x, events.Body, run.encoded.template, run.encoded.bound)
	if failure != nil {
		if run.encoded.call.Ambiguous && !failure.committed && failure.class == ClassTimeout {
			failure.class = classAmbiguous
		}
		return ended(failure.class, failure)
	}
	if fact.Interaction != nil {
		fact.Interaction.ClientState = usage.ClientTerminal
	}
	return nil
}

func (run *mediaRun) finishTrace() {
	if run.trace == nil {
		return
	}
	if u := run.fact.Usage; u != nil {
		run.trace.RecordUsage(u.InputTokens, u.OutputTokens, u.CachedInputTokens, u.MediaUnits)
	}
	run.trace.Finish(run.fact.Class, run.fact.Status)
}

func (run *mediaRun) fail(class string, f *AttemptFailure) (events.AttemptFact, *transport.Result, *AttemptFailure) {
	fact := &run.fact
	if f == nil {
		f = &AttemptFailure{}
	}
	f.class = class
	fact.Class = class
	fact.Committed = f.committed
	f.accepted = fact.Status == http.StatusOK
	f.Acceptance = upstream.Evidence{Reached: f.dispatched, Status: f.Status, Accepted: f.accepted}.Acceptance()
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = f.Acceptance
		if f.committed {
			fact.Interaction.ClientState = usage.ClientPartial
		}
	}
	fact.Duration = run.server.Now().Sub(fact.StartedAt)
	if f.retryAfter > 0 {
		retry := f.retryAfter
		fact.RetryAfter = &retry
	}
	fact.RecordEvidence(f.billingUncertain())
	run.finishTrace()
	return run.fact, nil, f
}

func (run *mediaRun) succeed(result *transport.Result) (events.AttemptFact, *transport.Result, *AttemptFailure) {
	run.fact.Duration = run.server.Now().Sub(run.fact.StartedAt)
	run.fact.RecordEvidence(true)
	run.finishTrace()
	return run.fact, result, nil
}
