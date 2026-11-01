package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

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

// The write committed; its response never arrives.

func TestConditionalWritesPreserveResponseETags(t *testing.T) {
	for _, etag := range []string{`W/"current"`, `"current"`} {
		t.Run(etag, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					w.Header().Set("ETag", etag)
					w.Write([]byte(`{"id":"user"}`))
					return
				}
				if r.Header.Get("If-Match") != etag {
					w.WriteHeader(http.StatusPreconditionFailed)
					w.Write([]byte(`{"type":"https://openllmproxy.dev/problems/invalid_version"}`))
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			token := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(token, []byte("fixture-token"), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := NewClient(server.URL, token)
			if err != nil {
				t.Fatal(err)
			}
			resource, _ := Lookup("scim_users_get")
			response, err := client.Call(t.Context(), resource, Arguments{Path: map[string]string{"scim_id": "user"}})
			if err != nil {
				t.Fatal(err)
			}
			patch, _ := Lookup("scim_users_patch")
			if _, err := client.Call(t.Context(), patch, Arguments{Path: map[string]string{"scim_id": "user"}, Body: json.RawMessage(`{"schemas":[],"Operations":[]}`), IfMatch: response.ETag}); err != nil {
				t.Fatalf("unchanged response ETag failed conditional write: %v", err)
			}
			if etag == `"current"` {
				if _, err := client.Call(t.Context(), patch, Arguments{Path: map[string]string{"scim_id": "user"}, Body: json.RawMessage(`{"schemas":[],"Operations":[]}`), IfMatch: "current"}); err != nil {
					t.Fatalf("bare tag failed conditional write: %v", err)
				}
			}
			_, stale := client.Call(t.Context(), patch, Arguments{Path: map[string]string{"scim_id": "user"}, Body: json.RawMessage(`{"schemas":[],"Operations":[]}`), IfMatch: `W/"stale"`})
			var problem *APIError
			if !errors.As(stale, &problem) || problem.Status != http.StatusPreconditionFailed {
				t.Fatalf("stale write: %v", stale)
			}
		})
	}
}
