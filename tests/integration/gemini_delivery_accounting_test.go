//go:build integration

package integration_test

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

type geminiUndeliverableReply struct {
	body func(string) []byte
}

func TestGeminiRetainedInteractionPreservesUsageWhenProjectionFails(t *testing.T) {
	const limit = 1 << 20 // newAccessHarness's native and projected response bound.
	terminal := func(id, extra string) []byte {
		return []byte(fmt.Sprintf(`{"id":%q,"model":%q,"status":"completed","steps":[],"usage":%s%s}`,
			id, vendorModel, geminiUsageCheckpoint(5, 4, 0), extra))
	}
	padded := func(id string, size int) []byte {
		base := terminal(id, `,"padding":""`)
		return bytes.Replace(base, []byte(`"padding":""`), []byte(`"padding":"`+strings.Repeat("x", size-len(base))+`"`), 1)
	}
	for _, test := range []struct {
		name, code string
		observed   bool
		body       func(string) []byte
	}{
		{"projected-byte-limit", "upstream_response_too_large", true, func(id string) []byte { return padded(id, limit) }},
		{"unbound-parent", "provider_protocol_error", true, func(id string) []byte {
			return terminal(id, `,"previous_interaction_id":"unbound-parent"`)
		}},
		{"escaped-native-identity", "provider_protocol_error", true, func(id string) []byte {
			return terminal(id, fmt.Sprintf(`,"native":{"resource":"%s"}`, strings.ReplaceAll(id, "_", `\u005f`)))
		}},
		{"malformed-terminal", "provider_protocol_error", false, func(id string) []byte {
			return bytes.Replace(terminal(id, ""), []byte(`"steps":[]`), []byte(`"steps":{}`), 1)
		}},
		{"different-native-identity", "provider_protocol_error", false, func(string) []byte {
			return terminal("different-native-interaction", "")
		}},
		{"native-byte-limit", "upstream_response_too_large", false, func(id string) []byte { return padded(id, limit+1) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newAccessHarness(t)
			native := newGeminiLifecycleProvider(t)
			var reply atomic.Pointer[geminiUndeliverableReply]
			provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if current := reply.Load(); current != nil && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1beta/interactions/") {
					if r.Header.Get("X-Goog-Api-Key") != vendorSecret {
						t.Error("retained retrieval lost provider authentication")
						http.Error(w, "bad authentication", http.StatusUnauthorized)
						return
					}
					id := strings.TrimPrefix(r.URL.Path, "/v1beta/interactions/")
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(current.body(id))
					return
				}
				native.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(provider.Close)
			slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", &geminiLifecycleProvider{Server: provider})
			local, original, owner := pendingGeminiGeneration(t, h, slug, key)
			_, upstream, err := h.Gateway.Resources.ReadInteractionContract(t.Context(), owner, local)
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "projected-byte-limit" {
				nativeBytes := len(test.body(upstream))
				projectedBytes := nativeBytes + len(local) - len(upstream) + len(slug) - len(vendorModel)
				if nativeBytes != limit || projectedBytes <= limit {
					t.Fatalf("fixture does not isolate projection growth: native=%d projected=%d limit=%d", nativeBytes, projectedBytes, limit)
				}
				t.Logf("native bytes=%d projected bytes=%d limit=%d", nativeBytes, projectedBytes, limit)
			}
			reply.Store(&geminiUndeliverableReply{body: test.body})
			sink := &captureSink{}
			h.Gateway.Sink = sink
			for range 2 {
				response, raw := geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/interactions/"+local, key, nil)
				if response.StatusCode != http.StatusBadGateway || !bytes.Contains(raw, []byte(test.code)) || bytes.Contains(raw, []byte(upstream)) {
					t.Fatalf("delivery rejection: status=%d body=%s", response.StatusCode, raw[:min(len(raw), 2048)])
				}
				res, _, err := h.Gateway.Resources.ReadInteractionContract(t.Context(), owner, local)
				if err != nil {
					t.Fatal(err)
				}
				if test.observed {
					assertGeminiUsageSnapshot(t, h, original, true, 5, 4, 0)
					if res.State != "completed" || bytes.Contains(res.Metadata, []byte(`"pending_usage"`)) {
						t.Fatalf("terminal provider work stayed pending after delivery rejection: state=%q metadata=%s", res.State, res.Metadata)
					}
					if err := h.Gateway.Resources.CheckPendingCostOwner(t.Context(), owner); err != nil {
						t.Fatalf("terminal provider usage kept owner quarantined: %v", err)
					}
				} else {
					var count int
					if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.attempt_usage_facts WHERE request_id=$1`, original).Scan(&count); err != nil || count != 0 {
						t.Fatalf("invalid native reply was accounted: count=%d err=%v", count, err)
					}
					if res.State != "in_progress" || !bytes.Contains(res.Metadata, []byte(`"pending_usage"`)) || !errors.Is(h.Gateway.Resources.CheckPendingCostOwner(t.Context(), owner), resources.ErrPendingUsage) {
						t.Fatalf("invalid native reply released pending ownership: state=%q", res.State)
					}
				}
				if sink.count() == 0 || len(sink.last().Attempts) != 1 {
					t.Fatal("retrieval lost its terminal attempt evidence")
				}
				fact := sink.last().Attempts[0]
				if fact.Interaction == nil || fact.Interaction.ClientState != usage.ClientUnobserved || fact.Committed || fact.UsageObserved || fact.Usage != nil {
					t.Fatalf("undelivered retrieval claimed delivery or generation usage: %+v", fact)
				}
				if test.observed && (fact.Class != "success" || fact.Interaction.UpstreamState != usage.UpstreamTerminal) {
					t.Fatalf("delivery rejection erased validated provider completion: %+v", fact)
				}
				if !test.observed && fact.Interaction.UpstreamState == usage.UpstreamTerminal {
					t.Fatalf("invalid provider reply claimed valid completion: %+v", fact)
				}
			}
		})
	}
}
