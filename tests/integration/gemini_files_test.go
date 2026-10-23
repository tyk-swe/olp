//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/batches"
)

type geminiFileFixture struct {
	server          *httptest.Server
	mu              sync.Mutex
	uploadBytes     []byte
	offset          int64
	expected        int64
	queries         int
	sessionCalls    int
	batchCalls      int
	batches         map[string]map[string]any
	rejectChunkOnce bool
	deletedFiles    map[string]bool
}

func newGeminiFileFixture(t *testing.T) *geminiFileFixture {
	t.Helper()
	fixture := &geminiFileFixture{batches: map[string]map[string]any{}, deletedFiles: map[string]bool{}}
	fileCreatedAt := time.Now().UTC()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1beta/files", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("GET /v1beta/models", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"models": []any{map[string]any{"name": "models/gemini-files-test"}}})
	})
	mux.HandleFunc("POST /v1beta/models/gemini-files-test:generateContent", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"candidates": []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{map[string]string{"text": "ok"}}}, "finishReason": "STOP"}}})
	})
	mux.HandleFunc("POST /upload/v1beta/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Goog-Upload-Protocol") != "resumable" || r.Header.Get("X-Goog-Upload-Command") != "start" ||
			r.Header.Get("X-Goog-Upload-Header-Content-Length") == "" || r.Header.Get("X-Goog-Upload-Header-Content-Type") == "" {
			http.Error(w, "invalid upload start", http.StatusBadRequest)
			return
		}
		expected, _ := strconv.ParseInt(r.Header.Get("X-Goog-Upload-Header-Content-Length"), 10, 64)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		fixture.mu.Lock()
		fixture.uploadBytes = nil
		fixture.offset = 0
		fixture.expected = expected
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
		if fixture.rejectChunkOnce {
			fixture.rejectChunkOnce = false
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			writeJSON(w, map[string]any{"error": map[string]any{"code": 429, "status": "RESOURCE_EXHAUSTED", "message": "retry chunk"}})
			return
		}
		chunk, _ := io.ReadAll(r.Body)
		fixture.uploadBytes = append(fixture.uploadBytes, chunk...)
		fixture.offset += int64(len(chunk))
		if fixture.offset > fixture.expected {
			http.Error(w, "too many bytes", http.StatusBadRequest)
			return
		}
		if command == "upload, finalize" && fixture.offset != fixture.expected {
			http.Error(w, "incomplete upload", http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Goog-Upload-Size-Received", strconv.FormatInt(fixture.offset, 10))
		if command == "upload, finalize" {
			w.Header().Set("X-Goog-Upload-Status", "final")
			writeJSON(w, map[string]any{"file": geminiFileObject(fileCreatedAt)})
			return
		}
		w.Header().Set("X-Goog-Upload-Status", "active")
	})
	mux.HandleFunc("GET /v1beta/files/{name}", func(w http.ResponseWriter, r *http.Request) {
		if _, action, ok := strings.Cut(r.PathValue("name"), ":"); ok && action == "download" {
			w.Header().Set("Content-Type", "text/plain")
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			_, _ = w.Write(fixture.uploadBytes)
			return
		}
		writeJSON(w, geminiFileObject(fileCreatedAt))
	})
	mux.HandleFunc("GET /download/v1beta/files/upstream-gemini-file:download", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		_, _ = w.Write(fixture.uploadBytes)
	})
	mux.HandleFunc("DELETE /v1beta/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		fixture.deletedFiles[r.PathValue("id")] = true
		fixture.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1beta/models/gemini-files-test:batchGenerateContent", func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid batch request", http.StatusBadRequest)
			return
		}
		batch, _ := request["batch"].(map[string]any)
		input, _ := batch["inputConfig"].(map[string]any)
		if input == nil {
			input, _ = batch["input_config"].(map[string]any)
		}
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		fixture.batchCalls++
		id := "batch-" + strconv.Itoa(fixture.batchCalls)
		metadata := map[string]any{
			"state": "BATCH_STATE_SUCCEEDED", "model": "models/gemini-files-test",
			"createTime": time.Now().UTC().Format(time.RFC3339), "inputConfig": input,
			"batchStats": map[string]any{"requestCount": "2"},
		}
		if input["fileName"] != nil || input["file_name"] != nil {
			fixture.deletedFiles["gemini-output-file"] = false // A new job creates fresh fixture evidence.
			metadata["output"] = map[string]any{"responsesFile": "files/gemini-output-file"}
		} else {
			metadata["output"] = map[string]any{"inlinedResponses": map[string]any{"inlinedResponses": []any{
				map[string]any{
					"metadata": map[string]string{"key": "inline-success"},
					"response": map[string]any{
						"candidates":    []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{map[string]string{"text": "ok"}}}, "finishReason": "STOP"}},
						"usageMetadata": map[string]any{"promptTokenCount": 1, "candidatesTokenCount": 1, "totalTokenCount": 2},
					},
				},
				map[string]any{"metadata": map[string]string{"key": "inline-error"}, "error": map[string]any{"code": 400, "message": "bad request"}},
			}}}
		}
		operation := map[string]any{"name": "batches/" + id, "metadata": metadata}
		fixture.batches[id] = operation
		writeJSON(w, operation)
	})
	mux.HandleFunc("GET /v1beta/batches/{id}", func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		operation := fixture.batches[r.PathValue("id")]
		if operation == nil {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, operation)
	})
	mux.HandleFunc("GET /v1beta/batches", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		operations := make([]any, 0, len(fixture.batches))
		for _, operation := range fixture.batches {
			operations = append(operations, operation)
		}
		writeJSON(w, map[string]any{"operations": operations})
	})
	mux.HandleFunc("POST /v1beta/batches/{action}", func(w http.ResponseWriter, r *http.Request) {
		id, action, ok := strings.Cut(r.PathValue("action"), ":")
		if !ok || action != "cancel" {
			http.NotFound(w, r)
			return
		}
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		if operation := fixture.batches[id]; operation != nil {
			writeJSON(w, map[string]any{})
			return
		}
		http.NotFound(w, r)
	})
	mux.HandleFunc("DELETE /v1beta/batches/{id}", func(w http.ResponseWriter, r *http.Request) {
		fixture.mu.Lock()
		delete(fixture.batches, r.PathValue("id"))
		fixture.mu.Unlock()
		writeJSON(w, map[string]any{})
	})
	mux.HandleFunc("GET /download/v1beta/files/gemini-output-file:download", func(w http.ResponseWriter, _ *http.Request) {
		fixture.mu.Lock()
		deleted := fixture.deletedFiles["gemini-output-file"]
		fixture.mu.Unlock()
		if deleted {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/jsonl")
		_, _ = io.WriteString(w, `{"key":"file-success","response":{"candidates":[],"usageMetadata":{"promptTokenCount":2,"candidatesTokenCount":1,"totalTokenCount":3}}}`+"\n"+
			`{"key":"file-error","error":{"code":400,"message":"bad request"}}`+"\n")
	})
	fixture.server = httptest.NewTLSServer(mux)
	t.Cleanup(fixture.server.Close)
	return fixture
}

func geminiFileObject(now time.Time) map[string]any {
	return map[string]any{
		"name": "files/upstream-gemini-file", "displayName": "sample.txt", "mimeType": "text/plain",
		"sizeBytes": "10", "createTime": now.Format(time.RFC3339Nano), "updateTime": now.Format(time.RFC3339Nano),
		"expirationTime": now.Add(48 * time.Hour).Format(time.RFC3339Nano), "sha256Hash": "aGVsbG93b3JsZA==",
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
	detail := h.want(owner, http.MethodGet, providerPath, nil, nil, http.StatusOK)
	updated := h.want(owner, http.MethodPatch, providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{
		map[string]any{"operation": "generation", "surface": "gemini", "mode": "unary"},
		map[string]any{"operation": "batch", "surface": "gemini", "mode": "unary"},
	}}, etagHeader(detail), http.StatusOK)
	if certified := h.want(owner, http.MethodPost, providerPath+"/models/"+modelID+"/certify", nil, etagHeader(updated), http.StatusOK); certified["status"] != "certified" {
		t.Fatalf("Gemini batch capability: %v", certified)
	}
	provider = h.want(owner, http.MethodGet, providerPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, providerPath+"/activate", nil, withMatch(provider, idem(uuid.NewString())), http.StatusOK)
	slug := "gemini-files-" + uuid.NewString()[:8]
	activateRoute := func(name, mode string) {
		draft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
			"slug": name, "operations": []string{"generation", "batch"}, "overall_timeout_ms": 1000, "max_attempts": 1,
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
		"name": "Gemini files", "scopes": []string{"inference"}, "allow_provider_state": true, "allowed_routes": []string{slug, strictSlug},
	}, idem(uuid.NewString()), http.StatusCreated)["secret"].(string)
	otherKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{
		"name": "Foreign Gemini session", "scopes": []string{"inference"}, "allow_provider_state": true, "allowed_routes": []string{slug},
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
	connection, dialErr := net.DialTimeout("tcp", h.HTTP.Listener.Addr().String(), time.Second)
	if dialErr != nil {
		t.Fatal(dialErr)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(connection, "POST %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nX-Goog-Upload-Command: upload\r\nX-Goog-Upload-Offset: 0\r\nContent-Length: 10\r\nConnection: close\r\n\r\nx", sessionPath, h.HTTP.Listener.Addr().String(), key); err != nil {
		t.Fatal(err)
	}
	stalled, readErr := http.ReadResponse(bufio.NewReader(connection), &http.Request{Method: http.MethodPost})
	if readErr != nil {
		t.Fatalf("stalled chunk outlived its route deadline: %v", readErr)
	}
	_ = stalled.Body.Close()
	_ = connection.Close()
	if stalled.StatusCode != http.StatusBadRequest && stalled.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("stalled chunk status=%d", stalled.StatusCode)
	}
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
	fixture.mu.Lock()
	fixture.rejectChunkOnce = true
	fixture.mu.Unlock()
	status, _, _ = h.gatewayRaw(http.MethodPost, sessionPath, key, strings.NewReader("hello"), map[string]string{"Content-Length": "5", "X-Goog-Upload-Command": "upload", "X-Goog-Upload-Offset": "0"})
	if status != http.StatusTooManyRequests {
		t.Fatalf("Gemini scripted chunk rejection status=%d", status)
	}
	status, _, headers = h.gatewayRaw(http.MethodPost, sessionPath, key, nil, map[string]string{"X-Goog-Upload-Command": "query"})
	if status != http.StatusOK || headers.Get("X-Goog-Upload-Size-Received") != "0" {
		t.Fatalf("proven-unaccepted chunk invalidated its upload session: %d %v", status, headers)
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
		"Content-Length": "5", "X-Goog-Upload-Command": "upload", "X-Goog-Upload-Offset": "5",
	})
	if status != http.StatusOK {
		t.Fatalf("Gemini last chunk status=%d body=%s", status, raw)
	}
	status, raw, _ = h.gatewayRaw(http.MethodPost, sessionPath, key, nil, map[string]string{
		"Content-Length": "0", "X-Goog-Upload-Command": "upload, finalize", "X-Goog-Upload-Offset": "10",
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
	unqualified, _ := json.Marshal(map[string]any{"batch": map[string]any{"inputConfig": map[string]any{"fileName": localName}}})
	blockedStatus, blockedBody, _ := h.gatewayRaw(http.MethodPost, "/gemini/v1beta/models/"+slug+":batchGenerateContent", key, bytes.NewReader(unqualified), map[string]string{"Content-Type": "application/json"})
	if blockedStatus != http.StatusUnprocessableEntity {
		t.Fatalf("unqualified native upload entered batch execution: %d %s", blockedStatus, blockedBody)
	}

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

func TestGeminiNativeBatchSettlementAndFiles(t *testing.T) {
	fixture := newGeminiFileFixture(t)
	h := newAccessHarness(t)
	owner := h.owner()
	trustRoot := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.server.TLS.Certificates[0].Certificate[0]})
	provider := h.want(owner, http.MethodPost, "/api/v1/providers", map[string]any{
		"name": "Gemini batches", "configuration": map[string]any{
			"kind": "gemini", "auth_mode": "api_key", "endpoint": fixture.server.URL + "/v1beta",
			"profile_id": "gemini-generation", "profile_revision": "1",
			"options": map[string]any{"network": map[string]any{"trust_roots_pem": string(trustRoot)}},
		}, "model": "gemini-files-test", "credential": "google-batch-fixture-key",
	}, idem(uuid.NewString()), http.StatusCreated)
	providerPath := "/api/v1/providers/" + provider["id"].(string)
	if probe := h.want(owner, http.MethodPost, providerPath+"/probe", nil, etagHeader(provider), http.StatusOK); probe["succeeded"] != true {
		t.Fatalf("Gemini provider probe: %v", probe)
	}
	models := h.want(owner, http.MethodGet, providerPath+"/models", nil, nil, http.StatusOK)
	modelID := models["items"].([]any)[0].(map[string]any)["id"].(string)
	detail := h.want(owner, http.MethodGet, providerPath, nil, nil, http.StatusOK)
	updated := h.want(owner, http.MethodPatch, providerPath+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{
		map[string]any{"operation": "generation", "surface": "gemini", "mode": "unary"},
		map[string]any{"operation": "batch", "surface": "gemini", "mode": "unary"},
	}}, etagHeader(detail), http.StatusOK)
	if certified := h.want(owner, http.MethodPost, providerPath+"/models/"+modelID+"/certify", nil, etagHeader(updated), http.StatusOK); certified["status"] != "certified" {
		t.Fatalf("Gemini batch capability: %v", certified)
	}
	provider = h.want(owner, http.MethodGet, providerPath, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, providerPath+"/activate", nil, withMatch(provider, idem(uuid.NewString())), http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/pricing/revisions", map[string]any{
		"effective_at": time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		"prices": []any{map[string]any{
			"provider_kind": "gemini", "provider_id": provider["id"], "model": "gemini-files-test",
			"operation": "generation", "currency": "USD", "input_per_million": "1000000",
			"output_per_million": "1000000", "batch_multiplier": "1",
		}},
	}, idem(uuid.NewString()), http.StatusCreated)
	slug := "gemini-batch-" + uuid.NewString()[:8]
	activate := func(routeSlug, fidelity string) {
		draft := h.want(owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
			"slug": routeSlug, "operations": []string{"generation", "batch"}, "overall_timeout_ms": 10000, "max_attempts": 1,
			"fidelity": map[string]any{"mode": fidelity},
			"targets":  []any{map[string]any{"provider_id": provider["id"], "provider_model": "gemini-files-test", "priority": 0, "weight": 1, "timeout_ms": 5000}},
		}, idem(uuid.NewString()), http.StatusCreated)
		h.want(owner, http.MethodPost, "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), http.StatusOK)
	}
	activate(slug, "transformed")
	strictSlug := slug + "-strict"
	activate(strictSlug, "strict")
	key := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Gemini batch", "scopes": []string{"inference"}, "allow_provider_state": true, "allowed_routes": []string{slug, strictSlug}}, idem(uuid.NewString()), http.StatusCreated)["secret"].(string)
	foreignKey := h.want(owner, http.MethodPost, "/api/v1/api-keys", map[string]any{"name": "Foreign Gemini file", "scopes": []string{"inference"}, "allow_provider_state": true, "allowed_routes": []string{slug}}, idem(uuid.NewString()), http.StatusCreated)["secret"].(string)
	h.refresh()

	fileStatus, file := uploadGeminiManagedOpenAIFile(t, h, key, slug)
	if fileStatus != http.StatusOK {
		t.Fatalf("Gemini input file upload: %d %v", fileStatus, file)
	}
	inlineRequest := []byte(`{"batch":{"inputConfig":{"requests":{"requests":[{"request":{"contents":[{"parts":[{"text":"hello"}]}]},"metadata":{"key":"inline-success"}},{"request":{"contents":[{"parts":[{"text":"bad"}]}]},"metadata":{"key":"inline-error"}}]}}}}`)
	strictStatus, _, _ := h.gatewayRaw(http.MethodPost, "/gemini/v1beta/models/"+strictSlug+":batchGenerateContent", key, bytes.NewReader(inlineRequest), map[string]string{"Content-Type": "application/json"})
	if strictStatus != http.StatusUnprocessableEntity {
		t.Fatalf("strict Gemini batch refusal status=%d", strictStatus)
	}
	fixture.mu.Lock()
	beforeForeign := fixture.batchCalls
	fixture.mu.Unlock()
	fileRequest, _ := json.Marshal(map[string]any{
		"input_file_id": file["id"], "endpoint": "/v1/chat/completions", "completion_window": "24h",
	})
	foreignStatus, _, _ := h.gatewayRaw(http.MethodPost, "/v1/batches", foreignKey, bytes.NewReader(fileRequest), map[string]string{"Content-Type": "application/json"})
	fixture.mu.Lock()
	afterForeign := fixture.batchCalls
	fixture.mu.Unlock()
	if foreignStatus != http.StatusNotFound || afterForeign != beforeForeign {
		t.Fatalf("foreign Gemini file batch status=%d dispatches=%d/%d", foreignStatus, beforeForeign, afterForeign)
	}
	status, raw, _ := h.gatewayRaw(http.MethodPost, "/gemini/v1beta/models/"+slug+":batchGenerateContent", key, bytes.NewReader(inlineRequest), map[string]string{"Content-Type": "application/json"})
	if status != http.StatusOK {
		t.Fatalf("Gemini inline batch create: %d %s", status, raw)
	}
	var operation map[string]any
	if json.Unmarshal(raw, &operation) != nil {
		t.Fatalf("invalid Gemini batch operation: %s", raw)
	}
	localName := strings.TrimPrefix(operation["name"].(string), "batches/")
	resourceID := geminiBatchResourceID(t, localName)
	deletePending, deletePendingBody, _ := h.gatewayRaw(http.MethodDelete, "/gemini/v1beta/batches/"+localName, key, nil, nil)
	if deletePending != http.StatusConflict || !bytes.Contains(deletePendingBody, []byte("batch_settlement_pending")) {
		t.Fatalf("inline Gemini evidence was deletable before settlement: %d %s", deletePending, deletePendingBody)
	}

	if pass, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil || pass.Settled != 1 {
		t.Fatalf("Gemini worker settlement: pass=%+v err=%v", pass, err)
	}
	counts, err := batches.NewStore(h.Pool).ResultCounts(t.Context(), resourceID)
	if err != nil || counts[batches.ResultSucceeded] != 1 || counts[batches.ResultErrored] != 1 {
		t.Fatalf("Gemini settlement counts=%v err=%v", counts, err)
	}
	rows, err := h.Pool.Query(t.Context(), `SELECT r.result_kind,count(f.attempt_id),
		count(*) FILTER(WHERE f.charge_status='billable')
		FROM olp.batch_settlement_results r LEFT JOIN olp.attempt_usage_facts f ON f.event_id=r.event_id
		WHERE r.resource_id=$1 GROUP BY r.result_kind`, resourceID)
	if err != nil {
		t.Fatal(err)
	}
	type factCounts struct{ facts, billable int64 }
	charged := map[batches.ResultKind]factCounts{}
	for rows.Next() {
		var kind batches.ResultKind
		var count factCounts
		if err := rows.Scan(&kind, &count.facts, &count.billable); err != nil {
			t.Fatal(err)
		}
		charged[kind] = count
	}
	rows.Close()
	if charged[batches.ResultSucceeded].facts != 1 || charged[batches.ResultSucceeded].billable != 1 ||
		charged[batches.ResultErrored].billable != 0 {
		t.Fatalf("Gemini batch charges=%v", charged)
	}
	fileStatus, raw, _ = h.gatewayRaw(http.MethodPost, "/v1/batches", key, bytes.NewReader(fileRequest), map[string]string{"Content-Type": "application/json"})
	if fileStatus != http.StatusOK {
		t.Fatalf("Gemini file batch create: %d %s", fileStatus, raw)
	}
	var fileOperation map[string]any
	_ = json.Unmarshal(raw, &fileOperation)
	outputID := fileOperation["output_file_id"].(string)
	errorID := fileOperation["error_file_id"].(string)
	downloadStatus, downloaded, _ := h.gatewayRaw(http.MethodGet, "/v1/files/"+outputID+"/content", key, nil, nil)
	if downloadStatus != http.StatusOK || !bytes.Contains(downloaded, []byte(`"custom_id":"file-success"`)) {
		t.Fatalf("Gemini mapped batch output download status=%d body=%s", downloadStatus, downloaded)
	}
	errorStatus, errorOutput, _ := h.gatewayRaw(http.MethodGet, "/v1/files/"+errorID+"/content", key, nil, nil)
	if errorStatus != http.StatusOK || !bytes.Contains(errorOutput, []byte(`"custom_id":"file-error"`)) {
		t.Fatalf("Gemini mapped batch error download status=%d body=%s", errorStatus, errorOutput)
	}
	deleteStatus, deleteBody, _ := h.gatewayRaw(http.MethodDelete, "/v1/files/"+errorID, key, nil, nil)
	if deleteStatus != http.StatusOK {
		t.Fatalf("delete Gemini error projection: %d %s", deleteStatus, deleteBody)
	}
	downloadStatus, downloaded, _ = h.gatewayRaw(http.MethodGet, "/v1/files/"+outputID+"/content", key, nil, nil)
	if downloadStatus != http.StatusOK || !bytes.Contains(downloaded, []byte(`"custom_id":"file-success"`)) {
		t.Fatalf("deleting error projection destroyed successful results: %d %s", downloadStatus, downloaded)
	}
	batchID := fileOperation["id"].(string)
	refreshStatus, refreshed, _ := h.gateway(http.MethodGet, "/v1/batches/"+batchID, key, nil)
	if refreshStatus != http.StatusOK || refreshed["error_file_id"] != errorID {
		t.Fatalf("deleted Gemini projection was resurrected: %d %v", refreshStatus, refreshed)
	}
	deleteStatus, _, _ = h.gatewayRaw(http.MethodDelete, "/v1/files/"+outputID, key, nil, nil)
	if deleteStatus != http.StatusConflict {
		t.Fatalf("last result projection was deleted before settlement: %d", deleteStatus)
	}
	if _, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	deleteStatus, deleteBody, _ = h.gatewayRaw(http.MethodDelete, "/v1/files/"+outputID, key, nil, nil)
	if deleteStatus != http.StatusOK {
		t.Fatalf("settled Gemini output could not be deleted: %d %s", deleteStatus, deleteBody)
	}
	fixture.mu.Lock()
	deleted := fixture.deletedFiles["gemini-output-file"]
	fixture.mu.Unlock()
	if !deleted {
		t.Fatal("last settled projection did not delete the shared provider object")
	}
	// A complete final chunk can be qualified without persisting input bytes.
	nativeInput := []byte(`{"key":"file-success","request":{"contents":[{"parts":[{"text":"hi"}]}]}}` + "\n" + `{"key":"file-error","request":{"contents":[{"parts":[{"text":"bad"}]}]}}` + "\n")
	startStatus, startBody, startHeaders := h.gatewayRaw(http.MethodPost, "/gemini/upload/v1beta/files", key, strings.NewReader(`{}`), map[string]string{
		"Content-Type": "application/json", "X-OLP-Route": slug, "X-Goog-Upload-Protocol": "resumable", "X-Goog-Upload-Command": "start",
		"X-Goog-Upload-Header-Content-Length": strconv.Itoa(len(nativeInput)), "X-Goog-Upload-Header-Content-Type": "application/jsonl",
	})
	if startStatus != http.StatusOK {
		t.Fatalf("native JSONL upload start: %d %s", startStatus, startBody)
	}
	uploadURL, err := url.Parse(startHeaders.Get("X-Goog-Upload-URL"))
	if err != nil {
		t.Fatal(err)
	}
	uploadStatus, uploadBody, _ := h.gatewayRaw(http.MethodPost, uploadURL.RequestURI(), key, bytes.NewReader(nativeInput), map[string]string{"Content-Length": strconv.Itoa(len(nativeInput)), "X-Goog-Upload-Command": "upload, finalize", "X-Goog-Upload-Offset": "0"})
	if uploadStatus != http.StatusOK || bytes.Contains(uploadBody, []byte("batch_manifest")) {
		t.Fatalf("qualified native JSONL upload: %d %s", uploadStatus, uploadBody)
	}
	var uploadedNative map[string]any
	_ = json.Unmarshal(uploadBody, &uploadedNative)
	inputName := uploadedNative["file"].(map[string]any)["name"].(string)
	_, _, _ = h.gatewayRaw(http.MethodGet, "/gemini/v1beta/"+inputName, key, nil, nil) // Refresh must preserve the private qualification.
	nativeRequest, _ := json.Marshal(map[string]any{"batch": map[string]any{"inputConfig": map[string]any{"fileName": inputName}}})
	nativeStatus, nativeBody, _ := h.gatewayRaw(http.MethodPost, "/gemini/v1beta/models/"+slug+":batchGenerateContent", key, bytes.NewReader(nativeRequest), map[string]string{"Content-Type": "application/json"})
	if nativeStatus != http.StatusOK {
		t.Fatalf("qualified file-backed native batch: %d %s", nativeStatus, nativeBody)
	}
	var nativeOperation map[string]any
	_ = json.Unmarshal(nativeBody, &nativeOperation)
	nativeName := nativeOperation["name"].(string)
	nativeOutput := nativeOperation["metadata"].(map[string]any)["output"].(map[string]any)["responsesFile"].(string)
	outputUUID, err := uuid.Parse(strings.TrimPrefix(nativeOutput, "files/gemini_file_"))
	if err != nil {
		t.Fatal(err)
	}
	var savedParent *uuid.UUID
	if err := h.Pool.QueryRow(t.Context(), `SELECT parent_id FROM olp.provider_resources WHERE id=$1`, outputUUID).Scan(&savedParent); err != nil || savedParent == nil || *savedParent != geminiBatchResourceID(t, strings.TrimPrefix(nativeName, "batches/")) {
		t.Fatalf("result-file lineage was not persisted: parent=%v err=%v", savedParent, err)
	}
	deletePending, _, _ = h.gatewayRaw(http.MethodDelete, "/gemini/v1beta/"+nativeOutput, key, nil, nil)
	if deletePending != http.StatusConflict {
		t.Fatalf("native result file was deletable before settlement: %d", deletePending)
	}
	if _, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	deletePending, deletePendingBody, _ = h.gatewayRaw(http.MethodDelete, "/gemini/v1beta/"+nativeOutput, key, nil, nil)
	if deletePending != http.StatusOK {
		t.Fatalf("settled native result cleanup: %d %s", deletePending, deletePendingBody)
	}
	projectionStatus, projectionBody, _ := h.gatewayRaw(http.MethodGet, "/gemini/v1beta/"+nativeName, key, nil, nil)
	if projectionStatus != http.StatusOK || !bytes.Contains(projectionBody, []byte(nativeOutput)) {
		t.Fatalf("batch projection lost a deleted result mapping: %d %s", projectionStatus, projectionBody)
	}
	listStatus, listBody, _ := h.gatewayRaw(http.MethodGet, "/gemini/v1beta/batches", key, nil, nil)
	if listStatus != http.StatusOK {
		t.Fatalf("native batch list failed after result cleanup: %d %s", listStatus, listBody)
	}
	deletePending, deletePendingBody, _ = h.gatewayRaw(http.MethodDelete, "/gemini/v1beta/"+nativeName, key, nil, nil)
	if deletePending != http.StatusOK {
		t.Fatalf("settled native batch could not be deleted: %d %s", deletePending, deletePendingBody)
	}

}

func uploadGeminiManagedOpenAIFile(t *testing.T, h *accessHarness, key, route string) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("purpose", "batch"); err != nil {
		t.Fatal(err)
	}
	part, err := form.CreateFormFile("file", "batch.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(part, `{"custom_id":"file-success","method":"POST","url":"/v1/chat/completions","body":{"model":"`+route+`","messages":[{"role":"user","content":"hi"}]}}`+"\n")
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/files", key, &body, map[string]string{
		"Content-Type": form.FormDataContentType(), "X-OLP-Route": route,
	})
	if status != http.StatusOK {
		return status, nil
	}
	var result map[string]any
	_ = json.Unmarshal(raw, &result)
	return status, result
}

func geminiBatchResourceID(t *testing.T, localID string) uuid.UUID {
	t.Helper()
	hexID := strings.TrimPrefix(localID, "gemini_batch_")
	id, err := uuid.Parse(hexID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
