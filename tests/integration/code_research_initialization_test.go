//go:build integration

package integration_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/gateway"
)

func TestCodeResearchInitializedNotificationCompletesOpenSSEHandshake(t *testing.T) {
	f := researchFixture(t)
	_, _, limiter := limSetup(t, "research-initialized")
	f.h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET policy=policy||'{"max_concurrency":1,"route_limits":{"qualification":{"max_concurrency":1}}}' WHERE id=$1`, f.keyID); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/providers/" + f.providers[0]
	provider := f.h.want(f.owner, "GET", path, nil, nil, 200)
	configuration := provider["configuration"].(map[string]any)
	configuration["options"].(map[string]any)["limits"] = map[string]any{"max_concurrency": 1}
	f.h.want(f.owner, "PATCH", path, map[string]any{"name": provider["name"], "configuration": configuration}, etagHeader(provider), 200)
	f.publishResearch(t, []string{"search"})
	initialized := make(chan struct{})
	f.peer.mu.Lock()
	f.peer.respond = func(w http.ResponseWriter, r *http.Request) {
		var message struct{ Method string }
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Error(err)
			return
		}
		if message.Method == "notifications/initialized" {
			close(initialized)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "native-session")
		fmt.Fprintf(w, "data: %s\n\n", researchInitResult)
		w.(http.Flusher).Flush()
		select {
		case <-initialized:
		case <-r.Context().Done():
		}
	}
	f.peer.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "POST", f.gateway.URL+"/code/qualification/mcp/search", strings.NewReader(researchInit))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.key)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "protocolVersion") {
		t.Fatalf("SSE initialization result: %s %v", line, err)
	}
	f.research(t, "POST", "search", f.key, response.Header.Get("Mcp-Session-Id"), `{"jsonrpc":"2.0","method":"notifications/initialized"}`, 202)
	if _, err := io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	researchSessionCount(t, f, 1)
}
