//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const researchInitResult = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","serverInfo":{"name":"fixture","version":"1"},"capabilities":{"tools":{}}}}`

func researchSessionCount(t *testing.T, f *codingPlanFixture, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		var count int
		if err := f.h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.code_research_sessions WHERE api_key_id=$1`, f.keyID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("retained %d research sessions; want %d", count, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCodeResearchFailedInitializationReclaimsSessions(t *testing.T) {
	f := researchFixture(t)
	f.peer.mu.Lock()
	success := f.peer.respond
	f.peer.mu.Unlock()
	for _, test := range []struct {
		name, media, body string
		status            int
		interrupt         bool
	}{
		{"RPC error", "application/json", `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"temporarily unavailable"}}`, 200, false},
		{"malformed", "application/json", `{"jsonrpc":`, 502, false},
		{"wrong ID", "application/json", strings.Replace(researchInitResult, `"id":1`, `"id":2`, 1), 200, false},
		{"incomplete SSE", "text/event-stream", ": heartbeat\n\ndata: " + researchInitResult, 200, false},
		{"interrupted JSON", "application/json", `{"jsonrpc":`, 502, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f.peer.mu.Lock()
			f.peer.respond = func(w http.ResponseWriter, r *http.Request) {
				if test.interrupt {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nMcp-Session-Id: native-session\r\nContent-Length: 4096\r\n\r\n%s", test.body)
					conn.Close()
					return
				}
				w.Header().Set("Content-Type", test.media)
				w.Header().Set("Mcp-Session-Id", "native-session")
				fmt.Fprint(w, test.body)
			}
			f.peer.mu.Unlock()
			attempts := 1
			if test.name == "RPC error" {
				attempts = 33 // Failed handshakes must not exhaust a key's 32 slots.
			}
			for range attempts {
				f.research(t, "POST", "search", f.key, "", researchInit, test.status)
				researchSessionCount(t, f, 0)
			}
		})
	}
	f.peer.mu.Lock()
	f.peer.respond = success
	f.peer.mu.Unlock()
	response, _ := f.research(t, "POST", "search", f.key, "", researchInit, 200)
	researchSessionCount(t, f, 1)
	f.research(t, "POST", "search", f.key, response.Header.Get("Mcp-Session-Id"), researchList, 200)
}

func TestCodeResearchServerPingsAndLiveRevocation(t *testing.T) {
	f := researchFixture(t)
	replies := make(chan string, 1)
	var answered atomic.Int32
	answer := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method != http.MethodPost {
			return false
		}
		var message struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
		}
		raw, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		if json.Unmarshal(raw, &message) != nil || message.Method != "" || message.ID == "" {
			return false
		}
		if string(message.Result) != "{}" || r.Header.Get("Authorization") != "Bearer research-vendor-key" || r.Header.Get("Mcp-Session-Id") != "native-session" {
			t.Errorf("invalid ping reply: %s %v", raw, r.Header)
		}
		answered.Add(1)
		replies <- message.ID
		w.WriteHeader(http.StatusAccepted)
		return true
	}
	f.peer.mu.Lock()
	f.peer.respond = func(w http.ResponseWriter, r *http.Request) {
		if answer(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Mcp-Session-Id", "native-session")
		fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"init-ping\",\"method\":\"ping\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case id := <-replies:
			if id != "init-ping" {
				t.Errorf("unexpected ping response ID: %q", id)
			}
			fmt.Fprintf(w, "data: %s\n\n", researchInitResult)
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			t.Error("initialization ping was not answered")
		}
	}
	f.peer.mu.Unlock()
	response, raw := f.research(t, "POST", "search", f.key, "", researchInit, 200)
	session := response.Header.Get("Mcp-Session-Id")
	if answered.Load() != 1 || strings.Contains(raw, "init-ping") || !strings.Contains(raw, "protocolVersion") {
		t.Fatalf("initialization ping handling: %d %s", answered.Load(), raw)
	}
	researchSessionCount(t, f, 1)
	for _, revoke := range []bool{false, true} {
		trigger := make(chan struct{})
		f.peer.mu.Lock()
		f.peer.respond = func(w http.ResponseWriter, r *http.Request) {
			if answer(w, r) {
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, ": heartbeat\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-trigger:
			case <-r.Context().Done():
				return
			}
			fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"stream-ping\",\"method\":\"ping\"}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-replies:
				fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":1,\"progressToken\":1}}\n\n")
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
				t.Error("stream ping did not complete")
			}
		}
		f.peer.mu.Unlock()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		request, err := http.NewRequestWithContext(ctx, "GET", f.gateway.URL+"/code/qualification/mcp/search", nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+f.key)
		request.Header.Set("Mcp-Session-Id", session)
		stream, err := http.DefaultClient.Do(request)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if revoke {
			if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET revoked_at=now() WHERE id=$1`, f.keyID); err != nil {
				cancel()
				t.Fatal(err)
			}
		}
		close(trigger)
		body, err := io.ReadAll(stream.Body)
		stream.Body.Close()
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if answered.Load() != 2 || strings.Contains(string(body), "stream-ping") || !revoke && !strings.Contains(string(body), "notifications/progress") {
			t.Fatalf("ping crossed revocation or was exposed to caller: %d %s", answered.Load(), body)
		}
	}
}
