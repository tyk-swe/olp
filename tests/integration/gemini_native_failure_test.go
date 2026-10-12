//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

const geminiNativeFailureExtension = `{"number":9007199254740993,"decimal":1.2300e+02,"null":null,"empty":"","false":false,"array":[],"object":{}}`

func geminiNativeFailureDifference(got, want []byte) string {
	for i := 0; i < len(got) && i < len(want); i++ {
		if got[i] != want[i] {
			return fmt.Sprintf("got length=%d want length=%d first different byte=%d got=%02x want=%02x", len(got), len(want), i, got[i], want[i])
		}
	}
	return fmt.Sprintf("got length=%d want length=%d common prefix preserved", len(got), len(want))
}

// The diagnostic echoes the authentication actually received by the upstream,
// including an arbitrary JSON escape spelling that a raw byte scrub misses.
func geminiNativeFailureDiagnostic(key string, escape bool) string {
	message, _ := json.Marshal("X-Goog-Api-Key " + key + " was rejected")
	echo, _ := json.Marshal("echo " + key + " trailing")
	if escape {
		var escaped strings.Builder
		for _, char := range key {
			fmt.Fprintf(&escaped, `\u%04x`, char)
		}
		echo = bytes.Replace(echo, []byte(key), []byte(escaped.String()), 1)
	}
	return fmt.Sprintf(`{"code":500,"message":%s,"details":[{"echo":%s,"retryable":false}]}`, message, echo)
}

func geminiNativeFailureObject(id, model, diagnostic string) string {
	return fmt.Sprintf(`{"id":%q,"model":%q,"status":"failed","steps":[],"usage":%s,"error":%s,"native":%s}`,
		id, model, geminiUsageCheckpoint(5, 4, 0), diagnostic, geminiNativeFailureExtension)
}

type geminiNativeUnaryFlushWriter struct {
	*httptest.ResponseRecorder
	flushes       int
	writeDeadline time.Time
}

func (w *geminiNativeUnaryFlushWriter) FlushError() error {
	w.flushes++
	return io.ErrClosedPipe
}

func (w *geminiNativeUnaryFlushWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		w.writeDeadline = deadline
	}
	return nil
}

func TestGeminiNativeUnaryInteractionBufferedFlushFailure(t *testing.T) {
	for _, operation := range []string{"generation", "retrieval"} {
		for _, state := range []string{"completed", "failed"} {
			t.Run(operation+"/"+state, func(t *testing.T) {
				h := newAccessHarness(t)
				provider := newGeminiLifecycleProvider(t)
				provider.partialUsage.Store(true)
				provider.terminalUsage.Store(true)
				var active *atomic.Bool
				var failureCalls *atomic.Int32
				if state == "failed" {
					provider, active, failureCalls = newGeminiNativeFailureProvider(t, "unary")
				}
				slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", provider, map[string]any{"response_metadata": false})
				method, path := http.MethodPost, "/gemini/v1beta/interactions"
				body := fmt.Sprintf(`{"model":%q,"input":"unary buffered flush proof","store":true}`, slug)
				var original, owner string
				if operation == "retrieval" {
					local, pendingOriginal, pendingOwner := pendingGeminiGeneration(t, h, slug, key)
					original, owner = pendingOriginal, pendingOwner
					method, path, body = http.MethodGet, path+"/"+local, ""
				}
				baseline := len(provider.captured())
				if active != nil {
					active.Store(true)
				}
				sink := &captureSink{}
				h.Gateway.Sink = sink
				writer := &geminiNativeUnaryFlushWriter{ResponseRecorder: httptest.NewRecorder()}
				request := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
				request.Header.Set("X-Goog-Api-Key", key)
				request.Header.Set("Content-Type", "application/json")
				started := time.Now()
				h.HTTP.Config.Handler.ServeHTTP(writer, request)
				if operation == "generation" && (writer.Result().Header.Get("X-Should-Retry") != "false" || writer.Result().Header.Get("X-OLP-Attempts") != "") {
					t.Error("accepted buffered native generation lacked its no-retry fence before body commitment with response metadata disabled")
				}
				calls := len(provider.captured()) - baseline
				if failureCalls != nil {
					calls = int(failureCalls.Load())
				}
				if calls != 1 || sink.count() != 1 || len(sink.last().Attempts) != 1 || writer.Body.Len() == 0 {
					t.Fatalf("buffered unary fixture missed its single native write: calls=%d terminal=%d bytes=%d", calls, sink.count(), writer.Body.Len())
				}
				event := sink.last()
				fact := event.Attempts[0]
				t.Logf("buffered unary first-flush failure: flushes=%d deadline-set=%t retry-header=%q class=%s outcome=%s first-byte=%t upstream=%s client=%s", writer.flushes, !writer.writeDeadline.IsZero(), writer.Result().Header.Get("X-Should-Retry"), fact.Class, event.Outcome, event.FirstByte != nil, fact.Interaction.UpstreamState, fact.Interaction.ClientState)
				if writer.flushes != 1 || writer.writeDeadline.Before(started) || writer.writeDeadline.After(started.Add(time.Minute)) {
					t.Error("native unary delivery did not attempt its bounded first flush")
				}
				if event.Outcome != "cancelled" || event.Status != 0 || !event.Committed || event.FirstByte != nil || fact.Interaction == nil || fact.Interaction.UpstreamState != usage.UpstreamTerminal || fact.Interaction.ClientState != usage.ClientUnobserved {
					t.Error("buffered unary first-flush failure claimed successful client delivery")
				}
				wantClass := "cancelled"
				if operation == "generation" && state == "failed" {
					wantClass = "upstream_server"
				}
				if fact.Class != wantClass {
					t.Errorf("buffered unary first-flush failure changed known provider outcome: got=%s want=%s", fact.Class, wantClass)
				}
				if operation == "generation" {
					wantInput, wantOutput := int64(3), int64(2)
					if state == "failed" {
						wantInput, wantOutput = 5, 4
					}
					if fact.Usage == nil || !fact.UsageObserved || !fact.UsageComplete || fact.BillingUncertain || fact.Usage.InputTokens != wantInput || fact.Usage.OutputTokens != wantOutput {
						t.Error("buffered unary first-flush failure erased original known consumption")
					}
				} else {
					assertGeminiUsageSnapshot(t, h, original, true, 5, 4, 0)
					if fact.Usage != nil || fact.UsageObserved || fact.GenerationUsageDeferred {
						t.Error("buffered retained unary read created a new generation charge")
					}
					if err := h.Gateway.Resources.CheckPendingCostOwner(t.Context(), owner); err != nil {
						t.Errorf("buffered retained unary read left original usage pending: %v", err)
					}
				}
			})
		}
	}
}

func geminiNativeFailureStream(id, model, diagnostic string) string {
	created := fmt.Sprintf(`{"event_type":"interaction.created","interaction":{"id":%q,"model":%q,"status":"in_progress","usage":%s},"native":%s}`,
		id, model, geminiUsageCheckpoint(3, 2, 0), geminiNativeFailureExtension)
	failed := fmt.Sprintf(`{"event_type":"error","interaction_id":%q,"interaction":{"id":%q,"model":%q,"status":"failed","usage":%s},"error":%s,"native":%s}`,
		id, id, model, geminiUsageCheckpoint(5, 4, 0), diagnostic, geminiNativeFailureExtension)
	return "id: cursor-created\nevent: native-created\nretry: 17\ndata: " + created + "\n\n" +
		"id: cursor-failed\nevent: native-error\nretry: 23\ndata: " + failed + "\n\n"
}

func newGeminiNativeFailureProvider(t *testing.T, mode string) (*geminiLifecycleProvider, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	native := newGeminiLifecycleProvider(t)
	native.partialUsage.Store(true)
	active, calls := &atomic.Bool{}, &atomic.Int32{}
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !active.Load() || !strings.HasPrefix(r.URL.Path, "/v1beta/interactions") {
			native.Config.Handler.ServeHTTP(w, r)
			return
		}
		calls.Add(1)
		credential := r.Header.Get("X-Goog-Api-Key")
		if credential != vendorSecret {
			t.Error("native failure fixture did not receive the applied provider credential")
			http.Error(w, "bad authentication", http.StatusUnauthorized)
			return
		}
		id := "v1_native_failure_fixture"
		if r.Method == http.MethodGet {
			id = strings.TrimPrefix(r.URL.Path, "/v1beta/interactions/")
		}
		diagnostic := geminiNativeFailureDiagnostic(credential, true)
		if mode == "sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, geminiNativeFailureStream(id, vendorModel, diagnostic))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, geminiNativeFailureObject(id, vendorModel, diagnostic))
	}))
	t.Cleanup(provider.Close)
	return &geminiLifecycleProvider{Server: provider}, active, calls
}

func geminiNativeFailureHealth(t *testing.T, h *accessHarness, ownerBrowser *browser, providerID string) map[string]any {
	t.Helper()
	page := h.want(ownerBrowser, http.MethodGet, "/api/v1/provider-health", nil, nil, http.StatusOK)
	for _, raw := range page["items"].([]any) {
		item := raw.(map[string]any)
		if item["provider_id"] == providerID {
			return item
		}
	}
	t.Fatal("public provider health omitted the fixture provider")
	return nil
}

func TestGeminiNativeInteractionFailureRedactsAppliedCredential(t *testing.T) {
	for _, operation := range []string{"generation", "retrieval"} {
		for _, mode := range []string{"unary", "sse"} {
			t.Run(operation+"/"+mode, func(t *testing.T) {
				h := newAccessHarness(t)
				provider, active, calls := newGeminiNativeFailureProvider(t, mode)
				ownerBrowser := h.owner()
				slug, key, providerID := provisionGeminiLifecycle(t, h, ownerBrowser, "gemini-interactions", provider, map[string]any{"response_metadata": false})
				health := func() map[string]any {
					return geminiNativeFailureHealth(t, h, ownerBrowser, providerID)
				}
				method, path := http.MethodPost, "/gemini/v1beta/interactions"
				body := []byte(fmt.Sprintf(`{"model":%q,"input":"native failure credential proof","store":true,"stream":%t}`, slug, mode == "sse"))
				var local, original, owner string
				if operation == "retrieval" {
					local, original, owner = pendingGeminiGeneration(t, h, slug, key)
					method, path, body = http.MethodGet, path+"/"+local, nil
					if mode == "sse" {
						path += "?stream=true"
					}
				}
				sink := &captureSink{}
				h.Gateway.Sink = sink
				beforeHealth := health()
				beforeLiveHealth := h.Gateway.Health().ProviderHealth(time.Hour)[providerID]
				active.Store(true)
				response, raw := geminiPublic(t, h, method, path, key, body)
				if response.StatusCode != http.StatusOK {
					t.Fatalf("native failure document changed HTTP status: got=%d want=%d", response.StatusCode, http.StatusOK)
				}
				if operation == "generation" && (response.Header.Get("X-Should-Retry") != "false" || response.Header.Get("X-OLP-Attempts") != "") {
					t.Error("accepted native generation lost its no-retry fence with response metadata disabled")
				}
				if calls.Load() != 1 || sink.count() != 1 || len(sink.last().Attempts) != 1 {
					t.Fatalf("native failure repeated provider work or lost one terminal fact: calls=%d terminal=%d", calls.Load(), sink.count())
				}
				terminal := sink.last()
				fact := terminal.Attempts[0]
				if !fact.Committed || fact.Status != http.StatusOK || fact.Interaction == nil || fact.Interaction.UpstreamState != usage.UpstreamTerminal {
					t.Fatalf("native failure lost accepted terminal/delivery evidence: %+v", fact)
				}
				if fact.Interaction.ClientState != usage.ClientTerminal || terminal.FirstByte == nil {
					t.Error("fully flushed native failed document did not record truthful terminal client visibility")
				}
				if operation == "generation" {
					original, owner = terminal.AccountingID, terminal.KeyID
					if fact.Usage == nil || !fact.UsageObserved || !fact.UsageComplete || fact.BillingUncertain || fact.Usage.InputTokens != 5 || fact.Usage.OutputTokens != 4 || fact.Usage.CachedInputTokens == nil || *fact.Usage.CachedInputTokens != 0 {
						t.Fatalf("native failure erased exact generation usage: %+v", fact)
					}
					if mode == "unary" {
						var interaction struct{ ID string }
						if json.Unmarshal(raw, &interaction) != nil {
							t.Fatal("native unary failure is not JSON")
						}
						local = interaction.ID
					} else {
						first, _, found := bytes.Cut(raw, []byte("\n\n"))
						_, data, hasData := bytes.Cut(first, []byte("data: "))
						var created struct{ Interaction struct{ ID string } }
						if !found || !hasData || json.Unmarshal(data, &created) != nil {
							t.Fatal("native SSE failure lost its valid created frame")
						}
						local = created.Interaction.ID
					}
				} else if fact.Usage != nil || fact.UsageObserved || fact.GenerationUsageDeferred {
					t.Fatalf("retrieval invented a new generation charge: %+v", fact)
				}
				if !strings.HasPrefix(local, "interaction_") {
					t.Fatal("native failure omitted the owner-scoped Interaction ID")
				}
				resource, upstream, err := h.Gateway.Resources.ReadInteractionContract(t.Context(), owner, local)
				if err != nil || resource.State != "failed" || bytes.Contains(resource.Metadata, []byte(`"pending_usage"`)) {
					t.Fatalf("native failed work lost its retained state or original accounting: resource=%+v err=%v", resource, err)
				}
				if bytes.Contains(raw, []byte(upstream)) || bytes.Contains(raw, []byte(vendorModel)) {
					t.Fatal("native failure exposed an unprojected provider identity")
				}
				if operation == "retrieval" || mode == "sse" {
					assertGeminiUsageSnapshot(t, h, original, true, 5, 4, 0)
					if err := h.Gateway.Resources.CheckPendingCostOwner(t.Context(), owner); err != nil {
						t.Fatalf("native failed work retained a settled cost owner: %v", err)
					}
				}
				afterHealth := health()
				afterLiveHealth := h.Gateway.Health().ProviderHealth(time.Hour)[providerID]
				delta := func(field string) int {
					return int(afterHealth[field].(float64) - beforeHealth[field].(float64))
				}
				t.Logf("native failure: HTTP=%d upstream=%s client=%s class=%s outcome=%s usage preserved; provider calls=%d; runtime health delta attempts/success/server/transport=%d/%d/%d/%d; persisted public health delta=%d/%d/%d/%d", response.StatusCode, fact.Interaction.UpstreamState, fact.Interaction.ClientState, fact.Class, terminal.Outcome, calls.Load(), afterLiveHealth.Attempts-beforeLiveHealth.Attempts, afterLiveHealth.Successes-beforeLiveHealth.Successes, afterLiveHealth.ServerErrors-beforeLiveHealth.ServerErrors, afterLiveHealth.TransportErrors-beforeLiveHealth.TransportErrors, delta("attempt_count"), delta("success_count"), delta("server_error_count"), delta("transport_error_count"))
				if operation == "generation" && mode == "sse" && (delta("attempt_count") != 1 || delta("success_count") != 0 || delta("server_error_count") != 1 || delta("transport_error_count") != 0) {
					t.Error("persisted original native generation failure did not become one provider server error with no provider success")
				}
				if operation == "generation" && (fact.Class != "upstream_server" || terminal.Outcome != "failure") {
					t.Error("active native generation failure was recorded as a successful provider result")
				}
				if operation == "retrieval" && (fact.Class != "success" || terminal.Outcome != "success") {
					t.Error("successful retrieval of a failed retained resource was classified as a new provider failure")
				}
				// Successful retrieval can report a failed retained resource. This
				// regression concerns credential sanitation, not that distinction.
				if bytes.Contains(raw, []byte(vendorSecret)) {
					t.Error("public native failure document exposes the applied upstream credential")
				}
				diagnostic := geminiNativeFailureDiagnostic("[REDACTED]", false)
				want := geminiNativeFailureObject(local, slug, diagnostic)
				if mode == "sse" {
					want = geminiNativeFailureStream(local, slug, diagnostic)
				}
				if !bytes.Equal(raw, []byte(want)) {
					t.Errorf("native failure differs from the exact identity-projected, credential-redacted document; member order, number spelling, extensions and SSE framing must remain intact: %s", geminiNativeFailureDifference(raw, []byte(want)))
				}
			})
		}
	}
}

type geminiNativeFailureWriter struct {
	*httptest.ResponseRecorder
	offered []byte
}

func (w *geminiNativeFailureWriter) Write(body []byte) (int, error) {
	if bytes.Contains(body, []byte(`"status":"failed"`)) || bytes.Contains(body, []byte(`"event_type":"error"`)) {
		w.offered = bytes.Clone(body)
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(body)
}

func TestGeminiNativeInteractionFailureSurvivesClientDeliveryFailure(t *testing.T) {
	for _, mode := range []string{"unary", "sse"} {
		t.Run(mode, func(t *testing.T) {
			h := newAccessHarness(t)
			provider, active, calls := newGeminiNativeFailureProvider(t, mode)
			ownerBrowser := h.owner()
			slug, key, providerID := provisionGeminiLifecycle(t, h, ownerBrowser, "gemini-interactions", provider, map[string]any{"response_metadata": false})
			sink := &captureSink{}
			h.Gateway.Sink = sink
			active.Store(true)
			writer := &geminiNativeFailureWriter{ResponseRecorder: httptest.NewRecorder()}
			body := fmt.Sprintf(`{"model":%q,"input":"native failure delivery proof","store":true,"stream":%t}`, slug, mode == "sse")
			request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/gemini/v1beta/interactions", strings.NewReader(body))
			request.Header.Set("X-Goog-Api-Key", key)
			request.Header.Set("Content-Type", "application/json")
			h.HTTP.Config.Handler.ServeHTTP(writer, request)
			if writer.Header().Get("X-Should-Retry") != "false" || writer.Header().Get("X-OLP-Attempts") != "" {
				t.Error("failed native generation delivery lost its accepted no-retry fence with response metadata disabled")
			}
			if calls.Load() != 1 || sink.count() != 1 || len(sink.last().Attempts) != 1 || len(writer.offered) == 0 {
				t.Fatalf("native failure delivery proof missed its terminal writer boundary: calls=%d terminal=%d offered=%d", calls.Load(), sink.count(), len(writer.offered))
			}
			event := sink.last()
			fact := event.Attempts[0]
			if event.Outcome != "cancelled" || event.Status != 0 || fact.Interaction == nil || fact.Interaction.UpstreamState != usage.UpstreamTerminal || fact.Interaction.ClientState == usage.ClientTerminal {
				t.Fatalf("native failure delivery lost the separate caller/provider outcomes: %+v", event)
			}
			if fact.Usage == nil || !fact.UsageObserved || !fact.UsageComplete || fact.BillingUncertain || fact.Usage.InputTokens != 5 || fact.Usage.OutputTokens != 4 {
				t.Fatalf("failed client delivery erased known native failure usage: %+v", fact)
			}
			t.Logf("native failure client delivery: class=%s outcome=%s upstream=%s client=%s provider calls=%d", fact.Class, event.Outcome, fact.Interaction.UpstreamState, fact.Interaction.ClientState, calls.Load())
			if mode == "sse" {
				health := geminiNativeFailureHealth(t, h, ownerBrowser, providerID)
				t.Logf("native failure client delivery public health attempts/success/server/transport=%v/%v/%v/%v", health["attempt_count"], health["success_count"], health["server_error_count"], health["transport_error_count"])
				if health["attempt_count"] != float64(1) || health["success_count"] != float64(0) || health["server_error_count"] != float64(1) || health["transport_error_count"] != float64(0) {
					t.Error("cancelled delivery erased the original persisted native provider failure")
				}
			}
			if fact.Class != "upstream_server" {
				t.Error("known native generation failure was replaced by the client delivery failure class")
			}
			if bytes.Contains(writer.offered, []byte(vendorSecret)) {
				t.Error("native failure writer was offered an applied provider credential")
			}
			diagnostic := geminiNativeFailureDiagnostic("[REDACTED]", false)
			var offered struct {
				ID          string
				Interaction struct{ ID string }
			}
			document := writer.offered
			if mode == "sse" {
				_, document, _ = bytes.Cut(document, []byte("data: "))
			}
			if json.Unmarshal(document, &offered) != nil {
				t.Fatal("failed native writer was not offered its native JSON document")
			}
			wantOffered, wantWritten := geminiNativeFailureObject(offered.ID, slug, diagnostic), ""
			if mode == "sse" {
				first, tail, _ := strings.Cut(geminiNativeFailureStream(offered.Interaction.ID, slug, diagnostic), "\n\n")
				wantOffered, wantWritten = tail, first+"\n\n"
			}
			if !bytes.Equal(writer.offered, []byte(wantOffered)) || !bytes.Equal(writer.Body.Bytes(), []byte(wantWritten)) {
				t.Error("native failed delivery changed wire shape or added an error frame beyond credential sanitation and local ID projection")
			}
		})
	}
}
