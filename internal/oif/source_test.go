package oif_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func FuzzSourceRetainsValidNativeLexemes(f *testing.F) {
	for _, s := range []string{`{"n":9007199254740993}`, `{"x":"\ud83d\ude00"}`, `[]`, `null`, `{"a":[false,0,""]}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := oif.ParseJSON(data, oif.Limits{MaxBytes: 8192, MaxNodes: 256, MaxDepth: 16})
		if err != nil {
			return
		}
		if !json.Valid(d.Bytes()) || !bytes.Equal(data, d.Bytes()) {
			t.Fatal("successful source parse changed JSON")
		}
		if strings.Contains(d.Raw(), "\xff") {
			t.Fatal("invalid UTF-8 accepted")
		}
	})
}

// decoded is what encoding/json makes of a JSON string, which Text and Chars
// are held to for every string a document can hold.
func decoded(t testing.TB, raw string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return s
}

func FuzzStringsDecodeAsEncodingJSONDecodesThem(f *testing.F) {
	for _, s := range []string{`{"a":"😀 x\n\"y\"\\"}`, `["\u0000\u001f","é\/","\\\\u0041"]`, `"plain"`, `{"k\n":{"k":["\t"]}}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := oif.ParseJSON(data, oif.Limits{MaxBytes: 8192, MaxNodes: 256, MaxDepth: 16})
		if err != nil {
			return
		}
		var visit func(v oif.Value)
		visit = func(v oif.Value) {
			switch v.Kind() {
			case oif.String:
				want := decoded(t, v.Raw())
				if got, ok := v.Text(); !ok || got != want {
					t.Fatalf("Text of %q is %q, want %q", v.Raw(), got, want)
				}
				if got, ok := v.Chars(); !ok || got != want {
					t.Fatalf("Chars of %q is %q, want %q", v.Raw(), got, want)
				}
			case oif.Array:
				for _, e := range v.Elements() {
					visit(e)
				}
			case oif.Object:
				for _, m := range v.Members() {
					visit(m.Value)
				}
			}
		}
		visit(d.Root())
	})
}

func TestParseJSONRejectsDuplicateKeysAtAnyObjectSize(t *testing.T) {
	for _, raw := range []string{
		`{"a":1,"a":2}`,
		`{"a":1,"b":{"a":1,"a":2}}`,
		"{\"a\":1,\"\\u0061\":2}",
	} {
		if _, err := oif.ParseJSON([]byte(raw), oif.Limits{}); err == nil {
			t.Fatalf("ParseJSON(%s) succeeded, want duplicate_key error", raw)
		}
	}
	// An object past the small-object scan must still reject a repeat of
	// its first member (its last member repeats it below).
	var b strings.Builder
	b.WriteString(`{"k0":0`)
	for i := 1; i < 40; i++ {
		fmt.Fprintf(&b, `,"k%d":0`, i)
	}
	b.WriteString(`,"k0":1}`)
	if _, err := oif.ParseJSON([]byte(b.String()), oif.Limits{}); err == nil {
		t.Fatal("ParseJSON of a large object with a repeated key succeeded, want duplicate_key error")
	}
	b.Reset()
	b.WriteString(`{"k0":0`)
	for i := 1; i < 40; i++ {
		fmt.Fprintf(&b, `,"k%d":0`, i)
	}
	b.WriteByte('}')
	if _, err := oif.ParseJSON([]byte(b.String()), oif.Limits{}); err != nil {
		t.Fatalf("ParseJSON of a large object with distinct keys: %v", err)
	}
}

// Left out of a copy, and not out of the document.

func TestValueBoundsSelectTheRequestedNativeLexeme(t *testing.T) {
	body := []byte("{ \"other\":\"same\", \"ne\\u0073ted\": { \"mo\\u0064el\":\"same\" }, \"number\":1.2300 }")
	document, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	value, ok := document.Lookup("/nested/model")
	if !ok {
		t.Fatal("missing model")
	}
	start, end := value.Bounds()
	patched := string(body[:start]) + `"selected"` + string(body[end:])
	if patched != "{ \"other\":\"same\", \"ne\\u0073ted\": { \"mo\\u0064el\":\"selected\" }, \"number\":1.2300 }" {
		t.Fatalf("range changed unrelated bytes %s", patched)
	}
}
