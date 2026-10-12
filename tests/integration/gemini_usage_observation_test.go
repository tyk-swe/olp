//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

// Replies are immutable once published. They describe usage checkpoints of
// the same native work, including a replay without a last_event_id cursor.
type geminiAccountingReply struct {
	status string
	usage  string
}

func (reply *geminiAccountingReply) write(w http.ResponseWriter, id string, stream bool) {
	counts := ""
	if reply.usage != "" {
		counts = `,"usage":` + reply.usage
	}
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "id: cursor-created\ndata: {\"event_type\":\"interaction.created\",\"interaction\":{\"id\":%q,\"status\":\"in_progress\"%s}}\n\n", id, counts)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	steps := ""
	if reply.status == "completed" {
		steps = `,"steps":[]`
	}
	fmt.Fprintf(w, `{"id":%q,"model":%q,"status":%q%s%s}`, id, vendorModel, reply.status, steps, counts)
}

func geminiUsageCheckpoint(input, output, cached int64) string {
	return fmt.Sprintf(`{"total_input_tokens":%d,"total_output_tokens":%d,"total_cached_tokens":%d}`, input, output, cached)
}

func observeGeminiCheckpoint(t *testing.T, h *accessHarness, provider *geminiLifecycleProvider, local, key, counts string, stream bool) {
	t.Helper()
	provider.accountingReply.Store(&geminiAccountingReply{status: "in_progress", usage: counts})
	path := "/gemini/v1beta/interactions/" + local
	if stream {
		path += "?stream=true"
	}
	response, raw := geminiPublic(t, h, http.MethodGet, path, key, nil)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(raw), `"id":"`+local+`"`) {
		t.Fatalf("checkpoint retrieval: %d %s", response.StatusCode, raw)
	}
}

func assertGeminiUsageSnapshot(t *testing.T, h *accessHarness, original string, complete bool, input, output, cached int64) {
	t.Helper()
	var count int
	var observed, settled, uncertain bool
	var in, out, cache int64
	err := h.Pool.QueryRow(t.Context(), `SELECT count(*),bool_and(usage_observed),bool_and(usage_complete),bool_and(charge_status='billing_uncertain'),
 COALESCE(sum(input_tokens),0),COALESCE(sum(output_tokens),0),COALESCE(sum(cached_input_tokens),0)
 FROM olp.attempt_usage_facts WHERE request_id=$1`, original).Scan(&count, &observed, &settled, &uncertain, &in, &out, &cache)
	if err != nil || count != 1 || !observed || settled != complete || uncertain == complete || in != input || out != output || cache != cached {
		t.Fatalf("original usage count=%d observed=%v complete=%v uncertain=%v tokens=%d/%d/%d err=%v", count, observed, settled, uncertain, in, out, cache, err)
	}
}

func TestGeminiPartialUsageReplaysSurviveCleanup(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", provider)
	for _, observer := range []string{"get", "stream"} {
		for _, cleanup := range []string{"delete", "expiry"} {
			t.Run(observer+"-"+cleanup, func(t *testing.T) {
				local, original, _ := pendingGeminiGeneration(t, h, slug, key)
				observeGeminiCheckpoint(t, h, provider, local, key, geminiUsageCheckpoint(10, 4, 2), observer == "stream")
				// Another gateway repeats an older native checkpoint. It must not
				// reduce the original request's known consumption before cleanup.
				replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
				replica.refresh()
				observeGeminiCheckpoint(t, replica, provider, local, key, geminiUsageCheckpoint(10, 1, 2), observer == "stream")
				if cleanup == "delete" {
					response, raw := geminiPublic(t, replica, http.MethodDelete, "/gemini/v1beta/interactions/"+local, key, nil)
					if response.StatusCode != http.StatusOK {
						t.Fatalf("delete: %d %s", response.StatusCode, raw)
					}
				} else {
					id := strings.TrimPrefix(local, "interaction_")
					if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_resources SET expires_at=now()-interval '1 second' WHERE id=$1`, id); err != nil {
						t.Fatal(err)
					}
					if _, err := replica.Gateway.Resources.Expire(t.Context(), time.Now()); err != nil {
						t.Fatal(err)
					}
				}
				assertGeminiUsageSnapshot(t, h, original, false, 10, 4, 2)
				if _, err := h.Gateway.Resources.Expire(t.Context(), time.Now()); err != nil {
					t.Fatal(err)
				}
				assertGeminiUsageSnapshot(t, h, original, false, 10, 4, 2)
			})
		}
	}
}

func TestGeminiPartialUsageKeepsOneSnapshotAndTerminalAuthority(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", provider)
	for _, test := range []struct {
		name, next, final, terminal string
		complete                    bool
		input, output, cached       int64
	}{
		{"incomparable-cleanup", geminiUsageCheckpoint(11, 1, 2), "", "delete", false, 10, 4, 2},
		{"cache-repartition-cleanup", geminiUsageCheckpoint(11, 4, 10), "", "delete", false, 10, 4, 2},
		{"missing-cache-cleanup", `{"total_input_tokens":11,"total_output_tokens":5}`, "", "delete", false, 10, 4, 2},
		{"coherent-advance-cleanup", geminiUsageCheckpoint(12, 6, 3), "", "delete", false, 12, 6, 3},
		{"missing-cancel-usage", geminiUsageCheckpoint(10, 1, 2), "", "cancel", false, 10, 4, 2},
		{"lower-authoritative-final", geminiUsageCheckpoint(11, 1, 2), geminiUsageCheckpoint(10, 1, 2), "completed", true, 10, 1, 2},
		{"zero-authoritative-final", geminiUsageCheckpoint(10, 1, 2), geminiUsageCheckpoint(0, 0, 0), "completed", true, 0, 0, 0},
		{"cache-authoritative-final", geminiUsageCheckpoint(11, 4, 10), geminiUsageCheckpoint(11, 5, 11), "completed", true, 11, 5, 11},
	} {
		t.Run(test.name, func(t *testing.T) {
			local, original, _ := pendingGeminiGeneration(t, h, slug, key)
			observeGeminiCheckpoint(t, h, provider, local, key, geminiUsageCheckpoint(10, 4, 2), false)
			observeGeminiCheckpoint(t, h, provider, local, key, test.next, false)
			method, path := http.MethodGet, "/gemini/v1beta/interactions/"+local
			switch test.terminal {
			case "delete":
				method = http.MethodDelete
			case "cancel":
				method, path = http.MethodPost, path+"/cancel"
			}
			nativeStatus := test.terminal
			if nativeStatus == "cancel" {
				nativeStatus = "cancelled"
			}
			provider.accountingReply.Store(&geminiAccountingReply{status: nativeStatus, usage: test.final})
			response, raw := geminiPublic(t, h, method, path, key, nil)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("terminal observer: %d %s", response.StatusCode, raw)
			}
			assertGeminiUsageSnapshot(t, h, original, test.complete, test.input, test.output, test.cached)
			if test.terminal != "delete" {
				// A later complete replay may return different totals, but the
				// removed pending owner cannot create or amend another event.
				provider.accountingReply.Store(&geminiAccountingReply{status: "completed", usage: geminiUsageCheckpoint(99, 99, 0)})
				response, raw = geminiPublic(t, h, http.MethodGet, "/gemini/v1beta/interactions/"+local, key, nil)
				if response.StatusCode != http.StatusOK {
					t.Fatalf("settled replay: %d %s", response.StatusCode, raw)
				}
				assertGeminiUsageSnapshot(t, h, original, test.complete, test.input, test.output, test.cached)
			}
		})
	}
}

func TestGeminiObservedZeroCheckpointSurvivesMissingTerminalUsage(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", provider)
	local, original, _ := pendingGeminiGeneration(t, h, slug, key)
	observeGeminiCheckpoint(t, h, provider, local, key, geminiUsageCheckpoint(0, 0, 0), false)
	var raw []byte
	if err := h.Pool.QueryRow(t.Context(), `SELECT metadata->'pending_usage' FROM olp.provider_resources WHERE id=$1`, strings.TrimPrefix(local, "interaction_")).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	pending, err := usage.Decode(raw)
	if err != nil || pending.Attempts[0].Usage.CachedInputTokens == nil || *pending.Attempts[0].Usage.CachedInputTokens != 0 {
		t.Fatalf("known cache zero became absent: %s %v", raw, err)
	}
	provider.accountingReply.Store(&geminiAccountingReply{status: "cancelled"})
	response, body := geminiPublic(t, h, http.MethodPost, "/gemini/v1beta/interactions/"+local+"/cancel", key, nil)
	if response.StatusCode != http.StatusOK || !json.Valid(body) {
		t.Fatalf("cancel: %d %s", response.StatusCode, body)
	}
	assertGeminiUsageSnapshot(t, h, original, false, 0, 0, 0)
}
