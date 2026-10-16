//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

const nativeConfigurationCorpus = `{"negative_zero":-0,"tiny_exponent":1e-1000,"long_decimal":0.1000000000000000000001,"unsafe_integer":9007199254740993,"null":null,"false":false,"zero":0,"empty":"","empty_array":[],"ordered":[false,0,"",null],"schema":{"__proto__":{"inert":true},"properties":{"z":{"type":"string"},"a":{"type":"number"}},"required":["z","a"]},"unknown":{"blocks":[{"id":"second"},{"id":"first"}]},"revision_marker":"first"}`

func nativeJSONAt(t *testing.T, raw []byte, path ...string) json.RawMessage {
	t.Helper()
	for _, component := range path {
		if len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '[' {
			var array []json.RawMessage
			index, err := strconv.Atoi(component)
			if err != nil || json.Unmarshal(raw, &array) != nil || index < 0 || index >= len(array) {
				t.Fatalf("native JSON array path is absent: %s", strings.Join(path, "/"))
			}
			raw = array[index]
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatalf("native JSON object path is invalid: %s", strings.Join(path, "/"))
		}
		var present bool
		raw, present = fields[component]
		if !present {
			t.Fatalf("native JSON path is absent: %s", strings.Join(path, "/"))
		}
	}
	return raw
}

func nativeJSONString(t *testing.T, raw []byte, path ...string) string {
	t.Helper()
	var value string
	if err := json.Unmarshal(nativeJSONAt(t, raw, path...), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func nativeWant(t *testing.T, h *accessHarness, owner *browser, method, path string, body any, headers map[string]string, status int) []byte {
	t.Helper()
	response, raw := h.do(owner, method, path, body, headers)
	if response.StatusCode != status {
		t.Fatalf("%s %s: status %d, want %d", method, path, response.StatusCode, status)
	}
	return raw
}

func nativeETag(t *testing.T, raw []byte) map[string]string {
	t.Helper()
	return map[string]string{"If-Match": `"` + nativeJSONString(t, raw, "etag") + `"`}
}

func requireNativeConfiguration(t *testing.T, configuration []byte, corpus string) {
	t.Helper()
	for _, path := range [][]string{
		{"options", "operation_defaults", "generation", "native_options", "fixture_provider"},
		{"options", "bindings", vendorModel, "defaults", "generation", "native_options", "fixture_binding"},
	} {
		var actual bytes.Buffer
		if err := json.Compact(&actual, nativeJSONAt(t, configuration, path...)); err != nil {
			t.Fatal(err)
		}
		if actual.String() != corpus {
			t.Fatalf("native source changed at %s: got %s", strings.Join(path, "/"), actual.String())
		}
	}
	if string(nativeJSONAt(t, configuration, "options", "operation_defaults", "generation", "values", "seed")) != "9007199254740993" {
		t.Fatal("typed native seed was rounded or stringified")
	}
}

// A separate manager must verify and install the exact persisted snapshot,
// then serve those same native defaults to the independent local provider.

// Corrupt source without changing its recorded digest. A new reader must
// refuse the release, never normalize the source or repair its hash.
