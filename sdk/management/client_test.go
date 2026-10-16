package management

import (
	"encoding/json"
	"os"
	"path/filepath"
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
