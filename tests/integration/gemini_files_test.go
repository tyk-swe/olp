//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

type geminiFileFixture struct {
	server       *httptest.Server
	mu           sync.Mutex
	uploadBytes  []byte
	offset       int64
	queries      int
	sessionCalls int
}

func newGeminiFileFixture(t *testing.T) *geminiFileFixture {
	t.Helper()
	fixture := &geminiFileFixture{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1beta/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"models": []any{map[string]any{"name": "models/gemini-files-test"}}})
	})
	mux.HandleFunc("POST /v1beta/models/gemini-files-test:generateContent", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{map[string]string{"text": "ok"}}}, "finishReason": "STOP"}}})
	})
	mux.HandleFunc("POST /upload/v1beta/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Upload-Protocol") != "resumable" || r.Header.Get("X-Goog-Upload-Command") != "start" ||
			r.Header.Get("X-Goog-Upload-Header-Content-Length") != "10" || r.Header.Get("X-Goog-Upload-Header-Content-Type") != "text/plain" {
			http.Error(w, "invalid upload start", http.StatusBadRequest)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fixture.mu.Lock()
		fixture.uploadBytes = nil
		fixture.offset = 0
		fixture.mu.Unlock()
		w.Header().Set("X-Goog-Upload-URL", fixture.server.URL+"/upload/session/token")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /upload/session/token", func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		fixture.sessionCalls++
		command := r.Header.Get("X-Goog-Upload-Command")
		if command == "query" {
			fixture.queries++
			w.Header().Set("X-Goog-Upload-Status", "active")
			w.Header().Set("X-Goog-Upload-Size-Received", strconv.FormatInt(fixture.offset, 10))
			return
		}
		offset, err := strconv.ParseInt(r.Header.Get("X-Goog-Upload-Offset"), 10, 64)
		if err != nil || offset != fixture.offset {
			http.Error(w, "invalid offset", http.StatusBadRequest)
			return
		}
		chunk, _ := io.ReadAll(r.Body)
		fixture.uploadBytes = append(fixture.uploadBytes, chunk...)
		fixture.offset += int64(len(chunk))
		w.Header().Set("X-Goog-Upload-Size-Received", strconv.FormatInt(fixture.offset, 10))
		if command == "upload, finalize" {
			w.Header().Set("X-Goog-Upload-Status", "final")
			writeJSON(w, map[string]any{"file": geminiFileObject()})
			return
		}
		w.Header().Set("X-Goog-Upload-Status", "active")
	})
	mux.HandleFunc("GET /v1beta/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, geminiFileObject())
	})
	mux.HandleFunc("DELETE /v1beta/files/{id}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /v1beta/files/{id}:download", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		_, _ = w.Write(fixture.uploadBytes)
	})
	fixture.server = httptest.NewTLSServer(mux)
	t.Cleanup(fixture.server.Close)
	return fixture
}

func geminiFileObject() map[string]any {
	return map[string]any{
		"name": "files/upstream-gemini-file", "displayName": "sample.txt", "mimeType": "text/plain",
		"sizeBytes": "10", "createTime": "2026-10-21T10:00:00Z", "updateTime": "2026-10-21T10:00:00Z",
		"expirationTime": "2026-10-23T10:00:00Z", "sha256Hash": "aGVsbG93b3JsZA==",
		"uri":         "https://generativelanguage.googleapis.com/v1beta/files/upstream-gemini-file",
		"downloadUri": "https://generativelanguage.googleapis.com/v1beta/files/upstream-gemini-file:download?alt=media",
		"state":       "ACTIVE", "source": "UPLOADED",
	}
}

func TestGeminiResumableFileLifecycle(t *testing.T) {
	fixture := newGeminiFileFixture(t)
	h := newAccessHarness(t)
	owner := h.owner()
	trustRoot := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.server.TLS.Certificates[0].Certificate[0]})
	provider := h.want(owner, http.MethodPost, "/api/v1/providers", map[string]any{
		"name": "Gemini Files", "configuration": map[string]any{
			"kind": "gemini", "auth_mode": "api_key", "endpoint": fixture.server.URL + "/v1beta",
			"profile_id": "gemini-generation", "profile_revision": "1",
			"options": map[string]any{"network": map[string]any{"trust_roots_pem": string(trustRoot)}},
		}, "model": "gemini-files-test", "credential": "google-files-fixture-key",
	}, idem(uuid.NewString()), http.StatusCreated)
	providerPath := "/api/v1/providers/" + provider["id"].(string)
	probe := h.want(owner, http.MethodPost, providerPath+"/probe", nil, etagHeader(provider), http.StatusOK)
	if probe["succeeded"] != true {
		t.Fatalf("Gemini provider probe: %v", probe)
	}
	models := h.want(owner, http.MethodGet, providerPath+"/models", nil, nil, http.StatusOK)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	model := h.want(owner, http.MethodGet, providerPath+"/models/"+modelID, nil, nil, http.StatusOK)
	updated := h.want(owner, http.MethodPatch, providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{
		map[string]any{"operation": "generation", "surface": "gemini", "mode": "unary"},
		map[string]any{"operation": "batch", "surface": "gemini", "mode": "unary"},
	}}, etagHeader(model), http.StatusOK)
	if certified := h.want(owner, http.MethodPost, providerPath+"/models/"+modelID+"/certify", nil, etagHeader(updated), http.StatusOK); certified["status"] != "certified" {
		t.Fatalf("Gemini batch capability: %v", certified)
	}
	provider = h.want(owner, http.MethodGet, providerPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, providerPath+"/activate", nil, withMatch(provider, idem(uuid.NewString())), http.StatusOK)
	slug := "gemini-files-" + uuid.NewString()[:8]
	activateRoute := func(name, mode string) {
		draft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
			"slug": name, "operations": []string{"generation", "batch"}, "overall_timeout_ms": 10000, "max_attempts": 1,
			"fidelity": map[string]any{"mode": mode},
			"targets":  []any{map[string]any{"provider_id": provider["id"], "provider_model": "gemini-files-test", "priority": 0, "weight": 1, "timeout_ms": 5000}},
		}, idem(uuid.NewString()), http.StatusCreated)
		h.want(owner, http.MethodPost, "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil,
			withMatch(draft, idem(uuid.NewString())), http.StatusOK)
	}
	strictSlug := slug + "-strict"
	activateRoute(slug, "transformed")
	activateRoute(strictSlug, "strict")
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "Gemini files", "scopes": []string{"inference"}, "allowed_routes": []string{slug, strictSlug},
	}, idem(uuid.NewString()), http.StatusCreated)["secret"].(string)
	otherKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "Foreign Gemini session", "scopes": []string{"inference"}, "allowed_routes": []string{slug},
	}, idem(uuid.NewString()), http.StatusCreated)["secret"].(string)
	h.refresh()

	status, raw, headers := h.gatewayRaw(http.MethodPost, "/gemini/upload/v1beta/files", key,
		strings.NewReader(`{"file":{"displayName":"sample.txt"}}`), map[string]string{
			"Content-Type": "application/json", "X-OLP-Route": slug,
			"X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start",
			"X-Goog-Upload-Header-Content-Length": "10", "X-Goog-Upload-Header-Content-Type": "text/plain",
		})
	if status != http.StatusOK || len(raw) != 0 || headers.Get("X-Goog-Upload-Status") != "active" {
		t.Fatalf("Gemini resumable start: status=%d headers=%v body=%s", status, headers, raw)
	}
	sessionURL, parseErr := url.Parse(headers.Get("X-Goog-Upload-URL"))
	if parseErr != nil || sessionURL.Path == "" {
		t.Fatalf("Gemini upload proxy URL=%q", headers.Get("X-Goog-Upload-URL"))
	}
	sessionPath := sessionURL.Path
	fixture.mu.Lock()
	sessionCalls := fixture.sessionCalls
	fixture.mu.Unlock()
	status, _, _ = h.gatewayRaw(http.MethodPost, sessionPath, otherKey, strings.NewReader("hello"), map[string]string{
		"Content-Length": "5", "X-Goog-Upload-Command": "upload", "X-Goog-Upload-Offset": "0",
	})
	fixture.mu.Lock()
	afterForeignSession := fixture.sessionCalls
	fixture.mu.Unlock()
	if status != http.StatusNotFound || afterForeignSession != sessionCalls {
		t.Fatalf("cross-key Gemini session status=%d dispatched=%d/%d, want 404 and no dispatch", status, sessionCalls, afterForeignSession)
	}
	beforeOffsetMismatch := afterForeignSession
	status, _, _ = h.gatewayRaw(http.MethodPost, sessionPath, key, strings.NewReader("hello"), map[string]string{
		"Content-Length": "5", "X-Goog-Upload-Command": "upload", "X-Goog-Upload-Offset": "1",
	})
	fixture.mu.Lock()
	afterOffsetMismatch := fixture.sessionCalls
	fixture.mu.Unlock()
	if status != http.StatusBadRequest || afterOffsetMismatch != beforeOffsetMismatch {
		t.Fatalf("Gemini offset mismatch status=%d dispatched=%d/%d, want 400 without dispatch", status, beforeOffsetMismatch, afterOffsetMismatch)
	}
	status, _, _ = h.gatewayRaw(http.MethodPost, sessionPath, key, strings.NewReader("hello"), map[string]string{
		"Content-Length": "5", "X-Goog-Upload-Command": "upload", "X-Goog-Upload-Offset": "0",
	})
	if status != http.StatusOK {
		t.Fatalf("Gemini first chunk status=%d", status)
	}
	queryStatus, _, queryHeaders := h.gatewayRaw(http.MethodPost, sessionPath, key, nil, map[string]string{"X-Goog-Upload-Command": "query"})
	if queryStatus != http.StatusOK || queryHeaders.Get("X-Goog-Upload-Size-Received") != "5" || queryHeaders.Get("X-Goog-Upload-Status") != "active" {
		t.Fatalf("Gemini upload query status=%d headers=%v", queryStatus, queryHeaders)
	}
	status, raw, _ = h.gatewayRaw(http.MethodPost, sessionPath, key, strings.NewReader("world"), map[string]string{
		"Content-Length": "5", "X-Goog-Upload-Command": "upload, finalize", "X-Goog-Upload-Offset": "5",
	})
	if status != http.StatusOK {
		t.Fatalf("Gemini finalize status=%d body=%s", status, raw)
	}
	var final map[string]any
	if json.Unmarshal(raw, &final) != nil {
		t.Fatalf("invalid Gemini finalize response: %s", raw)
	}
	file := final["file"].(map[string]any)
	localName := file["name"].(string)
	localID := strings.TrimPrefix(localName, "files/")
	getStatus, getBody, _ := h.gatewayRaw(http.MethodGet, "/gemini/v1beta/files/"+localID, key, nil, nil)
	if getStatus != http.StatusOK || !bytes.Contains(getBody, []byte(`"name":"files/`+localID+`"`)) {
		t.Fatalf("Gemini file get status=%d body=%s", getStatus, getBody)
	}
	downloadStatus, download, _ := h.gatewayRaw(http.MethodGet, "/gemini/download/v1beta/files/"+localID+":download?alt=media", key, nil, nil)
	if downloadStatus != http.StatusOK || string(download) != "helloworld" {
		t.Fatalf("Gemini file download status=%d body=%q", downloadStatus, download)
	}
	deleteStatus, deleteBody, _ := h.gatewayRaw(http.MethodDelete, "/gemini/v1beta/files/"+localID, key, nil, nil)
	if deleteStatus != http.StatusOK || string(deleteBody) != "{}" {
		t.Fatalf("Gemini file delete status=%d body=%s", deleteStatus, deleteBody)
	}
	strictStatus, _, _ := h.gatewayRaw(http.MethodPost, "/gemini/upload/v1beta/files", key, strings.NewReader(""),
		map[string]string{"X-OLP-Route": strictSlug, "X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start",
			"X-Goog-Upload-Header-Content-Length": "1", "X-Goog-Upload-Header-Content-Type": "text/plain"})
	if strictStatus != http.StatusUnprocessableEntity {
		t.Fatalf("strict Gemini file refusal status=%d", strictStatus)
	}
}
