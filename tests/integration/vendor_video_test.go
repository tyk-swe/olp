//go:build integration

package integration_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestRunwayVideoLifecycle creates, polls, reads and deletes a Runway video
// through the OpenAI video API: the task is polled until it succeeds, its
// output fetched from the CDN without the credential, and deleting the
// video deletes the task.
func TestRunwayVideoLifecycle(t *testing.T) {
	const task = "6f9b2c1d-1234-4abc-9def-0123456789ab"
	var polls, deletes atomic.Int32
	var created map[string]any
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn/video.mp4" {
			if r.Header.Get("Authorization") != "" {
				t.Error("the CDN received the credential")
			}
			w.Header().Set("Content-Type", "video/mp4")
			w.Write([]byte("mp4-bytes"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+vendorSecret || r.Header.Get("X-Runway-Version") != "2024-11-06" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/organization":
			writeJSON(w, map[string]any{"creditBalance": 100})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/text_to_video":
			created = decodeBody(t, r)
			writeJSON(w, map[string]any{"id": task, "estimatedCost": 50})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/tasks/"+task:
			if polls.Add(1) == 1 {
				writeJSON(w, map[string]any{"id": task, "status": "RUNNING", "progress": 0.5, "createdAt": "2026-10-05T12:00:00Z"})
				return
			}
			writeJSON(w, map[string]any{"id": task, "status": "SUCCEEDED", "createdAt": "2026-10-05T12:00:00Z", "output": []string{server.URL + "/cdn/video.mp4"}})
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/tasks/"+task:
			deletes.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	h := newAccessHarness(t)
	unary := func(operation string) map[string]any {
		return map[string]any{"operation": operation, "surface": "openai", "mode": "unary"}
	}
	slug, secret := provisionRoute(t, h, map[string]any{"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": server.URL + "/v1", "options": map[string]any{"vendor_id": "runway"}},
		vendorSecret, "gen4.5", []any{map[string]any{"operation": "video_create", "surface": "openai", "mode": "async"}, unary("video_get"), unary("video_content"), unary("video_delete")},
		[]string{"video_create", "video_get", "video_content", "video_delete"})
	status, video := h.videoCreate(slug, secret)
	if status != http.StatusCreated || video["status"] != "queued" || created["promptText"] != "a two second clip of rain" || created["model"] != "gen4.5" {
		t.Fatalf("video create: %d %v; Runway received %v", status, video, created)
	}
	id := video["id"].(string)
	if id == task {
		t.Fatal("the client sees the upstream task ID")
	}
	status, video, _ = h.gateway("GET", "/v1/videos/"+id, secret, nil)
	if status != http.StatusOK || video["status"] != "in_progress" || video["progress"] != float64(50) {
		t.Fatalf("running video: %d %v", status, video)
	}
	status, video, _ = h.gateway("GET", "/v1/videos/"+id, secret, nil)
	if status != http.StatusOK || video["status"] != "completed" {
		t.Fatalf("completed video: %d %v", status, video)
	}
	status, content, headers := h.gatewayRaw("GET", "/v1/videos/"+id+"/content", secret, nil, nil)
	if status != http.StatusOK || string(content) != "mp4-bytes" || !strings.HasPrefix(headers.Get("Content-Type"), "video/mp4") {
		t.Fatalf("video content: %d %q %v", status, content, headers)
	}
	status, deleted, _ := h.gateway("DELETE", "/v1/videos/"+id, secret, nil)
	if status != http.StatusOK || deleted["deleted"] != true || deletes.Load() != 1 {
		t.Fatalf("video delete: %d %v after %d deletions", status, deleted, deletes.Load())
	}
	if created["duration"] != float64(4) {
		t.Fatalf("Runway was not sent OpenAI's default duration: %v", created)
	}
}
