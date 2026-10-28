package mcpservers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tyk-swe/olp/internal/egress"
)

func TestLegacyTransportRequiresExplicitSelectionAndPinsProtocol(t *testing.T) {
	var sessions sync.Map
	var sequence atomic.Int64
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer legacy-fixture" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			stored, ok := sessions.Load(r.URL.Query().Get("session"))
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			stored.(*sdkmcp.SSEServerTransport).ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		id := fmt.Sprint(sequence.Add(1))
		transport := &sdkmcp.SSEServerTransport{Endpoint: "/messages?session=" + id, Response: w, MaxRequestBodyBytes: 1 << 20}
		sessions.Store(id, transport)
		defer sessions.Delete(id)
		w.Header().Set("Content-Type", "text/event-stream")
		connection, err := transport.Connect(r.Context())
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			message, err := connection.Read(r.Context())
			if err != nil {
				return
			}
			request, ok := message.(*jsonrpc.Request)
			if !ok || !request.ID.IsValid() {
				continue
			}
			result := json.RawMessage(`{"tools":[{"name":"lookup","inputSchema":{"type":"object"}}]}`)
			switch request.Method {
			case "initialize":
				var params struct {
					Protocol string `json:"protocolVersion"`
				}
				if json.Unmarshal(request.Params, &params) != nil || params.Protocol != "2024-11-05" {
					t.Error("legacy initialization did not pin the declared protocol")
					return
				}
				result = json.RawMessage(`{"protocolVersion":"2024-11-05","capabilities":{"tools":{}}}`)
			case "tools/call":
				calls.Add(1)
				result = json.RawMessage(`{"content":[{"type":"text","text":"legacy answer"}]}`)
			}
			if connection.Write(r.Context(), &jsonrpc.Response{ID: request.ID, Result: result}) != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	if _, err := DialTransport(t.Context(), fixturePolicy(), upstream.URL, []byte("legacy-fixture"), TransportOptions{ClientName: "fixture", Timeout: time.Second}); err == nil {
		t.Fatal("modern transport silently fell back to legacy SSE")
	}
	transport, err := DialTransport(t.Context(), fixturePolicy(), upstream.URL, []byte("legacy-fixture"), TransportOptions{ClientName: "fixture", Timeout: time.Second, Transport: "sse"})
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close(t.Context())
	catalog, err := transport.CertifyCatalog(t.Context())
	if err != nil || catalog.Protocol != "2024-11-05" || len(catalog.Tools) != 1 {
		t.Fatalf("legacy catalog failed: %#v, %v", catalog, err)
	}
	result, err := transport.CallTool(t.Context(), "lookup", json.RawMessage(`{}`))
	if err != nil || len(result.Content) != 1 || result.Content[0].Text != "legacy answer" || calls.Load() != 1 {
		t.Fatalf("legacy call failed: %#v, %v, calls=%d", result, err, calls.Load())
	}
}

func TestLegacyTransportRefusesUndeclaredPostDestination(t *testing.T) {
	var foreignCalls atomic.Int64
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer foreign.Close()
	for _, tc := range []struct {
		name     string
		endpoint func(*http.Request) string
	}{
		{"other origin", func(*http.Request) string { return foreign.URL }},
		{"redirect", func(r *http.Request) string { return "http://" + r.Host + "/redirect" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					w.Header().Set("Location", foreign.URL)
					w.WriteHeader(http.StatusTemporaryRedirect)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", tc.endpoint(r))
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer upstream.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			transport, err := DialTransport(ctx, fixturePolicy(), upstream.URL, []byte("legacy-fixture"), TransportOptions{ClientName: "fixture", Timeout: time.Second, Transport: "sse"})
			if err == nil {
				transport.Close(ctx)
				t.Fatal("legacy transport accepted an undeclared destination")
			}
			if foreignCalls.Load() != 0 {
				t.Fatal("legacy transport contacted a destination outside its registered origin")
			}
		})
	}
}

func fixturePolicy() *egress.Policy {
	return &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
}
func TestCertificationNegotiatesSessionsAndBoundsSchemasWithoutExecutingTools(t *testing.T) {
	for _, sse := range []bool{false, true} {
		t.Run(fmt.Sprint(sse), func(t *testing.T) {
			var closed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-only" {
					t.Error("missing bearer identity")
				}
				if r.Method == "DELETE" {
					closed.Store(true)
					w.WriteHeader(204)
					return
				}
				var request struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					t.Error("bad request")
					return
				}
				var result any
				switch request.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "fixture-session")
					result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
				case "notifications/initialized":
					w.WriteHeader(202)
					return
				case "tools/list":
					if r.Header.Get("Mcp-Session-Id") != "fixture-session" || r.Header.Get("MCP-Protocol-Version") != "2025-06-18" {
						t.Error("missing negotiated context")
					}
					result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "integer", "minimum": json.Number("9007199254740993")}}}}}}
				default:
					t.Errorf("unexpected side-effecting method %s", request.Method)
					w.WriteHeader(400)
					return
				}
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
				if sse {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.Write(body)
				}
			}))
			defer server.Close()
			catalog, err := Certify(t.Context(), fixturePolicy(), server.URL, []byte("fixture-only"))
			if err == nil && !strings.Contains(string(catalog.Tools[0].InputSchema), "9007199254740993") {
				t.Fatal("schema number lost precision")
			}
			if err != nil || len(catalog.Tools) != 1 || catalog.Protocol != "2025-06-18" || len(catalog.Digest) != 64 || !closed.Load() {
				t.Fatalf("certification: %v, %v", catalog, err)
			}
		})
	}
}
func TestTransportCallsUpstreamTools(t *testing.T) {
	for _, tc := range []struct {
		name string
		sse  bool
	}{{"json", false}, {"sse", true}} {
		sse := tc.sse
		t.Run(tc.name, func(t *testing.T) {
			var closed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+"fixture-only" {
					t.Error("missing bearer credential")
				}
				if r.Method == "DELETE" {
					closed.Store(true)
					w.WriteHeader(204)
					return
				}
				var request struct {
					ID     int             `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					t.Error("bad request")
					return
				}
				var result any
				switch request.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "fixture-session")
					result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}}
				case "notifications/initialized":
					w.WriteHeader(202)
					return
				case "tools/call":
					if r.Header.Get("Mcp-Session-Id") != "fixture-session" || r.Header.Get("MCP-Protocol-Version") != "2025-03-26" {
						t.Error("missing negotiated context")
					}
					if !strings.Contains(string(request.Params), `"lookup"`) {
						t.Error("missing tool name")
					}
					result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}, "isError": false}
				default:
					t.Errorf("unexpected method %s", request.Method)
					w.WriteHeader(400)
					return
				}
				body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
				if sse {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				} else {
					w.Header().Set("Content-Type", "application/json")
					w.Write(body)
				}
			}))
			defer server.Close()
			transport, err := DialTransport(t.Context(), fixturePolicy(), server.URL, []byte("fixture-only"), TransportOptions{ClientName: "test", Timeout: 10 * time.Second})
			if err != nil || transport.Protocol() != "2025-03-26" {
				t.Fatalf("dial: %v, %v", transport, err)
			}
			out, err := transport.CallTool(t.Context(), "lookup", json.RawMessage(`{}`))
			if err != nil || len(out.Content) != 1 || out.Content[0].Text != "ok" || out.IsError {
				t.Fatalf("call: %v, %v", out, err)
			}
			transport.Close(t.Context())
			if !closed.Load() {
				t.Fatal("session not closed")
			}
		})
	}
}
func TestTransportRefusesRedirects(t *testing.T) {
	var redirected atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Store(true)
		w.WriteHeader(200)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	if _, err := DialTransport(t.Context(), fixturePolicy(), server.URL, []byte("private-bearer"), TransportOptions{ClientName: "test", Timeout: 10 * time.Second}); err != ErrTransport || strings.Contains(err.Error(), "private-bearer") || redirected.Load() {
		t.Fatalf("redirect: %v redirected=%v", err, redirected.Load())
	}
}
func TestDialTransportRequiresExplicitOptions(t *testing.T) {
	for _, opts := range []TransportOptions{{}, {ClientName: "test"}, {Timeout: 10 * time.Second}} {
		if _, err := DialTransport(t.Context(), fixturePolicy(), "http://127.0.0.1:1", nil, opts); err != ErrTransport {
			t.Fatalf("options %v: %v", opts, err)
		}
	}
}
func TestTransportConcurrentToolCallsUseDistinctIDs(t *testing.T) {
	var mu sync.Mutex
	seen := map[int]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("bad request")
			return
		}
		if request.Method == "tools/call" {
			mu.Lock()
			if seen[request.ID] {
				t.Errorf("duplicate request id %d", request.ID)
			}
			seen[request.ID] = true
			mu.Unlock()
		}
		var result any
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "fixture-session")
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}, "isError": false}
		default:
			t.Errorf("unexpected method %s", request.Method)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		w.Write(body)
	}))
	defer server.Close()
	transport, err := DialTransport(t.Context(), fixturePolicy(), server.URL, []byte("fixture-only"), TransportOptions{ClientName: "test", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer transport.Close(t.Context())
	const calls = 16
	errs := make(chan error, calls)
	var wg sync.WaitGroup
	for range calls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := transport.CallTool(t.Context(), "lookup", json.RawMessage(`{}`))
			if err == nil && (len(out.Content) != 1 || out.Content[0].Text != "ok") {
				err = fmt.Errorf("bad result %v", out)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent call: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != calls {
		t.Fatalf("distinct ids: %d", len(seen))
	}
}
func TestCertificationRejectsRemoteSchemasPrivateDestinationsAndReflectedSecrets(t *testing.T) {
	var escaped any
	if json.Unmarshal([]byte(`{"properties":{"token":{"default":"\u003cprivate-bearer\u003e"}}}`), &escaped) != nil || !reflectsCredential(escaped, "<private-bearer>") {
		t.Fatal("escaped authentication material was not recognized")
	}
	for _, schema := range []string{`{"type":"object","$ref":"https://secret.example/schema"}`, `{"type":"object","type":"string"}`, `{"type":"array"}`, `{"type":"object","properties":{"value":{"type":"not-a-type"}}}`} {
		if validateSchema(json.RawMessage(schema)) == nil {
			t.Errorf("unsafe schema accepted: %s", schema)
		}
	}
	if _, err := Certify(context.Background(), &egress.Policy{}, "http://127.0.0.1:1", nil); err != ErrCertification {
		t.Fatalf("private target: %v", err)
	}
	for _, problem := range []string{"reflection", "duplicate", "missing", "cursor"} {
		t.Run(problem, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				json.NewDecoder(r.Body).Decode(&request)
				if request.ID == 0 {
					w.WriteHeader(202)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				var result any
				if request.Method == "initialize" {
					result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
				} else {
					tool := map[string]any{"name": "lookup", "description": "safe", "inputSchema": map[string]any{"type": "object"}}
					tools := []any{tool}
					if problem == "reflection" {
						tool["description"] = "private-bearer"
					}
					if problem == "duplicate" {
						tools = append(tools, tool)
					}
					out := map[string]any{"tools": tools}
					if problem == "missing" {
						delete(out, "tools")
					}
					if problem == "cursor" {
						out["nextCursor"] = "same"
					}
					result = out
				}
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
			}))
			defer server.Close()
			if _, err := Certify(t.Context(), fixturePolicy(), server.URL, []byte("private-bearer")); err != ErrCertification || strings.Contains(err.Error(), "private-bearer") {
				t.Fatalf("unsafe catalog: %v", err)
			}
		})
	}
}

func TestTransportCertifiesCatalogOnAnEstablishedSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}
		var request struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("bad request")
			return
		}
		var result any
		switch request.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "fixture-session")
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "notifications/initialized":
			w.WriteHeader(202)
			return
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "lookup", "description": "Look up", "inputSchema": map[string]any{"type": "object"}}}}
		default:
			t.Errorf("certification invoked %s", request.Method)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		_, _ = w.Write(body)
	}))
	defer server.Close()
	certified, err := Certify(t.Context(), fixturePolicy(), server.URL, []byte("fixture-only"))
	if err != nil {
		t.Fatalf("certify: %v", err)
	}
	transport, err := DialTransport(t.Context(), fixturePolicy(), server.URL, []byte("fixture-only"), TransportOptions{ClientName: "test", Timeout: 10 * time.Second})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer transport.Close(t.Context())
	live, err := transport.CertifyCatalog(t.Context())
	if err != nil {
		t.Fatalf("certify on session: %v", err)
	}
	if live.Digest != certified.Digest || live.Protocol != certified.Protocol || len(live.Tools) != 1 || live.Tools[0].Name != "lookup" {
		t.Fatalf("session catalog disagrees with certification: live=%+v certified=%+v", live, certified)
	}
	if transport.ListChanged() {
		t.Fatal("quiet catalog listing reported a change notification")
	}
}

func TestTransportObservesToolListChangedNotifications(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notify string
		want   bool
	}{
		{"list changed", "notifications/tools/list_changed", true},
		{"other notification", "notifications/progress", false},
		{"no notification", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "DELETE" {
					w.WriteHeader(204)
					return
				}
				var request struct {
					ID     int    `json:"id"`
					Method string `json:"method"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					t.Error("bad request")
					return
				}
				switch request.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "fixture-session")
					w.Header().Set("Content-Type", "application/json")
					body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}})
					_, _ = w.Write(body)
				case "notifications/initialized":
					w.WriteHeader(202)
				case "tools/call":
					w.Header().Set("Content-Type", "text/event-stream")
					if tc.notify != "" {
						note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": tc.notify})
						fmt.Fprintf(w, "event: message\ndata: %s\n\n", note)
					}
					body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}})
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", body)
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				default:
					t.Errorf("unexpected method %s", request.Method)
					w.WriteHeader(400)
				}
			}))
			defer server.Close()
			transport, err := DialTransport(t.Context(), fixturePolicy(), server.URL, []byte("fixture-only"), TransportOptions{ClientName: "test", Timeout: 10 * time.Second})
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer transport.Close(t.Context())
			out, err := transport.CallTool(t.Context(), "lookup", json.RawMessage(`{}`))
			if err != nil || len(out.Content) != 1 || out.Content[0].Text != "ok" {
				t.Fatalf("call beside a notification failed: %v, %v", out, err)
			}
			if transport.ListChanged() != tc.want {
				t.Fatalf("list changed observation = %v, want %v", transport.ListChanged(), tc.want)
			}
		})
	}
}
