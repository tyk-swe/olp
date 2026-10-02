package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestCodeProviderLimitsFailClosedBeforeDispatch(t *testing.T) {
	for _, dimension := range []string{"requests", "tokens", "concurrency"} {
		t.Run(dimension, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			one := int64(1)
			quota := &runtime.Limits{}
			switch dimension {
			case "requests":
				quota.RequestsPerMinute = &one
			case "tokens":
				quota.TokensPerMinute = &one
			case "concurrency":
				quota.MaxConcurrency = &one
			}
			configuration := h.rt.release.Snapshot.CodeConnections["revision:provider"]
			configuration.Options.Limits = quota
			h.rt.release.Snapshot.CodeConnections["revision:provider"] = configuration
			response := codeDo(t, server, []byte(`{"model":"native-model","input":"private"}`), nil)
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != 503 || !bytes.Contains(body, []byte("code_provider_rate_limited")) || h.mock.count("a") != 0 {
				t.Fatalf("provider quota bypassed: status=%d body=%s", response.StatusCode, body)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.marks) != 0 || len(ledger.aborts) != 1 {
				t.Fatalf("undispatched attempt was not refunded: marks=%v aborts=%v", ledger.marks, ledger.aborts)
			}
		})
	}
}

func TestCodeWebSocketRelaysPeerCloseCodeAndReason(t *testing.T) {
	for _, origin := range []string{"client", "upstream"} {
		for _, code := range []websocket.StatusCode{websocket.StatusNormalClosure, websocket.StatusPolicyViolation, websocket.StatusTryAgainLater} {
			t.Run(fmt.Sprintf("%s/%d", origin, code), func(t *testing.T) {
				h, _, server := newCodeForwardHarness(t)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				observed := make(chan error, 1)
				const reason = "qualified peer reason"
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					upstream, err := websocket.Accept(w, r, nil)
					if err != nil {
						observed <- err
						return
					}
					defer upstream.CloseNow()
					if origin == "upstream" {
						_ = upstream.Close(code, reason)
						return
					}
					_, _, err = upstream.Read(ctx)
					observed <- err
				})
				client, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"close"}}})
				if err != nil {
					t.Fatal(err)
				}
				defer client.CloseNow()
				if origin == "client" {
					_ = client.Close(code, reason)
					select {
					case err = <-observed:
					case <-ctx.Done():
						t.Fatal("client close did not reach upstream")
					}
				} else {
					_, _, err = client.Read(ctx)
				}
				close, ok := errors.AsType[websocket.CloseError](err)
				if !ok || close.Code != code || close.Reason != reason {
					t.Fatalf("close changed: %v", err)
				}
			})
		}
	}
}

func TestCodeWebSocketObservesAllowanceOutsideGenerations(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	before := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":20}}}`)
	after := []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":100}}}`)
	terminal := []byte(`{"type":"response.completed","response":{"id":"observed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		upstream, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer upstream.CloseNow()
		_ = upstream.Write(ctx, websocket.MessageText, before)
		if _, _, err := upstream.Read(ctx); err != nil {
			return
		}
		_ = upstream.Write(ctx, websocket.MessageText, terminal)
		_ = upstream.Write(ctx, websocket.MessageText, after)
		_, _, _ = upstream.Read(ctx)
	})
	client, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"allowance"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()
	if _, body, err := client.Read(ctx); err != nil || !bytes.Equal(body, before) {
		t.Fatalf("initial allowance changed: %s %v", body, err)
	}
	if err := client.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"native-model","input":[]}`)); err != nil {
		t.Fatal(err)
	}
	for _, expected := range [][]byte{terminal, after} {
		if _, body, err := client.Read(ctx); err != nil || !bytes.Equal(body, expected) {
			t.Fatalf("upstream message changed: %s %v", body, err)
		}
	}
	ledger.wait(t)
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.allowances) != 2 || *ledger.allowances[0].RemainingPercent != 80 || *ledger.allowances[1].RemainingPercent != 0 {
		t.Fatalf("account-level allowance lost: %+v", ledger.allowances)
	}
}
