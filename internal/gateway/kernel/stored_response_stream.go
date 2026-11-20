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
	projection responseProjection
	exchange   *PinnedExchange
	limit      int
	progress   *storedStreamProgress
}

// storedStreamProgress is the one value a stored stream writes: the codec
// calls emit and observe per event, and the ending reads what they saw.
type storedStreamProgress struct {
	committed, incomplete, failed bool
}

func (s *Server) streamStoredResponse(ctx context.Context, w http.ResponseWriter, x *Call, res *resources.Resource, p *Pin, query url.Values) *fault.Error {
	projection, refusal := storedStreamProjection(x, res, p)
	if refusal != nil {
		return refusal
	}
	endpoint, err := ResourceURL(p.Provider.Connector(), p.Model, responsePath(p.Provider.Connector(), "/"+url.PathEscape(res.UpstreamID)), query)
	if err != nil {
		return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address could not be resolved.")
	}
	x.mode = "streaming"
	resp, failure := s.PinnedDo(ctx, x, p, http.MethodGet, endpoint, nil, "")
	if failure != nil {
		if failure.Status == http.StatusNotFound {
			return fault.NotFoundError("not_found", "No stored response with this identifier exists for this key.")
		}
		return UpstreamError(failure)
	}
	defer resp.Body.Close()
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return fault.ServerError(http.StatusBadGateway, "fidelity_protocol_violation", "The provider did not return a Responses event stream.")
	}
	stream := storedStream{server: s, x: x, resource: res, projection: projection, exchange: resp.Exchange, limit: s.storedEventLimit(), progress: &storedStreamProgress{}}
	_, streamErr := protocols.StreamWithEvents(wire.Responses, wire.Responses,
		p.Provider.Connector().StreamPayload(resp.Body, stream.limit), stream.limit, x.Route.Slug, true,
		func(frame []byte) error { return stream.emit(ctx, w, frame) },
		func(event oif.Event) error { return stream.observe(ctx, event) })
	return stream.end(ctx, streamErr)
}

func storedStreamProjection(x *Call, res *resources.Resource, p *Pin) (responseProjection, *fault.Error) {
	projection := responseProjection{upstreamID: res.UpstreamID, localID: res.ID, route: res.RouteSlug}
	if res.Kind != resources.KindStrictResponse {
		return projection, nil
	}
	if !p.Provider.Supports(p.Model, "generation", "openai", "streaming") || !p.Provider.Connector().Supports("generation", "openai", "streaming") {
		return responseProjection{}, fault.InvalidRequest("target_capability", "The retained provider has no qualified Responses stream contract.", nil)
	}
	previousUpstream, previousLocal, err := responseParentProjection(res, x.generation.responseContract)
	if err != nil {
		return responseProjection{}, fault.ServerError(http.StatusConflict, "provider_resource_unavailable", "The retained parent response cannot be reconstructed.")
	}
	return responseProjection{upstreamID: res.UpstreamID, localID: res.ID, route: res.RouteSlug, previousUpstream: previousUpstream, previousLocal: previousLocal}, nil
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

func (t storedStream) observe(ctx context.Context, event oif.Event) error {
	observation, lifecycle, err := openai.ObserveResponseEventUsage(event)
	if err != nil {
		return err
	}
	if !lifecycle {
		return nil
	}
	if err := t.identified(event); err != nil {
		return err
	}
	t.settle(observation.Status)
	commitCtx, stopCommit := ResourceCommitContext(ctx)
	defer stopCommit()
	if t.server.applyResponseObservation(commitCtx, t.resource.ID, observation) != nil {
		return &responseLocalRefusal{cause: errors.New("retained response observation could not be committed")}
	}
	return nil
}

func (t storedStream) identified(event oif.Event) error {
	if t.strict() {
		_, err := validateNativeResponse(event.Source(), "/response", t.resource.UpstreamID)
		return err
	}
	id, present := event.Source().Lookup("/response/id")
	upstreamID, valid := id.Text()
	if !present || !valid || upstreamID != t.resource.UpstreamID {
		return errors.New("retained response observation has a different identifier")
	}
	return nil
}

func (t storedStream) settle(status string) {
	if status == "incomplete" {
		t.progress.incomplete = true
	}
	if status == "failed" {
		t.progress.failed = true
	}
	switch {
	case t.progress.failed:
		t.exchange.Complete(ClassUpstreamServer)
	case t.progress.incomplete && t.strict():
		t.exchange.Complete(ClassProtocol)
	case wire.ResponseStatusFinished(status):
		t.exchange.Complete(ClassSuccess)
	}
}

func (t storedStream) emit(ctx context.Context, w http.ResponseWriter, frame []byte) error {
	projected, err := t.project(ctx, frame)
	if err != nil {
		return err
	}
	if !t.progress.committed {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		t.x.ResponseHeaders(w.Header(), true)
		w.WriteHeader(http.StatusOK)
		t.progress.committed = true
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
		return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
	}
	t.x.Delivered(t.server.Now())
	t.exchange.Delivery(usage.ClientPartial)
	return nil
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

func (t storedStream) end(ctx context.Context, streamErr error) *fault.Error {
	switch {
	case streamErr != nil && (errors.Is(streamErr, fault.ErrClientWrite) || errors.Is(ctx.Err(), context.Canceled)):
		// The client left; the provider is not at fault.
		t.x.Failure = (&AttemptFailure{class: ClassCancelled}).Fault()
		t.exchange.Interrupt(ClassCancelled, true)
		t.server.Finish(t.x, &Outcome{Err: t.x.Failure, Committed: t.progress.committed, Cancelled: true}, 0)
		return nil
	case streamErr != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		t.x.Failure = (&AttemptFailure{class: ClassTimeout}).Fault()
		t.exchange.Interrupt(ClassTimeout, false)
		return t.undelivered()
	case streamErr != nil:
		t.x.Failure = t.broken(streamErr)
		return t.undelivered()
	case t.progress.incomplete && t.strict():
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

func (t storedStream) broken(streamErr error) *fault.Error {
	if _, upstream := errors.AsType[*wire.UpstreamError](streamErr); upstream && t.progress.failed {
		return fault.ServerError(http.StatusBadGateway, "upstream_response_failed", "The retained provider reported a failed response.")
	}
	_, local := errors.AsType[*responseLocalRefusal](streamErr)
	t.exchange.Interrupt(ClassProtocol, local)
	return fault.ServerError(http.StatusBadGateway, "response_stream_incomplete", "The retained response stream ended before its terminal contract.")
}

func (t storedStream) undelivered() *fault.Error {
	if !t.progress.committed {
		return t.x.Failure
	}
	t.server.Finish(t.x, &Outcome{Err: t.x.Failure, Committed: true}, http.StatusOK)
	return nil
}
