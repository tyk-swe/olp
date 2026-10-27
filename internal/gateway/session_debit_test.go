package gateway

import (
	"context"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/limits"
)

type fakeSessionDebits struct {
	mu        sync.Mutex
	events    []string
	intents   []limits.DebitIntent
	unknown   bool
	admitErr  error
	prepareID string
}

func (f *fakeSessionDebits) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, name)
}

func (f *fakeSessionDebits) Prepare(_ context.Context, intent limits.DebitIntent, _ []limits.DebitOwner) (limits.DebitState, error) {
	f.mu.Lock()
	f.intents = append(f.intents, intent)
	f.prepareID = intent.ID
	f.mu.Unlock()
	f.record("prepare")
	return limits.DebitPrepared, nil
}

func (f *fakeSessionDebits) Admit(_ context.Context, _, _ string, _ []limits.Request) error {
	f.record("admit")
	return f.admitErr
}

func (f *fakeSessionDebits) MarkDispatched(_ context.Context, _, _ string) error {
	f.record("dispatched")
	return nil
}

func (f *fakeSessionDebits) MarkUnknown(_ context.Context, _, _ string) error {
	f.record("unknown")
	return nil
}

func (f *fakeSessionDebits) ReleaseUndispatched(_ context.Context, _, _ string) error {
	f.record("released")
	return nil
}

func (f *fakeSessionDebits) HasUnknownExposure(_ context.Context, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unknown, nil
}

func (f *fakeSessionDebits) Currency(_ context.Context) (string, error) { return "USD", nil }

func (f *fakeSessionDebits) order() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.events)
}

func TestWebRTCSessionReservesDurableDebitBeforeProviderExists(t *testing.T) {
	ledger := &fakeSessionDebits{}
	mock := newWebRTCMock("close")
	h := webrtcHarness(t, mock, true)
	h.gateway.Debits = ledger
	var dispatchedAtCreate bool
	h.mock.set("a", mock.handler(t, func() bool {
		dispatchedAtCreate = slices.Contains(ledger.order(), "dispatched")
		return h.gateway.webrtcSessionCount() > 0
	}))
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	if !dispatchedAtCreate {
		t.Fatal("provider session created before the durable debit was dispatched")
	}
	env := sessionEnvelope(t, waitEnvelopes(t, h, 1))
	fact := env.Attempts[len(env.Attempts)-1]
	if fact.Execution == nil || fact.Execution.DispatchID != ledger.prepareID || ledger.prepareID == "" {
		t.Fatalf("terminal fact carries no durable dispatch reference: %+v", fact.Execution)
	}
	if fact.BillingUncertain || !fact.UsageObserved {
		t.Fatalf("observed usage settlement changed under the debit ledger: %+v", fact)
	}
	want := []string{"prepare", "admit", "dispatched"}
	if got := ledger.order(); !slices.Equal(got, want) {
		t.Fatalf("ledger order = %v, want %v", got, want)
	}
}

func TestWebRTCUnresolvedDurableExposureRefusesNewSession(t *testing.T) {
	ledger := &fakeSessionDebits{unknown: true}
	mock := newWebRTCMock("usage")
	h := webrtcHarness(t, mock, true)
	h.gateway.Debits = ledger
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status=%d, want refusal against unresolved exposure", resp.StatusCode)
	}
	if creates, _, _ := mock.counts(); creates != 0 {
		t.Fatal("dispatched against unresolved durable exposure")
	}
	if got := ledger.order(); len(got) != 0 {
		t.Fatalf("ledger events = %v, refusal must precede any new reservation", got)
	}
}

func TestWebRTCDebitAdmissionRefusalDispatchesNothingAndReleases(t *testing.T) {
	ledger := &fakeSessionDebits{admitErr: &limits.ExceededError{Dimension: "daily_cost"}}
	mock := newWebRTCMock("usage")
	h := webrtcHarness(t, mock, true)
	h.gateway.Debits = ledger
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("session admitted after durable debit admission refused it")
	}
	if creates, _, _ := mock.counts(); creates != 0 {
		t.Fatal("provider session created after debit refusal")
	}
	if got := ledger.order(); !slices.Equal(got, []string{"prepare", "admit", "released"}) {
		t.Fatalf("ledger order = %v", got)
	}
}

func TestNativeDuplexSessionReservesDurableDebitBeforeDial(t *testing.T) {
	ledger := &fakeSessionDebits{}
	h := xaiDuplexHarness(t, true)
	h.gateway.Debits = ledger
	conn, ctx := dialDuplex(t, h, fullKey, "/native/xai-realtime/sessions/"+routeSlug)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.update","session":{"type":"realtime","voice":"eve"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 {
		t.Fatalf("attempts = %d", len(env.Attempts))
	}
	fact := env.Attempts[0]
	if fact.Execution == nil || fact.Execution.DispatchID != ledger.prepareID || ledger.prepareID == "" {
		t.Fatalf("terminal fact carries no durable dispatch reference: %+v", fact.Execution)
	}
	if got := ledger.order(); !slices.Equal(got, []string{"prepare", "admit", "dispatched"}) {
		t.Fatalf("ledger order = %v", got)
	}
}

func TestNativeDuplexUnresolvedExposureRefusesBeforeDispatch(t *testing.T) {
	ledger := &fakeSessionDebits{unknown: true}
	h := xaiDuplexHarness(t, true)
	h.gateway.Debits = ledger
	conn, ctx := dialDuplex(t, h, fullKey, "/native/xai-realtime/sessions/"+routeSlug)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.update","session":{"type":"realtime"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("duplex session admitted against unresolved durable exposure")
	}
	if got := h.mock.count("a"); got != 0 {
		t.Fatalf("upstream sessions = %d, want refusal before dispatch", got)
	}
}

func TestWebRTCCallerHangupPathIsNotServed(t *testing.T) {
	mock := newWebRTCMock("usage")
	h := webrtcHarnessCfg(t, mock, true, Config{WebRTCBound: 300 * time.Millisecond})
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	location := resp.Header.Get("Location")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	hangup := h.do(t.Context(), http.MethodPost, location+"/hangup", fullKey, nil, nil)
	io.Copy(io.Discard, hangup.Body)
	hangup.Body.Close()
	if hangup.StatusCode != http.StatusNotFound {
		t.Fatalf("caller hangup status=%d, want the path unserved", hangup.StatusCode)
	}
	if _, _, hangups := mock.counts(); hangups != 0 {
		t.Fatal("caller hangup path closed the provider session")
	}
}
