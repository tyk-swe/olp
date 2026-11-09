package vendor

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"testing"
)

func TestPurviewRestrictions(t *testing.T) {
	for _, tc := range []struct {
		name, actions      string
		flagged, malformed bool
	}{
		{"block", `[{"@odata.type":"#microsoft.graph.restrictAccessAction","restrictionAction":"block"}]`, true, false},
		{"warn", `[{"@odata.type":"#microsoft.graph.restrictAccessAction","restrictionAction":"warn"}]`, false, false},
		{"audit", `[{"action":"restrictAccess","restrictionAction":"audit"}]`, false, false},
		{"missing", `[{"@odata.type":"#microsoft.graph.restrictAccessAction"}]`, false, true},
		{"null", `[{"action":"restrictAccess","restrictionAction":null}]`, false, true},
		{"unknown", `[{"action":"restrictAccess","restrictionAction":"unknownFutureValue"}]`, false, true},
		{"malformed after block", `[{"action":"restrictAccess","restrictionAction":"block"},{"action":"restrictAccess"}]`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := serve(t, reply(`{"policyActions":`+tc.actions+`}`))
			decision, err := evaluate(t, server, "microsoft_purview", configuration(server.URL, purviewApplication), Request{Role: User, Text: "private"})
			if (err != nil) != tc.malformed || err == nil && decision.Flagged() != tc.flagged {
				t.Fatalf("decision=%+v error=%v", decision, err)
			}
		})
	}
}

func TestConductIdentityAndFraming(t *testing.T) {
	for _, tc := range []struct {
		name                string
		status              int
		result              map[string]any
		text                string
		wantFlag, wantError bool
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, wantFlag: true},
		{name: "forbidden", status: http.StatusForbidden, wantFlag: true},
		{name: "unavailable", status: http.StatusServiceUnavailable, wantError: true},
		{name: "identity_marker", result: map[string]any{"structuredContent": map[string]bool{"identity_required": true}}, wantFlag: true},
		{name: "error_result", result: map[string]any{"isError": true}, wantError: true},
		{name: "allow", text: "ok"},
		{name: "block", text: "BLOCKED — policy", wantFlag: true},
		{name: "workspace_prefix_block", text: "[ws:workspace] BLOCKED — policy", wantFlag: true},
		{name: "workspace_prefix_allow", text: "[ws:workspace] ok"},
		{name: "missing_verdict", text: "", wantError: true},
		{name: "unrecognized_verdict", text: "okay to proceed", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := serve(t, func(w http.ResponseWriter, r *http.Request) {
				var request map[string]any
				_ = json.NewDecoder(r.Body).Decode(&request)
				if r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("X-Workspace-Id") != "workspace" || request["method"] != "tools/call" {
					t.Error("wrong Conduct request")
				}
				if tc.status != 0 {
					w.WriteHeader(tc.status)
					return
				}
				result := tc.result
				if result == nil {
					result = map[string]any{"content": []any{map[string]string{"type": "text", "text": tc.text}}}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": result})
			})
			decision, err := evaluate(t, server, "conduct", configuration(server.URL, `"parameters":{"workspace_id":"workspace"}`), Request{Role: User, Text: "content"})
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v, want error=%v", err, tc.wantError)
			}
			if err == nil && decision.Flagged() != tc.wantFlag {
				t.Fatalf("decision=%+v, want flagged=%v", decision, tc.wantFlag)
			}
		})
	}
}

func TestHiddenlayerOAuth(t *testing.T) {
	tokenCalls, scanCalls := 0, 0
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/token" {
			tokenCalls++
			id, password, ok := r.BasicAuth()
			if !ok || id != "client" || password != "secret" || r.URL.Query().Get("grant_type") != "client_credentials" {
				t.Error("incorrect OAuth request")
			}
			_, _ = io.WriteString(w, `{"access_token":"sealed-token"}`)
			return
		}
		scanCalls++
		if r.Header.Get("Authorization") != "Bearer sealed-token" {
			t.Error("missing access token")
		}
		_, _ = io.WriteString(w, `{"evaluation":{"action":"Allow"}}`)
	})
	_, err := evaluate(t, server, "hiddenlayer", configuration(server.URL+"/scan", `"auth_endpoint":`+strconv.Quote(server.URL+"/oauth2/token")), Request{Role: User, Text: "original"})
	if err != nil || tokenCalls != 1 || scanCalls != 1 {
		t.Fatalf("error %v tokens %d scans %d", err, tokenCalls, scanCalls)
	}
}
