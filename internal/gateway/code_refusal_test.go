package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/runtime"
)

type codeGrantAuthorizer struct{ calls atomic.Int64 }

func (a *codeGrantAuthorizer) AuthorizeCode(ctx context.Context, cfg runtime.Configuration, account codemode.Account) (CodeAuthorization, error) {
	auth, err := (codeTestAuthorizer{principal: "principal"}).AuthorizeCode(ctx, cfg, account)
	// Admission on a socket sees a newer token than its handshake used.
	auth.GrantGeneration = a.calls.Add(1)
	auth.CredentialID = fmt.Sprintf("credential-%d", auth.GrantGeneration)
	return auth, err
}

func TestCodeWebSocketRechecksHandshakeCredentialAfterRotation(t *testing.T) {
	for _, eligibility := range []runtime.Eligibility{runtime.Revoked, runtime.Lapsed} {
		for _, completed := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/after-%d-generations", eligibility, completed), func(t *testing.T) {
				h, ledger, server := newCodeForwardHarness(t)
				authorizer := &codeGrantAuthorizer{}
				h.gateway.CodeAuthorizer = authorizer
				var generations atomic.Int64
				closed := make(chan struct{})
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					defer close(closed)
					for {
						if _, _, err := conn.Read(r.Context()); err != nil {
							return
						}
						generations.Add(1)
						if err := conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"usage":{"total_tokens":3}}}`)); err != nil {
							return
						}
					}
				})
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				conn, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"rotated"}}})
				if err != nil {
					t.Fatal(err)
				}
				defer conn.CloseNow()
				frame := []byte(`{"type":"response.create","model":"native-model"}`)
				for range completed {
					if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
						t.Fatal(err)
					}
					if _, _, err := conn.Read(ctx); err != nil {
						t.Fatal(err)
					}
					ledger.wait(t)
				}
				h.rt.mu.Lock()
				if eligibility == runtime.Revoked {
					h.rt.revoked["credential-1"] = true
				} else {
					h.rt.lapsed = map[string]bool{"credential-1": true}
				}
				h.rt.mu.Unlock()
				if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
					t.Fatal(err)
				}
				_, _, err = conn.Read(ctx)
				close, ok := errors.AsType[websocket.CloseError](err)
				if !ok || close.Code != websocket.StatusPolicyViolation || close.Reason != "code_account_unavailable" {
					t.Fatalf("ineligible handshake credential dispatched: %v", err)
				}
				select {
				case <-closed:
				case <-ctx.Done():
					t.Fatal("upstream socket not closed")
				}
				ledger.mu.Lock()
				defer ledger.mu.Unlock()
				if authorizer.calls.Load() != int64(completed+2) || generations.Load() != int64(completed) || len(ledger.marks) != completed || len(ledger.aborts) != 1 || !reflect.DeepEqual(ledger.refusals, []string{"code_account_unavailable"}) {
					t.Fatalf("refused generation dispatched or leaked reservation: calls=%d generations=%d marks=%v aborts=%v refusals=%v", authorizer.calls.Load(), generations.Load(), ledger.marks, ledger.aborts, ledger.refusals)
				}
			})
		}
	}
}

func TestCodeAuthenticationRefusalRequestsDispatchedGrantRefreshWithoutReplay(t *testing.T) {
	for _, transport := range []string{"http", "websocket-handshake", "websocket-message"} {
		for _, status := range []int{401, 403, 500} {
			t.Run(fmt.Sprintf("%s/%d", transport, status), func(t *testing.T) {
				h, ledger, server := newCodeForwardHarness(t)
				h.gateway.CodeAuthorizer = &codeGrantAuthorizer{}
				body := fmt.Sprintf(`{"type":"error","status":%d}`, status)
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					if transport != "websocket-message" {
						w.WriteHeader(status)
						io.WriteString(w, "opaque refusal")
						return
					}
					conn, err := websocket.Accept(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.CloseNow()
					if _, _, err = conn.Read(r.Context()); err != nil {
						t.Error(err)
						return
					}
					if err = conn.Write(r.Context(), websocket.MessageText, []byte(body)); err != nil {
						t.Error(err)
					}
					_, _, _ = conn.Read(r.Context())
				})
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				switch transport {
				case "http":
					response := codeDo(t, server, []byte(`{"model":"native-model","input":[]}`), nil)
					got, _ := io.ReadAll(response.Body)
					if response.StatusCode != status || string(got) != "opaque refusal" {
						t.Fatal("HTTP refusal changed")
					}
					ledger.wait(t)
				case "websocket-handshake":
					conn, response, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"handshake-refusal"}}})
					if conn != nil {
						conn.CloseNow()
					}
					if err == nil || response == nil || response.StatusCode != status {
						t.Fatalf("WebSocket handshake refusal changed: response=%v error=%v", response, err)
					}
					response.Body.Close()
				case "websocket-message":
					conn, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"refusal"}}})
					if err != nil {
						t.Fatal(err)
					}
					defer conn.CloseNow()
					if err = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"native-model"}`)); err != nil {
						t.Fatal(err)
					}
					_, got, err := conn.Read(ctx)
					if err != nil || string(got) != body {
						t.Fatalf("WebSocket refusal changed: %v", err)
					}
					ledger.wait(t)
				}
				h.rt.mu.Lock()
				defer h.rt.mu.Unlock()
				if status == 401 {
					if !reflect.DeepEqual(h.rt.refused, []string{"credential-1"}) || !reflect.DeepEqual(h.rt.refusedGenerations, []int64{1}) {
						t.Fatalf("refresh attributed to the wrong authorization: %v %v", h.rt.refused, h.rt.refusedGenerations)
					}
				} else if len(h.rt.refused) != 0 {
					t.Fatal("non-authentication refusal requested refresh")
				}
				if h.mock.count("a") != 1 {
					t.Fatal("refused request replayed")
				}
			})
		}
	}
}
