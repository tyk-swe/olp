//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const realtimeCreate = `{"type":"response.create","event_id":"create_1"}`
const realtimeCreated = `{"type":"response.created","event_id":"created_1","response":{"id":"resp_1","status":"in_progress"}}`
const realtimeDelta = `{"type":"response.audio.delta","event_id":"delta_1","response_id":"resp_1","delta":"AQID"}`
const realtimeDone = `{"type":"response.done","event_id":"done_1","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`
const realtimeAmbiguousDone = `{"type":"response.audio.delta","type":"response.done","event_id":"done_1","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`

func TestStrictRealtimeNormalCloseContracts(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		providerEarly bool
		complete      bool
		ambiguousDone bool
	}{
		{name: "client_closes_pending_response"},
		{name: "provider_closes_pending_response", providerEarly: true},
		{name: "client_closes_completed_response", complete: true},
		{name: "provider_closes_after_duplicate_terminal_type", ambiguousDone: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			h := newAccessHarness(t)
			sink := &captureSink{}
			h.Gateway.Sink = sink
			var dials atomic.Int64
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, map[string]any{"data": []map[string]string{{"id": vendorModel}}})
			})
			mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, _ *http.Request) {
				writeResponsesFixture(w, vendorModel, "OK", false)
			})
			mux.HandleFunc("GET /v1/realtime", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("model") != vendorModel || r.Header.Get("Authorization") != "Bearer "+vendorSecret {
					http.Error(w, "wrong native binding", http.StatusBadRequest)
					return
				}
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				dials.Add(1)
				ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
				defer cancel()
				kind, got, err := conn.Read(ctx)
				if err != nil {
					return // Certification checks the handshake, then closes.
				}
				if kind != websocket.MessageText || !bytes.Equal(got, []byte(realtimeCreate)) {
					t.Errorf("provider saw changed response request: kind=%v err=%v got=%q", kind, err, got)
					return
				}
				for _, frame := range []string{realtimeCreated, realtimeDelta} {
					if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
						return
					}
				}
				if scenario.providerEarly {
					_ = conn.Close(websocket.StatusNormalClosure, "")
					return
				}
				if scenario.ambiguousDone {
					if err := conn.Write(ctx, websocket.MessageText, []byte(realtimeAmbiguousDone)); err != nil {
						return
					}
					_ = conn.Close(websocket.StatusNormalClosure, "")
					return
				}
				if scenario.complete {
					if err := conn.Write(ctx, websocket.MessageText, []byte(realtimeDone)); err != nil {
						return
					}
				}
				_, _, _ = conn.Read(ctx)
			})
			fixture := httptest.NewServer(mux)
			t.Cleanup(fixture.Close)
			slug, key := provisionStrictRealtime(t, h, "openai", fixture.URL+"/v1")
			beforeDials, beforeTerminals := dials.Load(), sink.count()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			address := strings.Replace(h.HTTP.URL, "http://", "ws://", 1) + "/v1/realtime?model=" + slug
			conn, _, err := websocket.Dial(ctx, address, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + key}}})
			if err != nil {
				t.Fatalf("strict realtime dial: %v", err)
			}
			defer conn.CloseNow()
			if err := conn.Write(ctx, websocket.MessageText, []byte(realtimeCreate)); err != nil {
				t.Fatalf("response.create: %v", err)
			}
			for _, want := range []string{realtimeCreated, realtimeDelta} {
				kind, got, err := conn.Read(ctx)
				if err != nil || kind != websocket.MessageText || !bytes.Equal(got, []byte(want)) {
					t.Fatalf("native response event changed: kind=%v err=%v got=%q", kind, err, got)
				}
			}
			if scenario.complete {
				kind, got, err := conn.Read(ctx)
				if err != nil || kind != websocket.MessageText || !bytes.Equal(got, []byte(realtimeDone)) {
					t.Fatalf("native terminal event changed: kind=%v err=%v got=%q", kind, err, got)
				}
			}
			if scenario.ambiguousDone {
				kind, got, err := conn.Read(ctx)
				if err != nil || kind != websocket.MessageText || !bytes.Equal(got, []byte(realtimeAmbiguousDone)) {
					t.Fatalf("ambiguous native event changed: kind=%v err=%v got=%q", kind, err, got)
				}
			}
			if scenario.providerEarly || scenario.ambiguousDone {
				_, _, err = conn.Read(ctx)
				if err == nil || websocket.CloseStatus(err) == websocket.StatusNormalClosure || websocket.CloseStatus(err) == websocket.StatusGoingAway {
					t.Fatalf("incomplete provider 1000 became normal client close: %v", err)
				}
			} else if err := conn.Close(websocket.StatusNormalClosure, ""); err != nil {
				t.Fatalf("normal client close: %v", err)
			}
			deadline := time.Now().Add(3 * time.Second)
			for sink.count() == beforeTerminals && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if dials.Load() != beforeDials+1 || sink.count() != beforeTerminals+1 {
				t.Fatalf("close caused duplicate/missing dispatch or terminal: dials=%d terminals=%d", dials.Load()-beforeDials, sink.count()-beforeTerminals)
			}
			terminal := sink.last()
			if !terminal.Committed || len(terminal.Attempts) != 1 || terminal.Attempts[0].Interaction == nil {
				t.Fatalf("strict close lost one committed native Attempt: %+v", terminal)
			}
			fact := terminal.Attempts[0]
			interaction := fact.Interaction
			if interaction.Fidelity != "strict" || interaction.PlanClass != "native_identity" {
				t.Fatalf("strict response lost native plan evidence: %+v", interaction)
			}
			switch {
			case scenario.providerEarly || scenario.ambiguousDone:
				if terminal.Outcome != "failure" || terminal.ErrorClass != "realtime_incomplete" || fact.Class != "protocol" ||
					interaction.UpstreamState != "outcome-unknown" || interaction.ClientState != "partially-observed" ||
					fact.UsageObserved || fact.UsageComplete || !fact.BillingUncertain {
					t.Fatalf("incomplete provider close looked terminal: %+v", terminal)
				}
			case scenario.complete:
				if terminal.Outcome != "success" || terminal.Status != http.StatusSwitchingProtocols || terminal.ErrorClass != "" || fact.Class != "success" ||
					interaction.UpstreamState != "terminal" || interaction.ClientState != "terminal" ||
					!fact.UsageObserved || !fact.UsageComplete || fact.BillingUncertain || fact.Usage == nil || fact.Usage.InputTokens != 3 || fact.Usage.OutputTokens != 2 {
					t.Fatalf("completed response lost clean close: %+v", terminal)
				}
			default:
				if terminal.Outcome != "cancelled" || terminal.Status != 0 || terminal.ErrorClass != "client_cancelled" || fact.Class != "cancelled" ||
					interaction.UpstreamState != "outcome-unknown" || interaction.ClientState != "partially-observed" ||
					fact.UsageObserved || fact.UsageComplete || !fact.BillingUncertain {
					t.Fatalf("graceful client cancellation looked terminal: %+v", terminal)
				}
			}
		})
	}
}
