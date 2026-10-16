package management

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
)

func TestMCPRedactionPreservesSchemaNamesWithoutExposingData(t *testing.T) {
	body := []byte(`{"access_token":"one-time-value","input_schema":{"type":"object","properties":{"access_token":{"type":"string"},"private_key":{"$ref":"#/$defs/password"}},"required":["access_token"],"$defs":{"password":{"type":"string"}},"examples":[{"access_token":"private-example"}]}}`)
	value, err := decodeMCPBody(body)
	if err != nil {
		t.Fatal(err)
	}
	redactMCP(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{`"access_token":{"type":"string"}`, `"private_key":{"$ref":"#/$defs/password"}`, `"password":{"type":"string"}`} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("schema identity lost: %s", encoded)
		}
	}
	if strings.Contains(string(encoded), "one-time-value") || strings.Contains(string(encoded), "private-example") {
		t.Fatalf("credential data retained: %s", encoded)
	}
}

func TestMCPPreservesAtomicParentMetadataThroughOrdinaryHandlerReplay(t *testing.T) {
	server := MCP{API: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") != `"observed"` || r.Header.Get("Idempotency-Key") != "same-intent" {
			t.Fatalf("lost mutation conditions: %v", r.Header)
		}
		w.Header().Set("ETag", `"slot"`)
		w.Header().Set("OLP-Previous-Parent-ETag", `"before"`)
		w.Header().Set("OLP-Parent-ETag", `"after"`)
		w.Header().Set("Authorization", "private")
		w.Write([]byte(`{"secret":"private","id":"slot"}`))
	})}
	input := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"put_credential_slot","arguments":{"path":{"provider_id":"p","slot_id":"s"},"if_match":"observed","idempotency_key":"same-intent","body":{"enabled":true}}}}`)
	for range 2 {
		request := httptest.NewRequest("POST", "/api/v1/mcp", bytes.NewReader(input))
		request.Header.Set("Content-Type", "application/json")
		reply, err := server.serveMCP(request, access.Principal{User: access.User{Role: "owner"}, Kind: "user", AllProjects: true})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(reply.Body)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Result struct {
				Structured map[string]any `json:"structuredContent"`
			} `json:"result"`
		}
		if err = json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		facts := result.Result.Structured
		if facts["etag"] != `"slot"` || facts["previous_parent_etag"] != `"before"` || facts["parent_etag"] != `"after"` {
			t.Fatalf("lost parent transition: %s", encoded)
		}
		if strings.Contains(string(encoded), "private") {
			t.Fatalf("exposed secret: %s", encoded)
		}
	}
}
