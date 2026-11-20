package kernel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

type storedStream struct {
	server     *Server
	x          *Call
	resource   *resources.Resource
	projection ResponseProjection
	exchange   *PinnedExchange
	limit      int
}

type storedStreamStatus struct {
	incomplete, failed bool
}

func (seen storedStreamStatus) after(status string) storedStreamStatus {
	return storedStreamStatus{incomplete: seen.incomplete || status == "incomplete", failed: seen.failed || status == "failed"}
}

func (s *Server) StreamStoredResponse(ctx context.Context, w http.ResponseWriter, x *Call, res *resources.Resource, p *Pin, query url.Values) *fault.Error {
	projection, refusal := storedStreamProjection(x, res, p)
	if refusal != nil {
		return refusal
	}
	endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, ResponsePath(p.Provider.Connector(), "/"+url.PathEscape(res.UpstreamID)), query)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	x.mode = "streaming"
	resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		if failure.Status == http.StatusNotFound {
			commitCtx, stopCommit := ResourceCommitContext(ctx)
			defer stopCommit()
			if err := s.Deps.Resources.Tombstone(commitCtx, res.ID); err != nil && !errors.Is(err, resources.ErrNotFound) {
				return fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "The missing response state could not be recorded.")
			}
			return fault.NotFoundError("not_found", "No stored response with this identifier exists for this key.")
		}
		return UpstreamError(failure)
	}
	defer resp.Body.Close()
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider did not return a Responses event stream.")
	}
	stream := storedStream{server: s, x: x, resource: res, projection: projection, exchange: resp.Exchange, limit: s.storedEventLimit()}
	// The codec reports each event through a callback, so the two results the
	// ending needs are folded here and nowhere else.
	var committed bool
	var seen storedStreamStatus
	_, streamErr := protocols.StreamWithEvents(wire.Responses, wire.Responses,
		p.Provider.Connector().StreamPayload(resp.Body, stream.limit), stream.limit, x.Route.Slug, true,
		func(frame []byte) error {
			var err error
			committed, err = stream.emit(ctx, w, frame, committed)
			return err
		},
		func(event oif.Event) error {
			var err error
			seen, err = stream.observe(ctx, event, seen)
			return err
		})
	return stream.end(ctx, streamErr, committed, seen)
}

func storedStreamProjection(x *Call, res *resources.Resource, p *Pin) (ResponseProjection, *fault.Error) {
	projection := ResponseProjection{UpstreamID: res.UpstreamID, LocalID: res.ID, Route: res.RouteSlug}
	if res.Kind != resources.KindStrictResponse {
		return projection, nil
	}
	if !p.Provider.Supports(p.Model, "generation", "openai", "streaming") || !p.Provider.Connector().Supports("generation", "openai", "streaming") {
		return ResponseProjection{}, fault.InvalidRequest("target_capability", "The retained provider has no qualified Responses stream contract.", nil)
	}
	previousUpstream, previousLocal, err := ResponseParentProjection(res, x.generation.responseContract)
	if err != nil {
		return ResponseProjection{}, fault.ServerError(http.StatusConflict, "provider_resource_unavailable", "The retained parent response cannot be reconstructed.")
	}
	return ResponseProjection{UpstreamID: res.UpstreamID, LocalID: res.ID, Route: res.RouteSlug, PreviousUpstream: previousUpstream, PreviousLocal: previousLocal}, nil
}

func (s *Server) storedEventLimit() int {
	if limit := int(s.Config.MaxEventBytes); limit > 0 {
		return limit
	}
	return 1 << 20
}

func (t storedStream) strict() bool {
	return t.resource.Kind == resources.KindStrictResponse
}

func (t storedStream) observe(ctx context.Context, event oif.Event, seen storedStreamStatus) (storedStreamStatus, error) {
	observation, lifecycle, err := openai.ObserveResponseEventUsage(event)
	if err != nil {
		return seen, err
	}
	if !lifecycle {
		return seen, nil
	}
	if err := t.identified(event); err != nil {
		return seen, err
	}
	now := seen.after(observation.Status)
	t.settle(now, observation.Status)
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	defer stopCommit()
	if t.server.ApplyResponseObservation(commitCtx, t.resource.ID, observation) != nil {
		return now, &responseLocalRefusal{cause: errors.New("retained response observation could not be committed")}
	}
	return now, nil
}

func (t storedStream) identified(event oif.Event) error {
	if t.strict() {
		_, err := ValidateNativeResponse(event.Source(), "/response", t.resource.UpstreamID)
		return err
	}
	id, present := event.Source().Lookup("/response/id")
	upstreamID, valid := id.Text()
	if !present || !valid || upstreamID != t.resource.UpstreamID {
		return errors.New("retained response observation has a different identifier")
	}
	return nil
}

func (t storedStream) settle(seen storedStreamStatus, status string) {
	switch {
	case seen.failed:
		t.exchange.Complete(ClassUpstreamServer)
	case seen.incomplete && t.strict():
		t.exchange.Complete(ClassProtocol)
	case wire.ResponseStatusFinished(status):
		t.exchange.Complete(ClassSuccess)
	}
}

func (t storedStream) emit(ctx context.Context, w http.ResponseWriter, frame []byte, committed bool) (bool, error) {
	projected, err := t.project(ctx, frame)
	if err != nil {
		return committed, err
	}
	if !committed {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		t.x.ResponseHeaders(w.Header(), true)
		w.WriteHeader(http.StatusOK)
		t.exchange.Delivery(usage.ClientUnobserved)
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	if n, err := w.Write(projected); err != nil || n != len(projected) {
		if n > 0 {
			t.exchange.Delivery(usage.ClientPartial)
		}
		if err == nil {
			err = io.ErrShortWrite
		}
		return true, fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return true, fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
	}
	t.x.Delivered(t.server.Now())
	t.exchange.Delivery(usage.ClientPartial)
	return true, nil
}

func (t storedStream) project(ctx context.Context, frame []byte) ([]byte, error) {
	projected, _, err := projectStoredResponseFrame(frame, t.projection, t.strict(), func(upstream string) string {
		lookupCtx, stopLookup := ResourceCommitContext(ctx)
		defer stopLookup()
		return t.server.previousResponseLocal(lookupCtx, t.resource.NativeIdentity(), nil, upstream)
	})
	if err != nil || len(projected) > t.limit {
		return nil, errors.New("retained response event could not be projected")
	}
	mapped, err := t.server.mapGenerationReferenceFrame(ctx, t.x, t.resource.NativeIdentity(), projected)
	if err != nil {
		return nil, &responseLocalRefusal{cause: err}
	}
	if len(mapped) > t.limit {
		return nil, &responseLocalRefusal{cause: errors.New("projected retained response event exceeds byte limit")}
	}
	if !bytes.Contains(mapped, []byte("event: response.failed\n")) && !bytes.Contains(mapped, []byte("event: error\n")) {
		return mapped, nil
	}
	redacted, err := redactFailedResponseFrame(mapped, t.x.sensitive)
	if err != nil {
		return nil, err
	}
	if len(redacted) > t.limit {
		return nil, errors.New("redacted response event exceeds byte limit")
	}
	return redacted, nil
}

func (t storedStream) end(ctx context.Context, streamErr error, committed bool, seen storedStreamStatus) *fault.Error {
	switch {
	case streamErr != nil && (errors.Is(streamErr, fault.ErrClientWrite) || errors.Is(ctx.Err(), context.Canceled)):
		// The client left; the provider is not at fault.
		t.x.Failure = (&AttemptFailure{class: ClassCancelled}).Fault()
		t.exchange.Interrupt(ClassCancelled, true)
		t.server.Finish(t.x, &Outcome{Err: t.x.Failure, Committed: committed, Cancelled: true}, 0)
		return nil
	case streamErr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		t.x.Failure = (&AttemptFailure{class: ClassTimeout}).Fault()
		t.exchange.Interrupt(ClassTimeout, false)
		return t.undelivered(committed)
	case streamErr != nil:
		t.x.Failure = t.broken(streamErr, seen)
		return t.undelivered(committed)
	case seen.incomplete && t.strict():
		t.x.Failure = fault.ServerError(http.StatusBadGateway, "response_incomplete", "The retained provider response ended incomplete.")
		t.exchange.Delivery(usage.ClientTerminal)
		t.server.Finish(t.x, &Outcome{Err: t.x.Failure, Committed: true}, http.StatusOK)
		return nil
	}
	t.exchange.Complete(ClassSuccess)
	t.exchange.Delivery(usage.ClientTerminal)
	t.server.Finish(t.x, &Outcome{Committed: true}, http.StatusOK)
	return nil
}

func (t storedStream) broken(streamErr error, seen storedStreamStatus) *fault.Error {
	if _, upstream := errors.AsType[*wire.UpstreamError](streamErr); upstream && seen.failed {
		return fault.ServerError(http.StatusBadGateway, "upstream_response_failed", "The retained provider reported a failed response.")
	}
	_, local := errors.AsType[*responseLocalRefusal](streamErr)
	t.exchange.Interrupt(ClassProtocol, local)
	return fault.ServerError(http.StatusBadGateway, "response_stream_incomplete", "The retained response stream ended before its terminal contract.")
}

func (t storedStream) undelivered(committed bool) *fault.Error {
	if !committed {
		return t.x.Failure
	}
	t.server.Finish(t.x, &Outcome{Err: t.x.Failure, Committed: true}, http.StatusOK)
	return nil
}
