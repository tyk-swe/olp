package mcpservers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

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
