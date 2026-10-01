package observability

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/media"
)

func TestReadinessMediaReconciliationReflectsDegradation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		summary *media.Summary
		gaps    int64
		want    string
	}{
		{"healthy", &media.Summary{Pending: 2}, 0, "ok"},
		{"failed", &media.Summary{Failed: 3}, 0, "degraded"},
		{"stale", &media.Summary{Stale: 1}, 0, "degraded"},
		{"gaps", &media.Summary{}, 1, "degraded"},
		{"unread", nil, 0, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := &storeProbe{database: "ok", media: tc.summary, tasks: &WorkerTaskHealth{}}
			degraded := mediaDegraded(tc.summary, tc.gaps)
			h := readinessResponse(&State{}, time.Now(), nil, probe, nil,
				true, true, true, false, degraded, false, false, tc.gaps)
			if h.MediaReconciliation != tc.want {
				t.Fatalf("media_reconciliation = %q, want %q", h.MediaReconciliation, tc.want)
			}
			if degraded && h.Status != "degraded" {
				t.Fatalf("status = %q, want degraded", h.Status)
			}
		})
	}
}
