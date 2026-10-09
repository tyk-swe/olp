package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tokenFile(t *testing.T, secret string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(secret+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClientUsesContractPreconditionsAndReloadsToken(t *testing.T) {
	tokens := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokens = append(tokens, r.Header.Get("Authorization"))
		if r.URL.Path != "/api/v1/providers/p/activate" || r.Method != "POST" || r.Header.Get("If-Match") != `"observed"` || r.Header.Get("Idempotency-Key") != "retry-key" {
			t.Errorf("request = %s %s %v", r.Method, r.URL, r.Header)
		}
		w.Header().Set("ETag", `"next"`)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	path := tokenFile(t, "first")
	client, err := NewClient(server.URL, path)
	if err != nil {
		t.Fatal(err)
	}
	op, _ := Lookup("activate_provider")
	input := Arguments{Path: map[string]string{"provider_id": "p"}, IfMatch: "observed", IdempotencyKey: "retry-key"}
	for _, token := range []string{"first", "rotated"} {
		if err := os.WriteFile(path, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		response, err := client.Call(t.Context(), op, input)
		if err != nil || response.ETag != `"next"` {
			t.Fatalf("response = %+v, %v", response, err)
		}
	}
	if strings.Join(tokens, ",") != "Bearer first,Bearer rotated" {
		t.Fatalf("tokens = %v", tokens)
	}
}

func TestClientRefusesRedirectsAndRedactsErrors(t *testing.T) {
	called := false
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer destination.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	defer server.Close()
	client, err := NewClient(server.URL, tokenFile(t, "private-token"))
	if err != nil {
		t.Fatal(err)
	}
	op, _ := Lookup("list_routes")
	_, err = client.Call(t.Context(), op, Arguments{})
	if err == nil || called || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("redirect: called=%v err=%v", called, err)
	}
	problem := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"type":"https://openllmproxy.dev/problems/validation_failed","detail":"private-token"}`))
	}))
	defer problem.Close()
	client, err = NewClient(problem.URL, tokenFile(t, "private-token"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Call(t.Context(), op, Arguments{})
	if err == nil || !strings.Contains(err.Error(), "validation_failed") || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("error = %v", err)
	}
}

func TestRequestRefusesPathTraversalMissingConditionsAndUnknownParameters(t *testing.T) {
	op, _ := Lookup("activate_provider")
	for _, args := range []Arguments{
		{Path: map[string]string{"provider_id": ".."}, IfMatch: "observed", IdempotencyKey: "key"},
		{Path: map[string]string{"provider_id": "a/b"}, IfMatch: "observed", IdempotencyKey: "key"},
		{Path: map[string]string{"provider_id": "p"}, IdempotencyKey: "key"},
		{Path: map[string]string{"provider_id": "p"}, IfMatch: "observed"},
		{Path: map[string]string{"provider_id": "p"}, IfMatch: "observed", IdempotencyKey: "key", Query: map[string]any{"secret": "hidden"}},
	} {
		if _, err := op.Request(context.Background(), args); err == nil {
			t.Errorf("accepted %+v", args)
		}
	}
	usage, _ := Lookup("usage_summary")
	if _, err := usage.Request(t.Context(), Arguments{}); err == nil {
		t.Fatal("accepted missing usage range")
	}
	_, err := usage.Request(t.Context(), Arguments{Query: map[string]any{"start": []any{"2026-10-01"}, "end": "2026-10-09"}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegistryCoversEveryTokenJSONOperation(t *testing.T) {
	data, err := os.ReadFile("../../openapi/management.json")
	if err != nil {
		t.Fatal(err)
	}
	type operation struct {
		Name     string                `json:"operationId"`
		Security []map[string][]string `json:"security"`
		Body     *struct {
			Content map[string]any `json:"content"`
		} `json:"requestBody"`
	}
	var document struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err = json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	expected := map[string]bool{}
	for path, item := range document.Paths {
		for method, raw := range item {
			if method != "get" && method != "post" && method != "put" && method != "patch" && method != "delete" {
				continue
			}
			var op operation
			if err := json.Unmarshal(raw, &op); err != nil {
				t.Fatal(err)
			}
			token := false
			for _, alternative := range op.Security {
				if _, ok := alternative["managementToken"]; ok {
					token = true
				}
			}
			if !token || path == "/api/v1/mcp" {
				continue
			}
			if op.Body != nil {
				if _, ok := op.Body.Content["application/json"]; !ok {
					continue
				}
			}
			expected[op.Name] = true
		}
	}
	for _, op := range Operations() {
		if !expected[op.Name] {
			t.Errorf("undeclared operation %s", op.Name)
		}
		delete(expected, op.Name)
		var schema map[string]any
		if err := json.Unmarshal(op.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
	}
	if len(expected) != 0 {
		t.Fatalf("missing operations: %v", expected)
	}
}

func TestClientOriginAndTokenBounds(t *testing.T) {
	for _, path := range []string{"https://other.example.com/api/v1/keys", "//other.example.com/api/v1/keys", "/v1/chat/completions"} {
		if _, err := (Operation{Method: "GET", Path: path}).Request(t.Context(), Arguments{}); err == nil {
			t.Errorf("accepted foreign operation %s", path)
		}
	}
	for _, origin := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com/api", "https://example.com?secret=one", "file:///tmp/token"} {
		if _, err := NewClient(origin, "file"); err == nil {
			t.Errorf("accepted %s", origin)
		}
	}
	for _, secret := range []string{"", "a\nb", strings.Repeat("a", 4097)} {
		if _, err := ReadTokenFile(tokenFile(t, secret)); err == nil {
			t.Error("accepted invalid token")
		}
	}
}
