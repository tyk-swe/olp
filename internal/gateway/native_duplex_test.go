package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/duplexcontract"
	"github.com/tyk-swe/olp/internal/routing"
)

func xaiDuplexHarness(t *testing.T, withUsage bool) *harness {
	t.Helper()
	h := strictHarness(t, func(snapshot *routing.Snapshot) {
		for id, provider := range snapshot.Providers {
			if provider.Name != "a" {
				continue
			}
			provider.ProfileID, provider.ProfileRevision = "xai-realtime", connectors.ProfileRevision
			provider.VendorID = "xai"
			provider.Capabilities = []routing.Capability{{Model: modelA, Operation: "realtime", Surface: "native", Mode: "realtime"}}
			snapshot.Providers[id] = provider
		}
		route := snapshot.Routes[routeSlug]
		route.Operations = []string{"realtime"}
		route.Targets = route.Targets[:1]
		snapshot.Routes[routeSlug] = route
	})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		ctx := r.Context()
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created","session":{"id":"sess-upstream"}}`)); err != nil {
			return
		}
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		if withUsage {
			if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.done","response":{"id":"r1","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`)); err != nil {
				return
			}
		}
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	})
	return h
}

func dialDuplex(t *testing.T, h *harness, key, path string) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + path
	conn, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })
	return conn, ctx
}

func TestNativeDuplexXAIRelaysOpaqueFramesAndSettlesUsageOnce(t *testing.T) {
	h := xaiDuplexHarness(t, true)
	conn, ctx := dialDuplex(t, h, fullKey, "/native/xai-realtime/sessions/"+routeSlug)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.update","session":{"type":"realtime","voice":"eve"}}`)); err != nil {
		t.Fatal(err)
	}
	_, created, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(created), "session.created") {
		t.Fatalf("first relayed frame = %q err=%v", created, err)
	}
	_, done, err := conn.Read(ctx)
	if err != nil || !strings.Contains(string(done), "response.done") {
		t.Fatalf("second relayed frame = %q err=%v", done, err)
	}
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	if got := h.mock.count("a"); got != 1 {
		t.Fatalf("upstream sessions = %d, want exactly one pinned dial", got)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 {
		t.Fatalf("attempts = %d, want one settled attempt", len(env.Attempts))
	}
	attempt := env.Attempts[0]
	if !attempt.UsageObserved || attempt.Usage == nil || attempt.Usage.InputTokens != 11 || attempt.Usage.OutputTokens != 7 {
		t.Fatalf("settled usage = %+v observed=%v", attempt.Usage, attempt.UsageObserved)
	}
	if attempt.Status != http.StatusOK {
		t.Fatalf("settled status = %d, want a successful terminal status so request pricing applies", attempt.Status)
	}
}

func TestNativeDuplexXAITextFirstSessionStaysUncertain(t *testing.T) {
	h := xaiDuplexHarness(t, true)
	conn, ctx := dialDuplex(t, h, fullKey, "/native/xai-realtime/sessions/"+routeSlug)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}`)); err != nil {
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
		t.Fatalf("attempts = %d, want one settled attempt", len(env.Attempts))
	}
	if !env.Attempts[0].BillingUncertain {
		t.Fatalf("text-first session settled as billing-complete: %+v", env.Attempts[0])
	}
}

func TestNativeDuplexXAIRefusesTranscriptionBeforeDispatch(t *testing.T) {
	h := xaiDuplexHarness(t, true)
	conn, ctx := dialDuplex(t, h, fullKey, "/native/xai-realtime/sessions/"+routeSlug)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.update","session":{"type":"transcription"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("transcription-only session was admitted on a profile that does not declare it")
	} else if code := websocket.CloseStatus(err); code != websocket.StatusPolicyViolation {
		t.Fatalf("close code = %v, want policy violation (%v)", code, err)
	}
	if got := h.mock.count("a"); got != 0 {
		t.Fatalf("upstream sessions = %d, want refusal before any dispatch", got)
	}
}

func TestNativeDuplexMissingUsageStaysUncertain(t *testing.T) {
	h := xaiDuplexHarness(t, false)
	conn, ctx := dialDuplex(t, h, fullKey, "/native/xai-realtime/sessions/"+routeSlug)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.update","session":{"type":"realtime"}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 || !env.Attempts[0].BillingUncertain || env.Attempts[0].UsageObserved {
		t.Fatalf("missing terminal usage must settle uncertain, got %+v", env.Attempts)
	}
}

type recordingPeer struct {
	frames chan []byte
	writes chan []byte
}

func (p *recordingPeer) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	select {
	case frame, ok := <-p.frames:
		if !ok {
			return 0, nil, websocket.CloseError{Code: websocket.StatusNormalClosure}
		}
		return websocket.MessageText, frame, nil
	case <-ctx.Done():
		return 0, nil, ctx.Err()
	}
}

func (p *recordingPeer) Write(_ context.Context, _ websocket.MessageType, data []byte) error {
	p.writes <- data
	return nil
}

func (*recordingPeer) Ping(context.Context) error               { return nil }
func (*recordingPeer) Close(websocket.StatusCode, string) error { return nil }
func (*recordingPeer) CloseNow() error                          { return nil }

func TestNativeDuplexGainsNoResponseCaching(t *testing.T) {
	h, _ := enableTestCache(t, xaiDuplexHarness(t, true))
	for i := 0; i < 2; i++ {
		conn, ctx := dialDuplex(t, h, fullKey, "/native/xai-realtime/sessions/"+routeSlug)
		if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.update","session":{"type":"realtime"}}`)); err != nil {
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
	}
	if got := h.mock.count("a"); got != 2 {
		t.Fatalf("upstream sessions = %d, want both sessions dispatched with no replay", got)
	}
}

func TestDuplexSetupAckRequiresSetupComplete(t *testing.T) {
	template, err := duplexcontract.Compile(duplexcontract.Config{
		Provider: connectors.Config{
			Kind: "vertex_ai", VendorID: "google-vertex", AuthMode: "service_account",
			Endpoint:     connectors.DefaultEndpoint("vertex_ai", "us-central1", "example-project"),
			CloudRegion:  "us-central1",
			CloudProject: "example-project",
			ProfileID:    "vertex-realtime", ProfileRevision: connectors.ProfileRevision,
		},
		Model: "gemini-live-example",
	})
	if err != nil {
		t.Fatal(err)
	}
	upstream := &recordingPeer{frames: make(chan []byte, 1), writes: make(chan []byte, 4)}
	client := &recordingPeer{frames: make(chan []byte), writes: make(chan []byte, 4)}
	upstream.frames <- []byte(`{"serverContent":{"turnComplete":true}}`)
	if err := awaitDuplexSetupAck(t.Context(), upstream, client, template.Observer(1<<20), time.Second); err == nil {
		t.Fatal("content before setup completion was admitted as an acknowledgement")
	}
	upstream.frames <- []byte(`{"setupComplete":{}}`)
	if err := awaitDuplexSetupAck(t.Context(), upstream, client, template.Observer(1<<20), time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case ack := <-client.writes:
		if string(ack) != `{"setupComplete":{}}` {
			t.Fatalf("forwarded ack = %s", ack)
		}
	default:
		t.Fatal("setup acknowledgement was not forwarded to the client")
	}
}

func bedrockDuplexHarness(t *testing.T) *harness {
	t.Helper()
	return strictHarness(t, func(snapshot *routing.Snapshot) {
		for id, provider := range snapshot.Providers {
			if provider.Name != "a" {
				continue
			}
			endpoint, err := connectors.BedrockEndpoint("us-east-1", false)
			if err != nil {
				t.Fatal(err)
			}
			provider.Kind = "bedrock"
			provider.ProfileID, provider.ProfileRevision = "bedrock-realtime", connectors.ProfileRevision
			provider.VendorID = "amazon-bedrock"
			provider.AuthMode = "static"
			provider.CloudRegion = "us-east-1"
			provider.Endpoint = endpoint
			provider.Capabilities = []routing.Capability{{Model: modelA, Operation: "realtime", Surface: "native", Mode: "realtime"}}
			snapshot.Providers[id] = provider
		}
		route := snapshot.Routes[routeSlug]
		route.Operations = []string{"realtime"}
		route.Targets = route.Targets[:1]
		snapshot.Routes[routeSlug] = route
	})
}

func TestNativeDuplexBedrockBridgesWebSocketToBidirectionalStream(t *testing.T) {
	h := bedrockDuplexHarness(t)
	upstream := &recordingPeer{frames: make(chan []byte, 4), writes: make(chan []byte, 4)}
	upstream.frames <- []byte(`{"event":{"contentStart":{"contentId":"c1","type":"TEXT","role":"ASSISTANT"}}}`)
	upstream.frames <- []byte(`{"event":{"textOutput":{"content":"hello from sonic"}}}`)
	opens := 0
	var streamCtx context.Context
	h.gateway.bedrockStream = func(ctx context.Context, _ *call, _ *pin) (duplexPeer, error) {
		opens++
		streamCtx = ctx
		return upstream, nil
	}
	conn, ctx := dialDuplex(t, h, fullKey, "/native/bedrock-realtime/sessions/"+routeSlug)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"event":{"sessionStart":{"inferenceConfig":{"maxTokens":1024}}}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case setup := <-upstream.writes:
		if !strings.Contains(string(setup), "sessionStart") {
			t.Fatalf("forwarded setup = %s", setup)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("setup frame never reached the bidirectional stream")
	}
	if _, frame, err := conn.Read(ctx); err != nil || !strings.Contains(string(frame), "contentStart") {
		t.Fatalf("first relayed frame = %q err=%v", frame, err)
	}
	if _, frame, err := conn.Read(ctx); err != nil || !strings.Contains(string(frame), "textOutput") {
		t.Fatalf("second relayed frame = %q err=%v", frame, err)
	}
	if streamCtx == nil || streamCtx.Err() != nil {
		t.Fatalf("stream context cancelled while the session is live: %v", streamCtx.Err())
	}
	if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	if opens != 1 {
		t.Fatalf("bidirectional streams opened = %d, want exactly one pinned stream", opens)
	}
	env := h.sink.last(t)
	if streamCtx.Err() == nil {
		t.Fatal("stream context still live after the session closed")
	}
	if len(env.Attempts) != 1 || !env.Attempts[0].BillingUncertain || env.Attempts[0].UsageObserved {
		t.Fatalf("Nova Sonic terminal usage must settle uncertain, never zero, got %+v", env.Attempts)
	}
}

func TestNativeDuplexBedrockCleanStreamEndSettlesObservedUsage(t *testing.T) {
	h := bedrockDuplexHarness(t)
	upstream := &recordingPeer{frames: make(chan []byte, 8), writes: make(chan []byte, 4)}
	for _, frame := range []string{
		`{"event":{"completionStart":{"sessionId":"s1","promptName":"p1","completionId":"c1"}}}`,
		`{"event":{"contentStart":{"contentId":"x1","type":"TEXT","role":"ASSISTANT"}}}`,
		`{"event":{"textOutput":{"content":"hello"}}}`,
		`{"event":{"contentEnd":{"contentId":"x1"}}}`,
		`{"event":{"usageEvent":{"sessionId":"s1","promptName":"p1","completionId":"c1","totalInputTokens":12,"totalOutputTokens":5,"totalTokens":17}}}`,
		`{"event":{"completionEnd":{"sessionId":"s1","promptName":"p1","completionId":"c1","stopReason":"END_TURN"}}}`,
	} {
		upstream.frames <- []byte(frame)
	}
	h.gateway.bedrockStream = func(context.Context, *call, *pin) (duplexPeer, error) {
		return upstream, nil
	}
	conn, ctx := dialDuplex(t, h, fullKey, "/native/bedrock-realtime/sessions/"+routeSlug)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"event":{"sessionStart":{"inferenceConfig":{"maxTokens":1024}}}}`)); err != nil {
		t.Fatal(err)
	}
	for _, frame := range []string{
		`{"event":{"promptEnd":{"promptName":"p1"}}}`,
		`{"event":{"sessionEnd":{}}}`,
	} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	close(upstream.frames)
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			break
		}
	}
	env := h.sink.last(t)
	if len(env.Attempts) != 1 {
		t.Fatalf("attempts = %d", len(env.Attempts))
	}
	fact := env.Attempts[0]
	if !fact.UsageObserved || !fact.UsageComplete || fact.BillingUncertain || fact.Usage == nil || fact.Usage.InputTokens != 12 || fact.Usage.OutputTokens != 5 {
		t.Fatalf("clean Bedrock stream end did not settle observed usage: %+v", fact)
	}
}

func TestNativeDuplexRefusesForeignKeyAndUnknownDialect(t *testing.T) {
	h := xaiDuplexHarness(t, true)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	base := "ws" + strings.TrimPrefix(h.server.URL, "http")
	if conn, _, err := websocket.Dial(ctx, base+"/native/xai-realtime/sessions/"+routeSlug, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + otherKey}}}); err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("key without the route opened a duplex session")
	}
	if conn, _, err := websocket.Dial(ctx, base+"/native/no-such-dialect/sessions/"+routeSlug, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}}}); err == nil {
		conn.Close(websocket.StatusNormalClosure, "")
		t.Fatal("unknown duplex dialect opened a session")
	}
	if got := h.mock.count("a"); got != 0 {
		t.Fatalf("upstream sessions = %d, want no dispatch", got)
	}
}

func TestNativeDuplexRollbackStopsNewSessionsAndKeepsSettlement(t *testing.T) {
	h := xaiDuplexHarness(t, true)
	conn, ctx := dialDuplex(t, h, fullKey, "/native/xai-realtime/sessions/"+routeSlug)
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"session.update","session":{"type":"realtime"}}`)); err != nil {
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
	settled := h.sink.last(t)
	if len(settled.Attempts) != 1 || !settled.Attempts[0].UsageObserved {
		t.Fatalf("settlement before rollback = %+v", settled.Attempts)
	}
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		delete(s.Routes, routeSlug)
	})
	url := "ws" + strings.TrimPrefix(h.server.URL, "http") + "/native/xai-realtime/sessions/" + routeSlug
	dialCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err := websocket.Dial(dialCtx, url, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}}}); err == nil {
		t.Fatal("new duplex session admitted after the route was retired")
	}
	if got := h.mock.count("a"); got != 1 {
		t.Fatalf("upstream sessions = %d, want the retired family to dispatch nothing new", got)
	}
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	settlements := 0
	for _, env := range h.sink.envs {
		if len(env.Attempts) > 0 && env.Attempts[0].UsageObserved {
			settlements++
		}
	}
	if settlements != 1 {
		t.Fatalf("settlement history after rollback = %d records, want the original one kept", settlements)
	}
}

type blockedNativeClosePeer struct {
	*closingRelayPeer
	started chan struct{}
	release <-chan struct{}
}

func (p *blockedNativeClosePeer) Close(websocket.StatusCode, string) error {
	close(p.started)
	<-p.release
	return nil
}

type observedNativeClosePeer struct {
	*closingRelayPeer
	closed chan struct{}
	once   sync.Once
}

func (p *observedNativeClosePeer) CloseNow() error { p.once.Do(func() { close(p.closed) }); return nil }

func TestNativeDuplexRevocationStopsProviderBeforeCallerHandshake(t *testing.T) {
	h := xaiDuplexHarness(t, false)
	route := h.rt.release.Snapshot.Routes[routeSlug]
	provider := h.rt.release.Snapshot.Providers[route.Targets[0].ProviderID]
	template, err := duplexcontract.Compile(duplexcontract.Config{Provider: provider.Connector(), Model: modelA})
	if err != nil {
		t.Fatal(err)
	}
	h.rt.mu.Lock()
	delete(h.rt.keys, fullKey)
	h.rt.mu.Unlock()
	releaseClose := make(chan struct{})
	stop := sync.OnceFunc(func() { close(releaseClose) })
	defer stop()
	client := &blockedNativeClosePeer{closingRelayPeer: &closingRelayPeer{frames: make(chan []byte)}, started: make(chan struct{}), release: releaseClose}
	upstream := &observedNativeClosePeer{closingRelayPeer: &closingRelayPeer{frames: make(chan []byte)}, closed: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, _, err := h.gateway.relayNativeDuplex(t.Context(), &call{request: request{release: h.rt.release}, routeLeg: routeLeg{route: &route}, keyID: h.keyID}, &pin{provider: provider, slot: provider.Slots[0]}, client, upstream, fullKey, template.Observer(1024), 1024)
		done <- err
	}()
	select {
	case <-client.started:
	case <-time.After(realtimeReauth + time.Second):
		t.Fatal("revoked session never began closing")
	}
	select {
	case <-upstream.closed:
	default:
		t.Fatal("provider kept running while the caller held its close handshake open")
	}
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, errDuplexAuthority) {
			t.Fatalf("revocation outcome=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("relay did not finish after the caller close completed")
	}
}
