//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
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

func TestNativeConfigurationPublicRevisionReplayPromotionAndReload(t *testing.T) {
	vendor := newVendor(t)
	var mu sync.Mutex
	var captured []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			mu.Lock()
			captured = append([]byte(nil), body...)
			mu.Unlock()
		}
		vendor.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(upstream.Close)
	h := newAccessHarness(t)
	owner := h.owner()
	configuration := map[string]any{
		"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": upstream.URL + "/v1", "profile_id": "compatible-chat", "profile_revision": "1",
		"options": map[string]any{
			"operation_defaults": map[string]any{"generation": map[string]any{"dialect": "openai-chat", "values": map[string]any{"seed": json.RawMessage("9007199254740993")}, "native_options": map[string]any{"fixture_provider": json.RawMessage(nativeConfigurationCorpus)}}},
			"bindings":           map[string]any{vendorModel: map[string]any{"defaults": map[string]any{"generation": map[string]any{"dialect": "openai-chat", "native_options": map[string]any{"fixture_binding": json.RawMessage(nativeConfigurationCorpus)}}}}},
		},
	}
	created := nativeWant(t, h, owner, "POST", "/api/v3/providers", map[string]any{"name": "Native source", "model": vendorModel, "credential": vendorSecret, "configuration": configuration}, idem("native-corpus-create"), 201)
	providerID := nativeJSONString(t, created, "id")
	path := "/api/v3/providers/" + providerID
	detail := nativeWant(t, h, owner, "GET", path, nil, nil, 200)
	requireNativeConfiguration(t, nativeJSONAt(t, detail, "configuration"), nativeConfigurationCorpus)
	detail = nativeWant(t, h, owner, "PATCH", path, map[string]any{"name": "Native source renamed", "configuration": nativeJSONAt(t, detail, "configuration")}, nativeETag(t, detail), 200)
	requireNativeConfiguration(t, nativeJSONAt(t, detail, "configuration"), nativeConfigurationCorpus)
	certifyProfileNetworkProvider(t, h, owner, providerID)
	detail = nativeWant(t, h, owner, "GET", path, nil, nil, 200)
	firstRevision := string(nativeJSONAt(t, detail, "active_revision"))
	revision := nativeWant(t, h, owner, "GET", path+"/revisions/"+firstRevision, nil, nil, 200)
	requireNativeConfiguration(t, nativeJSONAt(t, revision, "configuration"), nativeConfigurationCorpus)
	slug, key := publishProfileNetworkRoute(t, h, owner, providerID)

	// A separate manager must verify and install the exact persisted snapshot,
	// then serve those same native defaults to the independent local provider.
	reloaded := newAccessHarnessOn(t, h.Pool, h.DBURL)
	reloaded.refresh()
	if reloaded.Runtime.Release().Digest != h.Runtime.Release().Digest {
		t.Fatal("fresh runtime reader changed the recorded serving digest")
	}
	status, _, _ := reloaded.gateway("POST", "/v1/chat/completions", key, map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "native defaults after reload"}}})
	if status != 200 {
		t.Fatalf("serving reloaded native configuration: status %d", status)
	}
	mu.Lock()
	served := append([]byte(nil), captured...)
	mu.Unlock()
	for _, field := range []string{"fixture_provider", "fixture_binding"} {
		var compact bytes.Buffer
		if err := json.Compact(&compact, nativeJSONAt(t, served, field)); err != nil || compact.String() != nativeConfigurationCorpus {
			t.Fatalf("runtime reload changed provider-bound native field %s", field)
		}
	}
	if string(nativeJSONAt(t, served, "seed")) != "9007199254740993" {
		t.Fatal("reloaded runtime rounded the provider-bound seed")
	}

	secondCorpus := strings.ReplaceAll(nativeConfigurationCorpus, `"revision_marker":"first"`, `"revision_marker":"second"`)
	secondConfiguration := json.RawMessage(strings.ReplaceAll(string(nativeJSONAt(t, detail, "configuration")), `"revision_marker":"first"`, `"revision_marker":"second"`))
	detail = nativeWant(t, h, owner, "PATCH", path, map[string]any{"name": "Native source renamed", "configuration": secondConfiguration}, nativeETag(t, detail), 200)
	requireNativeConfiguration(t, nativeJSONAt(t, detail, "configuration"), secondCorpus)
	certifyProfileNetworkProvider(t, h, owner, providerID)
	detail = nativeWant(t, h, owner, "GET", path, nil, nil, 200)
	secondRevision := string(nativeJSONAt(t, detail, "active_revision"))
	restoreHeaders := nativeETag(t, detail)
	restoreHeaders["Idempotency-Key"] = "restore-native-corpus"
	restorePath := path + "/revisions/" + firstRevision + "/restore-as-draft"
	restored := nativeWant(t, h, owner, "POST", restorePath, nil, restoreHeaders, 200)
	requireNativeConfiguration(t, nativeJSONAt(t, restored, "provider", "configuration"), nativeConfigurationCorpus)
	replayed := nativeWant(t, h, owner, "POST", restorePath, nil, restoreHeaders, 200)
	if !bytes.Equal(restored, replayed) {
		t.Fatal("encrypted idempotency replay changed the native configuration response")
	}
	revision = nativeWant(t, h, owner, "GET", path+"/revisions/"+secondRevision, nil, nil, 200)
	requireNativeConfiguration(t, nativeJSONAt(t, revision, "configuration"), secondCorpus)
	certifyProfileNetworkProvider(t, h, owner, providerID)

	exported := nativeWant(t, h, owner, "GET", "/api/v3/configuration/export", nil, nil, 200)
	document := nativeJSONAt(t, exported, "document")
	requireNativeConfiguration(t, nativeJSONAt(t, document, "providers", "0", "configuration"), nativeConfigurationCorpus)
	destination := newAccessHarness(t)
	destinationOwner := destination.owner()
	planned := nativeWant(t, destination, destinationOwner, "POST", "/api/v3/configuration/plan", map[string]any{"document": document}, nil, 200)
	binding := nativeJSONString(t, planned, "blockers", "0", "key")
	apply := map[string]any{"document": document, "secret_bindings": map[string]any{binding: vendorSecret}}
	nativeWant(t, destination, destinationOwner, "POST", "/api/v3/configuration/apply", apply, idem("native-promote"), 200)
	providers := nativeWant(t, destination, destinationOwner, "GET", "/api/v3/providers", nil, nil, 200)
	destinationID := nativeJSONString(t, providers, "items", "0", "id")
	destinationPath := "/api/v3/providers/" + destinationID
	promoted := nativeWant(t, destination, destinationOwner, "GET", destinationPath, nil, nil, 200)
	requireNativeConfiguration(t, nativeJSONAt(t, promoted, "configuration"), nativeConfigurationCorpus)
	nativeWant(t, destination, destinationOwner, "POST", "/api/v3/configuration/apply", apply, idem("native-promote-again"), 200)
	repeated := nativeWant(t, destination, destinationOwner, "GET", destinationPath, nil, nil, 200)
	if nativeJSONString(t, repeated, "etag") != nativeJSONString(t, promoted, "etag") {
		t.Fatal("identical native source import was treated as a configuration change")
	}
	exported = nativeWant(t, destination, destinationOwner, "GET", "/api/v3/configuration/export", nil, nil, 200)
	requireNativeConfiguration(t, nativeJSONAt(t, exported, "document", "providers", "0", "configuration"), nativeConfigurationCorpus)

	// Corrupt source without changing its recorded digest. A new reader must
	// refuse the release, never normalize the source or repair its hash.
	var releaseID, recordedDigest string
	var snapshot []byte
	if err := h.Pool.QueryRow(t.Context(), "SELECT id::text,sha256,snapshot FROM olp_go.runtime_releases ORDER BY sequence DESC LIMIT 1").Scan(&releaseID, &recordedDigest, &snapshot); err != nil {
		t.Fatal(err)
	}
	corrupted := bytes.ReplaceAll(snapshot, []byte(`"negative_zero":-0`), []byte(`"negative_zero":0`))
	if bytes.Equal(snapshot, corrupted) {
		t.Fatal("snapshot fixture lacks its exact native negative zero")
	}
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp_go.runtime_releases SET snapshot=$2 WHERE id=$1", releaseID, corrupted); err != nil {
		t.Fatal(err)
	}
	reader := newAccessHarnessOn(t, h.Pool, h.DBURL)
	if err := reader.Runtime.Refresh(t.Context()); err == nil || !strings.Contains(err.Error(), "snapshot digest mismatch") {
		t.Fatalf("corrupted native source did not fail digest verification: %v", err)
	}
	var retainedDigest string
	if err := h.Pool.QueryRow(t.Context(), "SELECT sha256 FROM olp_go.runtime_releases WHERE id=$1", releaseID).Scan(&retainedDigest); err != nil || retainedDigest != recordedDigest {
		t.Fatal("runtime reader rewrote a historical digest after corruption")
	}
}

func TestPublicProviderNativeConfigurationRetainsNegativeZero(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	response, raw := h.do(owner, "POST", "/api/v3/providers", json.RawMessage(`{
		"name":"Native configuration source","model":"fixture-model","credential":"fixture-secret",
		"configuration":{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"http://127.0.0.1:1/v1","profile_id":"compatible-chat","profile_revision":"1",
		"options":{"operation_defaults":{"generation":{"dialect":"openai-chat","native_options":{"fixture_negative_zero":-0}}}}}
	}`), idem("native-source"))
	if response.StatusCode != 201 {
		t.Fatalf("create provider: status %d", response.StatusCode)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := json.Unmarshal(fields["id"], &id); err != nil {
		t.Fatal(err)
	}
	response, raw = h.do(owner, "GET", "/api/v3/providers/"+id, nil, nil)
	if response.StatusCode != 200 {
		t.Fatalf("read provider: status %d", response.StatusCode)
	}
	for _, name := range []string{"configuration", "options", "operation_defaults", "generation", "native_options", "fixture_negative_zero"} {
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		var present bool
		raw, present = fields[name]
		if !present {
			t.Fatalf("public provider response is missing %s", name)
		}
	}
	if string(raw) != "-0" {
		t.Fatalf("public provider response changed native -0 into %s", raw)
	}
}
