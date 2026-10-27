package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/connectors"
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
