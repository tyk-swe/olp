package geminiinteractions

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/domain/geminilifecycle"
	"github.com/tyk-swe/olp/internal/domain/protocols/sse"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

type binding struct {
	store    bool
	parent   *resources.Resource
	existing *resources.Resource
	upstream string
	resumed  bool
}

func (b binding) previousID() string {
	if b.parent != nil {
		return b.parent.ID
	}
	if b.existing != nil && b.existing.ParentID != nil {
		return resources.LocalID(resources.KindInteraction, *b.existing.ParentID)
	}
	return ""
}

type relay struct {
	turn     turn
	pin      *kernel.Pin
	exchange *kernel.PinnedExchange
	binding  binding
	state    *geminilifecycle.InteractionStream
	limit    int
}

type relayed struct {
	local     *resources.Resource
	localID   string
	committed bool
}

func (b binding) unrelayed() relayed {
	if b.existing != nil {
		return relayed{local: b.existing, localID: b.existing.ID}
	}
	return relayed{localID: b.upstream}
}

type localRefusal struct{ cause error }

func (e *localRefusal) Error() string { return "interaction projection or persistence failed" }

func (e *localRefusal) Unwrap() error { return e.cause }

func (t turn) relay(ctx context.Context, pin *kernel.Pin, response *kernel.PinnedResponse, b binding) (bool, error) {
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "text/event-stream" {
		return false, errors.New("provider did not return Interaction SSE")
	}
	r := t.newRelay(pin, response.Exchange, b)
	seen := b.unrelayed()
	err = sse.Decode(response.Body, r.limit, func(frame sse.Frame) error {
		next, err := r.forward(ctx, frame, seen)
		seen = next
		return err
	})
	if err == nil && r.state.Done {
		return seen.committed, nil
	}
	r.interrupt(ctx, err)
	if err == nil {
		return seen.committed, errors.New("Interaction SSE ended before terminal event")
	}
	return seen.committed, err
}

func (t turn) newRelay(pin *kernel.Pin, exchange *kernel.PinnedExchange, b binding) relay {
	return relay{
		turn: t, pin: pin, exchange: exchange, binding: b, limit: cmp.Or(max(int(t.kernel.Config.MaxEventBytes), 0), 1<<20),
		state: &geminilifecycle.InteractionStream{ID: b.upstream, Created: b.resumed, Resumed: b.resumed},
	}
}

func (r relay) forward(ctx context.Context, frame sse.Frame, seen relayed) (relayed, error) {
	event, err := r.accept(frame)
	if err != nil {
		return seen, err
	}
	named := r.account(event, seen)
	bound, err := r.persist(ctx, event, named)
	if err != nil {
		return named, &localRefusal{cause: err}
	}
	projected, err := r.project(event, bound)
	if err != nil {
		return bound, err
	}
	output := encode(frame, projected)
	if len(output) > r.limit {
		return bound, errors.New("projected Interaction event exceeds byte limit")
	}
	return relayed{local: bound.local, localID: bound.localID, committed: true}, r.write(output, event, bound.committed)
}

func (r *relay) accept(frame sse.Frame) (geminilifecycle.InteractionEvent, error) {
	event, err := geminilifecycle.ParseInteractionEvent([]byte(frame.Data), r.limit)
	if err != nil {
		r.exchange.InvalidNative()
		return geminilifecycle.InteractionEvent{}, err
	}
	if err := r.state.Accept(event); err != nil {
		r.exchange.InvalidNative()
		return geminilifecycle.InteractionEvent{}, err
	}
	if r.binding.upstream != "" && r.state.ID != r.binding.upstream {
		r.exchange.InvalidNative()
		return geminilifecycle.InteractionEvent{}, errors.New("provider changed Interaction resource identity")
	}
	return event, nil
}

func (r relay) account(event geminilifecycle.InteractionEvent, seen relayed) relayed {
	if object, ok := event.Source.Root().Lookup("interaction"); ok {
		r.turn.recordInteractionUsage(r.exchange, object)
	}
	if r.state.Done {
		r.complete(event)
	}
	if !r.binding.store && seen.local == nil {
		return relayed{localID: r.state.ID, committed: seen.committed}
	}
	return seen
}

func (r relay) complete(event geminilifecycle.InteractionEvent) {
	x := r.turn.x
	if !x.ResourceCall() && failed(event) {
		r.exchange.Complete(kernel.ClassUpstreamServer)
		x.Failure = generationFailure()
		return
	}
	r.exchange.Complete(kernel.ClassSuccess)
}

func (r relay) persist(ctx context.Context, event geminilifecycle.InteractionEvent, seen relayed) (relayed, error) {
	bound, err := r.bind(ctx, event, seen)
	if err != nil {
		return seen, err
	}
	observed := event.Status
	if event.Type == "error" {
		observed = "failed"
	}
	if bound.local == nil || observed == "" {
		return bound, nil
	}
	object, _ := event.Source.Root().Lookup("interaction")
	commitCtx, stopCommit := kernel.ResourceCommitContext(ctx)
	defer stopCommit()
	err = r.turn.observeInteraction(commitCtx, r.turn.authority.ID, bound.local.ID, observed, object)
	return bound, err
}

func (r relay) bind(ctx context.Context, event geminilifecycle.InteractionEvent, seen relayed) (relayed, error) {
	if !r.binding.store || seen.local != nil {
		return seen, nil
	}
	t := r.turn
	local, err := t.kernel.StoreInteraction(ctx, r.exchange, r.pin, t.authority.ID, r.state.ID, cmp.Or(event.Status, "created"), r.binding.parent, !r.state.Done)
	if err != nil {
		return seen, err
	}
	return relayed{local: local, localID: local.ID, committed: seen.committed}, nil
}

func (r relay) project(event geminilifecycle.InteractionEvent, seen relayed) ([]byte, error) {
	source := event.Source
	if failed(event) {
		redacted, err := fault.RedactNativeFailureDocument(source, r.turn.x.Sensitive())
		if err != nil {
			return nil, err
		}
		source = redacted
	}
	safe := geminilifecycle.InteractionEvent{Source: source, Type: event.Type, ID: event.ID, Status: event.Status}
	projected, err := safe.WithProjection(seen.localID, r.binding.previousID(), r.turn.x.Route.Slug)
	if err != nil {
		return nil, err
	}
	if seen.local != nil && containsNativeResourceID(projected, r.state.ID) {
		return nil, errors.New("Interaction event contains an unbound native resource identity")
	}
	return projected, nil
}

func (r relay) write(output []byte, event geminilifecycle.InteractionEvent, committed bool) error {
	t := r.turn
	controller := http.NewResponseController(t.w)
	_ = controller.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	if !committed {
		t.w.Header().Set("Content-Type", "text/event-stream")
		t.w.Header().Set("Cache-Control", "no-store")
		t.x.ResponseHeaders(t.w.Header(), true)
		if !t.x.ResourceCall() {
			t.w.Header().Set("X-Should-Retry", "false")
		}
		t.w.WriteHeader(http.StatusOK)
		r.exchange.Delivery(usage.ClientUnobserved)
	}
	if n, err := t.w.Write(output); err != nil || n != len(output) {
		if n > 0 {
			r.exchange.Delivery(usage.ClientPartial)
		}
		return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
	}
	if err := controller.Flush(); err != nil {
		return fmt.Errorf("%w: %v", fault.ErrClientWrite, err)
	}
	t.x.Delivered(t.kernel.Now())
	switch {
	case r.state.Done:
		r.exchange.Delivery(usage.ClientTerminal)
	case event.Type == "step.stop":
		r.exchange.Delivery(usage.ClientActionable)
	default:
		r.exchange.Delivery(usage.ClientPartial)
	}
	return nil
}

func (r relay) interrupt(ctx context.Context, err error) {
	_, local := errors.AsType[*localRefusal](err)
	switch {
	case errors.Is(err, fault.ErrClientWrite), errors.Is(ctx.Err(), context.Canceled):
		r.exchange.Interrupt(kernel.ClassCancelled, true)
	case errors.Is(ctx.Err(), context.DeadlineExceeded) && !local:
		r.exchange.Interrupt(kernel.ClassTimeout, false)
	default:
		r.exchange.Interrupt(kernel.ClassProtocol, local)
	}
}

func failed(event geminilifecycle.InteractionEvent) bool {
	return event.Type == "error" || event.Status == "failed"
}

func encode(frame sse.Frame, data []byte) []byte {
	var output bytes.Buffer
	if frame.ID != nil {
		output.WriteString("id: ")
		output.WriteString(*frame.ID)
		output.WriteByte('\n')
	}
	if frame.Event != nil {
		output.WriteString("event: ")
		output.WriteString(*frame.Event)
		output.WriteByte('\n')
	}
	if frame.RetryMS != nil {
		output.WriteString("retry: ")
		output.WriteString(strconv.FormatUint(*frame.RetryMS, 10))
		output.WriteByte('\n')
	}
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		output.WriteString("data: ")
		output.Write(line)
		output.WriteByte('\n')
	}
	output.WriteByte('\n')
	return output.Bytes()
}
