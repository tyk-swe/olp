package mcp

import (
	"context"
	"encoding/json"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/mcp/messages"
	"github.com/tyk-swe/olp/internal/mcp/sessions"
	"github.com/tyk-swe/olp/internal/oif"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestPricingStateFor(t *testing.T) {
	str := func(v string) *string { return &v }
	cases := []struct {
		price *string
		want  accounting.PricingState
	}{
		{nil, accounting.PricingUnpriced},
		{str("0"), accounting.PricingFree},
		{str("0.00"), accounting.PricingFree},
		{str("0.01"), accounting.PricingPriced},
		{str("10"), accounting.PricingPriced},
	}
	for _, tc := range cases {
		if got := accounting.PricingStateFor(tc.price); got != tc.want {
			t.Errorf("accounting.PricingStateFor(%v) = %q, want %q", tc.price, got, tc.want)
		}
	}
}

func TestReservationAmountDefaultsToZero(t *testing.T) {
	var nilReservation *accounting.Reservation
	if got := nilReservation.Amount(); got != "0" {
		t.Errorf("nil reservation amount = %q, want 0", got)
	}
	if got := (&accounting.Reservation{}).Amount(); got != "0" {
		t.Errorf("unpriced reservation amount = %q, want 0", got)
	}
	price := "2.50"
	if got := (&accounting.Reservation{UnitPrice: &price}).Amount(); got != price {
		t.Errorf("priced reservation amount = %q, want %q", got, price)
	}
}

func TestResolvedToolsetLookupPrecedence(t *testing.T) {
	resolved := toolsetFromResolution("toolset", "rev", routing.MCPToolsetResolution{
		Tools:   []routing.MCPToolsetTool{{Alias: "live", Name: "live_tool"}, {Alias: "both", Name: "both_tool"}},
		Drifted: []routing.MCPDriftedTool{{Alias: "both"}, {Alias: "drifted"}},
		Invalid: []routing.MCPDriftedTool{{Alias: "invalid"}},
		Missing: []routing.MCPDriftedTool{{Alias: "gone"}},
	})
	cases := []struct {
		alias string
		want  toolLookup
		name  string
	}{
		{"live", toolFound, "live_tool"},
		{"both", toolFound, "both_tool"},
		{"drifted", toolDrifted, ""},
		{"invalid", toolInvalidSchema, ""},
		{"gone", toolServerMissing, ""},
		{"unknown", toolMissing, ""},
	}
	for _, tc := range cases {
		tool, got := resolved.lookup(tc.alias)
		if got != tc.want || tool.Name != tc.name {
			t.Errorf("lookup(%q) = (%q, %v), want (%q, %v)", tc.alias, tool.Name, got, tc.name, tc.want)
		}
	}
}

func TestResolvedToolsetListingFailure(t *testing.T) {
	drifted := resolvedToolset{drifted: []routing.MCPDriftedTool{{Alias: "a"}}, invalid: []routing.MCPDriftedTool{{Alias: "b"}}}
	if got := drifted.listingFailure(); got != errToolDefinitionChanged {
		t.Errorf("drifted listingFailure = %v, want tool definition changed", got)
	}
	invalid := resolvedToolset{invalid: []routing.MCPDriftedTool{{Alias: "b"}}}
	if got := invalid.listingFailure(); got != errToolSchemaInvalid {
		t.Errorf("invalid listingFailure = %v, want tool schema invalid", got)
	}
	missingOnly := resolvedToolset{missing: []routing.MCPDriftedTool{{Alias: "c"}}}
	if got := missingOnly.listingFailure(); got != nil {
		t.Errorf("missing-only listingFailure = %v, want nil", got)
	}
}

func TestCanonicalUUID(t *testing.T) {
	if !canonicalUUID("123e4567-e89b-12d3-a456-426614174000") {
		t.Error("canonicalUUID rejected a canonical UUID")
	}
	for _, raw := range []string{"", "123e4567e89b12d3a456426614174000", "123E4567-E89B-12D3-A456-426614174000", "not-a-uuid"} {
		if canonicalUUID(raw) {
			t.Errorf("canonicalUUID accepted %q", raw)
		}
	}
}

func TestToolResultResponses(t *testing.T) {
	resolved := resolvedToolset{id: "toolset", revisionID: "revision"}
	tool := routing.MCPToolsetTool{Alias: "lookup"}
	for _, tc := range []struct {
		name   string
		result upstream.ToolResult
	}{
		{name: "nil content"},
		{name: "empty content", result: upstream.ToolResult{Content: []upstream.ToolContent{}}},
		{name: "structured error", result: upstream.ToolResult{Content: []upstream.ToolContent{{Type: "text", Text: "result"}}, StructuredContent: json.RawMessage(`{"value":42}`), IsError: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome := Execution{Outcome: Replayed{Receipt: Receipt{AttemptID: "attempt", DispatchID: "dispatch"}, Result: tc.result}}
			fresh, repeated := httptest.NewRecorder(), httptest.NewRecorder()
			writeToolResult(fresh, resolved, tool, tc.result, "attempt", "dispatch")
			writeExecution(repeated, resolved, tool, outcome)
			if fresh.Code != http.StatusOK || repeated.Code != fresh.Code || repeated.Body.String() != fresh.Body.String() {
				t.Fatalf("REST replay differs from fresh result: %s / %s", fresh.Body, repeated.Body)
			}
			var rest map[string]json.RawMessage
			if err := json.Unmarshal(fresh.Body.Bytes(), &rest); err != nil {
				t.Fatal(err)
			}
			content, err := json.Marshal(tc.result.Content)
			if err != nil {
				t.Fatal(err)
			}
			structured, err := json.Marshal(tc.result.StructuredContent)
			if err != nil {
				t.Fatal(err)
			}
			isError, err := json.Marshal(tc.result.IsError)
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]json.RawMessage{
				"toolset_id": json.RawMessage(`"toolset"`), "toolset_revision_id": json.RawMessage(`"revision"`), "name": json.RawMessage(`"lookup"`),
				"attempt_id": json.RawMessage(`"attempt"`), "dispatch_id": json.RawMessage(`"dispatch"`),
				"content": content, "structured_content": structured, "is_error": isError,
			}
			if !reflect.DeepEqual(rest, want) {
				t.Fatalf("unexpected REST response: %s", fresh.Body)
			}
			rpcFresh, rpcRepeated := httptest.NewRecorder(), httptest.NewRecorder()
			writeSessionToolResult(rpcFresh, json.RawMessage(`7`), resolved, tc.result, "attempt", "dispatch")
			writeSessionExecution(rpcRepeated, json.RawMessage(`7`), resolved, outcome)
			if rpcFresh.Code != http.StatusOK || rpcRepeated.Code != rpcFresh.Code || rpcRepeated.Body.String() != rpcFresh.Body.String() {
				t.Fatalf("RPC replay differs from fresh result: %s / %s", rpcFresh.Body, rpcRepeated.Body)
			}
			var rpc struct {
				JSONRPC string                     `json:"jsonrpc"`
				ID      int                        `json:"id"`
				Result  map[string]json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(rpcFresh.Body.Bytes(), &rpc); err != nil {
				t.Fatal(err)
			}
			wantContent := func() json.RawMessage {
				if tc.result.Content == nil {
					return json.RawMessage(`[]`)
				}
				return content
			}()
			if rpc.JSONRPC != "2.0" || rpc.ID != 7 || string(rpc.Result["content"]) != string(wantContent) || string(rpc.Result["isError"]) != string(isError) {
				t.Fatalf("unexpected RPC response: %s", rpcFresh.Body)
			}
			if string(rpc.Result["structuredContent"]) != string(tc.result.StructuredContent) {
				t.Fatalf("unexpected RPC structured content: %s", rpcFresh.Body)
			}
			var meta map[string]string
			if err := json.Unmarshal(rpc.Result["_meta"], &meta); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(meta, map[string]string{"olp.attempt_id": "attempt", "olp.dispatch_id": "dispatch", "olp.toolset_id": "toolset", "olp.toolset_revision_id": "revision"}) {
				t.Fatalf("unexpected RPC metadata: %+v", meta)
			}
		})
	}
}

func TestRPCRequestIDsRoundTripWithoutConversion(t *testing.T) {
	for _, id := range []string{`9007199254740993`, `1.234567890123456789`, `"request-1"`, `0`} {
		t.Run(id, func(t *testing.T) {
			raw := []byte(`{"jsonrpc":"2.0","id":` + id + `,"method":"ping"}`)
			parsed, code := messages.Parse(raw, oif.Limits{MaxBytes: maxCallBodyBytes})
			if code != 0 || parsed.Kind != messages.RequestMessage || string(parsed.ID) != id {
				t.Fatalf("request ID changed: %+v, code %d", parsed, code)
			}
			recorder := httptest.NewRecorder()
			writeRPCResult(recorder, http.StatusOK, parsed.ID, struct{}{})
			reply, code := messages.Parse(recorder.Body.Bytes(), oif.Limits{MaxBytes: maxCallBodyBytes})
			if code != 0 || reply.Kind != messages.Response || string(reply.ID) != id || string(reply.Result) != "{}" {
				t.Fatalf("response ID or empty result changed: %s", recorder.Body.String())
			}
			var managementRequest messages.Request
			if err := json.Unmarshal(raw, &managementRequest); err != nil || !managementRequest.Valid() {
				t.Fatalf("management rejected consumer request ID: %v", err)
			}
		})
	}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":null,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":true,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":1,"id":2,"method":"ping"}`,
		`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`,
	} {
		if _, code := messages.Parse([]byte(raw), oif.Limits{MaxBytes: maxCallBodyBytes}); code == 0 {
			t.Fatalf("accepted invalid envelope %s", raw)
		}
	}
}

func TestSharedSessionRejectsOtherIdentities(t *testing.T) {
	issuer := "issuer"
	binding := func(installation, project, individual, delegation string) sessions.Binding {
		return sessions.BindingOf(access.Authority{ID: "key", InstallationID: installation, ProjectID: &project, WorkloadIssuerID: &issuer, WorkloadDigest: "workload", EndUserDigest: individual, DelegationTokenDigest: delegation})
	}
	owner := binding("installation", "project", "person", "delegation")
	store, err := sessions.New(&sessionReplySource{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(t.Context(), sessions.Record{ID: "session", Binding: owner, Protocol: messages.SupportedProtocols()[0], ToolsetID: "toolset", ToolsetRevisionID: "revision", ReleaseID: "release"})
	if err != nil || !created {
		t.Fatalf("create session: %v, %v", created, err)
	}
	record, found, err := store.Access(t.Context(), "session", owner)
	if err != nil || !found || record.ToolsetID != "toolset" || record.ToolsetRevisionID != "revision" {
		t.Fatalf("owner could not recover its pinned session: %+v, %v", record, err)
	}
	for _, stranger := range []sessions.Binding{
		binding("other", "project", "person", "delegation"),
		binding("installation", "other", "person", "delegation"),
		binding("installation", "project", "other", "delegation"),
		binding("installation", "project", "person", "other"),
	} {
		if _, found, err := store.Access(t.Context(), "session", stranger); err != nil || found {
			t.Fatalf("session accepted another identity: found %v, error %v", found, err)
		}
	}
}

type sessionReplySource struct{ fields []any }

func (s *sessionReplySource) Do(_ context.Context, args ...string) (any, error) {
	if len(args) < 15 {
		return s.fields, nil
	}
	s.fields = []any{
		"installation_id", args[8], "project_id", args[9], "principal_digest", args[10],
		"protocol", args[11], "toolset_id", args[12], "toolset_revision_id", args[13], "release_id", args[14],
		"created_at_ms", "0", "last_active_at_ms", "0", "initialized", "0",
	}
	return int64(1), nil
}
