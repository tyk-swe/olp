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
	if err == nil {
		t.Fatal("accepted an array for a scalar query")
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
		Responses map[string]struct {
			Content map[string]any `json:"content"`
		} `json:"responses"`
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
			jsonResponses := true
			for status, response := range op.Responses {
				if !strings.HasPrefix(status, "2") || len(response.Content) == 0 {
					continue
				}
				supported := false
				for mediaType := range response.Content {
					if mediaType == "application/json" || strings.HasSuffix(mediaType, "+json") {
						supported = true
					}
				}
				jsonResponses = jsonResponses && supported
			}
			if !jsonResponses {
				continue
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

func TestClientPreservesAtomicParentMutationMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("OLP-Previous-Parent-ETag", `"previous"`)
		w.Header().Set("OLP-Parent-ETag", `"current"`)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := NewClient(server.URL, tokenFile(t, "private-token"))
	if err != nil {
		t.Fatal(err)
	}
	operation, _ := Lookup("delete_credential_slot")
	result, err := client.Call(t.Context(), operation, Arguments{Path: map[string]string{"provider_id": "p", "slot_id": "s"}, IfMatch: "observed", IdempotencyKey: "same-intent"})
	if err != nil || result.PreviousParentETag != `"previous"` || result.ParentETag != `"current"` {
		t.Fatalf("parent metadata lost: %v", err)
	}
}

func TestRequestSerializesDeclaredScalarQueriesLosslessly(t *testing.T) {
	operation := Operation{Method: "GET", Path: "/api/v1/scalars", Parameters: []Parameter{
		{Name: "count", In: "query", Type: "integer"},
		{Name: "active", In: "query", Type: "boolean"},
		{Name: "name", In: "query", Type: "string"},
	}}
	for _, value := range []any{int(0), int8(0), int16(0), int32(0), int64(0), uint(0), uint8(0), uint16(0), uint32(0), uint64(0), float64(0), json.Number("0")} {
		request, err := operation.Request(t.Context(), Arguments{Query: map[string]any{"count": value, "active": false, "name": nil}})
		if err != nil || request.URL.RawQuery != "active=false&count=0" {
			t.Fatalf("%T: request=%v err=%v", value, request, err)
		}
	}
	for _, input := range []struct {
		value any
		want  string
	}{
		{int64(-9223372036854775808), "-9223372036854775808"},
		{uint64(18446744073709551615), "18446744073709551615"},
		{json.Number("9007199254740993"), "9007199254740993"},
		{float64(42), "42"},
	} {
		request, err := operation.Request(t.Context(), Arguments{Query: map[string]any{"count": input.value}})
		if err != nil || request.URL.Query().Get("count") != input.want {
			t.Fatalf("%T: request=%v err=%v", input.value, request, err)
		}
	}
	for _, query := range []map[string]any{
		{"count": "1"}, {"count": 1.5}, {"count": float64(1 << 53)}, {"count": []int{1}},
		{"count": json.Number("01")}, {"count": json.Number("1.5")}, {"count": json.Number("NaN")},
		{"active": "false"}, {"name": true},
	} {
		if _, err := operation.Request(t.Context(), Arguments{Query: query}); err == nil {
			t.Fatalf("accepted %#v", query)
		}
	}
	operation.Parameters[0].Required = true
	for _, query := range []map[string]any{nil, {"count": nil}} {
		if _, err := operation.Request(t.Context(), Arguments{Query: query}); err == nil {
			t.Fatal("accepted missing required integer")
		}
	}
}

func TestResponseMetadataPreservesOnlyDeclaredSafeFacts(t *testing.T) {
	operation, _ := Lookup("put_credential_slot")
	header := http.Header{}
	header.Set("ETag", `"child"`)
	header.Set("OLP-Previous-Parent-ETag", `"previous"`)
	header.Set("OLP-Parent-ETag", `"current"`)
	header.Set("Authorization", "private-token")
	metadata := operation.ResponseMetadata(header)
	if metadata["etag"] != `"child"` || metadata["previous_parent_etag"] != `"previous"` || metadata["parent_etag"] != `"current"` || len(metadata) != 3 {
		t.Fatalf("metadata=%v", metadata)
	}
	read, _ := Lookup("list_routes")
	if facts := read.ResponseMetadata(header); len(facts) != 1 || facts["etag"] != `"child"` {
		t.Fatal("exposed headers absent from this operation's response contract")
	}
}

func TestClientRequiresCallerOwnedMutationIdentityBeforeDispatch(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	defer server.Close()
	client, err := NewClient(server.URL, tokenFile(t, "private-token"))
	if err != nil {
		t.Fatal(err)
	}
	operation, _ := Lookup("create_provider")
	_, err = client.Call(t.Context(), operation, Arguments{Body: json.RawMessage(`{}`)})
	if err == nil || called || !strings.Contains(err.Error(), "retain the same key") {
		t.Fatalf("missing caller identity: called=%v err=%v", called, err)
	}
}

func TestClientReplaysCallerIdentityAfterCommittedResponseLoss(t *testing.T) {
	attempts := 0
	var committedKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		key := r.Header.Get("Idempotency-Key")
		if attempts == 1 {
			committedKey = key
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			connection.Close() // The write committed; its response never arrives.
			return
		}
		if key != committedKey {
			t.Errorf("new mutation identity %q after %q", key, committedKey)
		}
		w.Write([]byte(`{"id":"same-created-provider"}`))
	}))
	defer server.Close()
	client, err := NewClient(server.URL, tokenFile(t, "private-token"))
	if err != nil {
		t.Fatal(err)
	}
	operation, _ := Lookup("create_provider")
	intent := Arguments{Body: json.RawMessage(`{"name":"same intent"}`), IdempotencyKey: "owned-intent"}
	if _, err = client.Call(t.Context(), operation, intent); err == nil {
		t.Fatal("lost response appeared successful")
	}
	response, err := client.Call(t.Context(), operation, intent)
	if err != nil || string(response.Body) != `{"id":"same-created-provider"}` || attempts != 2 {
		t.Fatalf("response=%v attempts=%d err=%v", response, attempts, err)
	}
}
