//go:build integration

package integration_test

import (
	"fmt"
	"net/http"
	"testing"
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

// Another gateway repeats an older native checkpoint. It must not
// reduce the original request's known consumption before cleanup.

// A later complete replay may return different totals, but the
// removed pending owner cannot create or amend another event.
