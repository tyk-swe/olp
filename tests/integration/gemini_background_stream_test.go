//go:build integration

package integration_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGeminiBackgroundStreamResumesSameOwnedWorkAfterReaderLoss(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", provider)
	path := "/gemini/v1beta/interactions"
	countPosts := func() int {
		count := 0
		for _, call := range provider.captured() {
			if call.Method == http.MethodPost && call.Path == "/v1beta/interactions" {
				count++
			}
		}
		return count
	}
	before := countPosts()
	body := []byte(fmt.Sprintf(`{"model":%q,"input":"durable stream","background":true,"store":true}`, slug))
	response, raw := geminiPublic(t, h, http.MethodPost, path, key, body)
	var accepted struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(raw, &accepted) != nil || !strings.HasPrefix(accepted.ID, "interaction_") || accepted.Status != "in_progress" || countPosts() != before+1 {
		t.Fatalf("background work was not accepted once: %d %s", response.StatusCode, raw)
	}

	// A fresh gateway must resolve the encrypted owner mapping before opening
	// the accepted work's stream. The first reader closes after one cursor;
	// another gateway resumes the same provider resource, without a new POST.
	restarted := newAccessHarnessOn(t, h.Pool, h.DBURL)
	restarted.refresh()
	client := &http.Client{Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, restarted.HTTP.URL+path+"/"+accepted.ID+"?stream=true", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Goog-Api-Key", key)
	stream, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if stream.StatusCode != http.StatusOK || !strings.HasPrefix(stream.Header.Get("Content-Type"), "text/event-stream") {
		stream.Body.Close()
		t.Fatalf("restarted gateway did not stream accepted work: %d", stream.StatusCode)
	}
	first, err := bufio.NewReader(stream.Body).ReadString('\n')
	stream.Body.Close()
	if err != nil || first != "id: cursor-created\n" {
		t.Fatalf("first durable cursor changed: %q %v", first, err)
	}

	resumed := newAccessHarnessOn(t, h.Pool, h.DBURL)
	resumed.refresh()
	response, raw = geminiPublic(t, resumed, http.MethodGet, path+"/"+accepted.ID+"?stream=true&last_event_id="+url.QueryEscape("cursor-start"), key, nil)
	if response.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte("id: cursor-delta")) || !bytes.Contains(raw, []byte("id: cursor-complete")) || !bytes.Contains(raw, []byte(`"id":"`+accepted.ID+`"`)) || bytes.Contains(raw, []byte("id: cursor-created")) || bytes.Contains(raw, []byte("v1_fixture_")) {
		t.Fatalf("durable stream resume changed cursor or leaked native identity: %d %s", response.StatusCode, raw)
	}
	if countPosts() != before+1 {
		t.Fatal("reader loss or cursor resume created another provider interaction")
	}
	gets := 0
	var upstreamPath string
	for _, call := range provider.captured() {
		if call.Method != http.MethodGet || !strings.HasPrefix(call.Path, "/v1beta/interactions/") {
			continue
		}
		gets++
		if strings.Contains(call.Path, accepted.ID) || !strings.Contains(call.Query, "stream=true") {
			t.Fatalf("provider received a local ID or lost streaming control: %+v", call)
		}
		if upstreamPath == "" {
			upstreamPath = call.Path
		} else if upstreamPath != call.Path {
			t.Fatalf("resumed a different provider resource: %q and %q", upstreamPath, call.Path)
		}
		if gets == 2 && !strings.Contains(call.Query, "last_event_id=cursor-start") {
			t.Fatalf("provider did not receive the resume cursor: %+v", call)
		}
	}
	if gets != 2 {
		t.Fatalf("expected exactly two provider reads of one accepted work, got %d", gets)
	}
}
