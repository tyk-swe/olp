package observability

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type failingRow struct{ err error }

func (r failingRow) Scan(...any) error { return r.err }

// failingQuerier fails every statement, as a store does when the refresh
// deadline has expired.
type failingQuerier struct{ err error }

func (q failingQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	return failingRow{err: q.err}
}

func (q failingQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, q.err
}

func (q failingQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, q.err
}

func TestMetricsOmitRecoveryCountersWhenTheirReadFails(t *testing.T) {
	body, err := CollectMetrics(context.Background(), &State{
		Pool: failingQuerier{err: context.DeadlineExceeded},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "olp_async_worker_observability_available 0\n") {
		t.Fatalf("failed reads must report unavailable:\n%s", body)
	}
	for _, series := range []string{
		"olp_request_metadata_events_reclaimed_total",
		"olp_request_metadata_events_recovered_total",
		"olp_request_metadata_persistence_duplicates_total",
		"olp_request_metadata_events_processed_total",
	} {
		if strings.Contains(body, series) {
			t.Fatalf("counter %s must be omitted, not reset to zero:\n%s", series, body)
		}
	}
}

func TestRecoveryCountersRenderStoredValues(t *testing.T) {
	var body strings.Builder
	writeRecoveryCounters(&body, WorkerRecoveryCounters{RequestMetadataReclaimed: 5000})
	if !strings.Contains(body.String(), "olp_request_metadata_events_reclaimed_total 5000\n") {
		t.Fatalf("counters:\n%s", body.String())
	}
}

func TestProviderMetricLabelsAreEscapedOnce(t *testing.T) {
	var body strings.Builder
	writeProviderMetrics(&body, []ProviderHealthRecord{{
		ProviderID:   "p1",
		ProviderName: "Acme \"EU\"\t\\x\u200b\nnext",
		ProviderKind: "openai",
		Status:       "healthy",
	}})
	want := `olp_provider_health{provider_id="p1",provider_name="Acme \"EU\"` + "\t" + `\\x` + "\u200b" + `\nnext",provider_kind="openai",status="healthy"} 1` + "\n"
	if !strings.Contains(body.String(), want) {
		t.Fatalf("want %q in:\n%s", want, body.String())
	}
}
