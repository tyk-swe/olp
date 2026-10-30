package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/codemode"
)

type codeGrantAuthorizer struct{ calls atomic.Int64 }

func TestCodeGenericWebSocketRequiresExplicitAffinity(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Olp-Code-Affinity") != "" || r.Header.Get("Thread-Id") != "" || r.Header.Get("Session_id") != "" {
			t.Error("generic socket acquired an upstream identity")
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
		if err = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"generic-socket","usage":{"total_tokens":3,"input_tokens":2,"output_tokens":1}}}`)); err != nil {
			t.Error(err)
		}
		_, _, _ = conn.Read(r.Context())
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}}})
	if conn != nil {
		_ = conn.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusBadRequest || h.mock.count("a") != 0 {
		t.Fatalf("unaffined generic socket reached upstream, response %v error %v", response, err)
	}
	_ = response.Body.Close()
	bound, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "X-Olp-Code-Affinity": {"socket"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer bound.CloseNow()
	if err = bound.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"native-model","input":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	_, body, err := bound.Read(ctx)
	if err != nil || !strings.Contains(string(body), "response.completed") {
		t.Fatalf("affined generic socket failed, body %s error %v", body, err)
	}
	if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 3 {
		t.Fatalf("generic socket usage lost %+v", usage)
	}
}

func (a *codeGrantAuthorizer) AuthorizeCode(ctx context.Context, cfg routing.Configuration, account codemode.Account, dispatch codemode.Dispatch) (codemode.Authorization, error) {
	auth, err := (codeTestAuthorizer{principal: "principal"}).AuthorizeCode(ctx, cfg, account, dispatch)
	// Admission on a socket sees a newer token than its handshake used.
	auth.GrantGeneration = a.calls.Add(1)
	auth.CredentialID = fmt.Sprintf("credential-%d", auth.GrantGeneration)
	return auth, err
}

func TestCodeAuthenticationRefusalRequestsDispatchedGrantRefreshWithoutReplay(t *testing.T) {
	for _, transport := range []string{"http", "websocket-handshake", "websocket-message"} {
		for _, status := range []int{401, 403, 429, 500} {
			t.Run(fmt.Sprintf("%s/%d", transport, status), func(t *testing.T) {
				h, ledger, server := newCodeForwardHarness(t)
				h.gateway.CodeAuthorizer = &codeGrantAuthorizer{}
				now := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
				h.gateway.now = func() time.Time { return now }
				body := fmt.Sprintf(`{"type":"error","status":%d,"headers":{"retry-after":"120"}}`, status)
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					if transport != "websocket-message" {
						w.Header().Set("Retry-After", "120")
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
				ledger.mu.Lock()
				if ledger.health.RetryAt == nil || !ledger.health.RetryAt.Equal(now.Add(2*time.Minute)) {
					t.Errorf("retry timing lost for %s: %+v", transport, ledger.health)
				}
				ledger.mu.Unlock()
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

type codeRecoveryAuthorizer struct{ calls atomic.Int64 }

func (a *codeRecoveryAuthorizer) AuthorizeCode(ctx context.Context, cfg routing.Configuration, account codemode.Account, dispatch codemode.Dispatch) (codemode.Authorization, error) {
	auth, err := (codeTestAuthorizer{principal: "principal"}).AuthorizeCode(ctx, cfg, account, dispatch)
	auth.CredentialID = "credential"
	auth.GrantGeneration = a.calls.Add(1)
	return auth, err
}

func TestCodeInitialRecoveryUsesRefreshedGrantAfterProvenRejection(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	if err := json.Unmarshal([]byte(`{"initial_recovery_attempts":1}`), &route); err != nil {
		t.Fatal(err)
	}
	h.rt.release.Snapshot.CodeRoutes["coding"] = route
	h.gateway.CodeAuthorizer = &codeRecoveryAuthorizer{}
	body := []byte(`{"model":"native-model","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]}],"instructions":"Read local text","stream":false,"metadata":{"label":"initial"},"parallel_tool_calls":true,"reasoning":{"effort":"high","summary":"auto"},"text":{"verbosity":"low"},"tools":[{"type":"function","name":"read_file","description":"Read local text","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false},"strict":true}],"tool_choice":"auto"}`)
	var executions atomic.Int64
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		forwarded, err := io.ReadAll(r.Body)
		if err != nil || string(forwarded) != string(body) {
			t.Error("portable request changed during recovery", err)
		}
		if h.mock.count("a") == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"type":"authentication_error","code":"invalid_api_key"}}`)
			return
		}
		executions.Add(1)
		io.WriteString(w, `{"id":"recovered","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	})
	response := codeDo(t, server, body, nil)
	got, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(got), `"id":"recovered"`) || h.mock.count("a") != 2 || executions.Load() != 1 {
		t.Fatalf("recovery status=%d body=%s requests=%d executions=%d", response.StatusCode, got, h.mock.count("a"), executions.Load())
	}
	ledger.wait(t)
	ledger.wait(t)
}

func TestCodeInitialRecoveryNeverReplaysUnprovenOrOpaqueWork(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		reply       string
		body        string
		headers     http.Header
		enabled     bool
		established bool
		truncated   bool
	}{
		{name: "disabled", status: 429, reply: `{"error":{"type":"usage_limit_reached"}}`},
		{name: "server-error", status: 500, reply: `{"error":{"type":"usage_limit_reached"}}`, enabled: true},
		{name: "incomplete-body", status: 429, reply: `{"error":{"type":"usage_limit_reached"}}`, truncated: true, enabled: true},
		{name: "duplicate-field", status: 429, reply: `{"error":{"type":"usage_limit_reached","type":"usage_limit_reached"}}`, enabled: true},
		{name: "partial-output", status: 200, reply: "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"partial\"}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", enabled: true},
		{name: "unknown-quota", status: 429, reply: `{"error":{"code":"unknown"}}`, enabled: true},
		{name: "transient-limit", status: 429, reply: `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`, enabled: true},
		{name: "accepted-error", status: 200, reply: "data: {\"type\":\"error\",\"status\":429,\"error\":{\"type\":\"usage_limit_reached\"}}\n\n", enabled: true},
		{name: "opaque-header", status: 429, reply: `{"error":{"type":"usage_limit_reached"}}`, headers: http.Header{"X-Codex-Turn-State": {"opaque"}}, enabled: true},
		{name: "repeated-opaque-header", status: 429, reply: `{"error":{"type":"usage_limit_reached"}}`, headers: http.Header{"X-Codex-Turn-State": {"", "opaque"}}, enabled: true},
		{name: "opaque-input", status: 429, reply: `{"error":{"type":"usage_limit_reached"}}`, body: `{"model":"native-model","input":[{"type":"reasoning","encrypted_content":"opaque"}]}`, enabled: true},
		{name: "account-owned-tool", status: 429, reply: `{"error":{"type":"usage_limit_reached"}}`, body: `{"model":"native-model","input":"hello","tools":[{"type":"file_search","vector_store_ids":["vs_private"]}]}`, enabled: true},
		{name: "account-owned-prompt", status: 429, reply: `{"error":{"type":"usage_limit_reached"}}`, body: `{"model":"native-model","input":"hello","prompt":{"id":"pmpt_private"}}`, enabled: true},
		{name: "unknown-top-level-state", status: 429, reply: `{"error":{"type":"usage_limit_reached"}}`, body: `{"model":"native-model","input":"hello","future_state":{"id":"opaque"}}`, enabled: true},
		{name: "unknown-nested-state", status: 429, reply: `{"error":{"type":"usage_limit_reached"}}`, body: `{"model":"native-model","input":"hello","reasoning":{"effort":"high","future_state":"opaque"}}`, enabled: true},
		{name: "established", status: 429, reply: `{"error":{"type":"usage_limit_reached"}}`, established: true, enabled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			if test.enabled {
				route.InitialRecoveryAttempts = 2
			}
			h.rt.release.Snapshot.CodeRoutes["coding"] = route
			body := []byte(`{"model":"native-model","input":[]}`)
			if test.body != "" {
				body = []byte(test.body)
			}
			if test.established {
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"id":"established"}`) })
				response := codeDo(t, server, body, nil)
				io.Copy(io.Discard, response.Body)
				ledger.wait(t)
			}
			before := h.mock.count("a")
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				forwarded, err := io.ReadAll(r.Body)
				if err != nil || string(forwarded) != string(body) {
					t.Error("native initial request changed before forwarding", err)
				}
				if test.truncated {
					w.Header().Set("Content-Length", fmt.Sprint(len(test.reply)+1))
				}
				if test.status == 200 {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				w.WriteHeader(test.status)
				io.WriteString(w, test.reply)
			})
			response := codeDo(t, server, body, test.headers)
			got, _ := io.ReadAll(response.Body)
			if response.StatusCode != test.status || string(got) != test.reply || h.mock.count("a") != before+1 {
				t.Fatalf("unsafe recovery status=%d body=%s requests=%d", response.StatusCode, got, h.mock.count("a")-before)
			}
			ledger.wait(t)
		})
	}
}

func TestCodeInitialRecoveryStopsAtPublishedBound(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.InitialRecoveryAttempts = 2
	h.rt.release.Snapshot.CodeRoutes["coding"] = route
	const rejection = `{"error":{"type":"usage_limit_reached"}}`
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, rejection)
	})
	response := codeDo(t, server, []byte(`{"model":"native-model","input":[]}`), nil)
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != 429 || string(body) != rejection || h.mock.count("a") != 3 {
		t.Fatalf("unbounded recovery status=%d body=%s requests=%d", response.StatusCode, body, h.mock.count("a"))
	}
	for range 3 {
		usage := ledger.wait(t)
		if usage.Total == nil || *usage.Total != 0 {
			t.Fatal("proven nonexecution retained uncertain usage")
		}
	}
}

func TestCodeInitialRecoveryDoesNotReplayAmbiguousTransport(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.InitialRecoveryAttempts = 2
	h.rt.release.Snapshot.CodeRoutes["coding"] = route
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close()
	})
	response := codeDo(t, server, []byte(`{"model":"native-model","input":[]}`), nil)
	io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusBadGateway || h.mock.count("a") != 1 || ledger.wait(t).Total != nil {
		t.Fatal("ambiguous transport replayed or lost usage uncertainty")
	}
}

func TestCodeInitialRecoveryRecordsAttributedAdmissionRefusals(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%t", replacement), func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			identifyHarness(h, "header")
			route := h.rt.release.Snapshot.CodeRoutes["coding"]
			if replacement {
				route.InitialRecoveryAttempts = 1
				h.rt.release.Snapshot.CodeRoutes["coding"] = route
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					ledger.mu.Lock()
					ledger.refuse = "code_pool_denied"
					ledger.mu.Unlock()
					w.WriteHeader(429)
					io.WriteString(w, `{"error":{"type":"usage_limit_reached"}}`)
				})
			} else {
				ledger.refuse = "code_pool_denied"
			}
			identifier := "private-recovery-user"
			response := codeDo(t, server, []byte(`{"model":"native-model","input":"hello"}`), http.Header{"X-OLP-End-User": {identifier}})
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != 409 || !strings.Contains(string(body), "code_pool_denied") {
				t.Fatalf("refusal response=%d body=%s", response.StatusCode, body)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			expected := codeTestRefusal{key: h.rt.keys[fullKey].ID, digest: (endUserRuntime{h.rt}).EndUserDigest(&route.ProjectID, identifier), code: "code_pool_denied"}
			if len(ledger.refusalRecords) != 1 || ledger.refusalRecords[0] != expected {
				t.Fatalf("attributed refusal missing %+v", ledger.refusalRecords)
			}
			want := 0
			if replacement {
				want = 1
			}
			if h.mock.count("a") != want {
				t.Fatal("refused admission dispatched an extra request")
			}
		})
	}
}
