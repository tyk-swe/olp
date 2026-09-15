package usage

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

type recordingHealthExecer struct {
	calls [][]any
	sql   string
	err   error
}

func (r *recordingHealthExecer) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	r.sql = sql
	r.calls = append(r.calls, args)
	return pgconn.CommandTag{}, r.err
}

func TestStreamNameIsNamespaced(t *testing.T) {
	if got := StreamName("olp:go:v1:install:"); got != "olp:go:v1:install:request-metadata" {
		t.Errorf("stream = %s", got)
	}
	if StreamName("a:") == StreamName("b:") {
		t.Error("two installations must not share a stream")
	}
}

func TestCheckpointTaskRecordsOneOutcome(t *testing.T) {
	cases := []struct {
		name      string
		outcome   Outcome
		progress  bool
		success   bool
		successes int64
		failures  int64
		skipped   int64
	}{
		{name: "success with progress", outcome: OutcomeSuccess, progress: true, success: true, successes: 1},
		{name: "success without progress", outcome: OutcomeSuccess, success: true, successes: 1},
		{name: "failure", outcome: OutcomeFailure, failures: 1},
		{name: "skipped", outcome: OutcomeSkipped, skipped: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			execer := &recordingHealthExecer{}
			if err := CheckpointTask(context.Background(), execer, TaskMaintenance, tc.outcome, tc.progress); err != nil {
				t.Fatalf("checkpoint: %v", err)
			}
			if len(execer.calls) != 1 {
				t.Fatalf("calls = %d, want 1", len(execer.calls))
			}
			want := []any{string(TaskMaintenance), tc.success, tc.progress, tc.successes, tc.failures, tc.skipped}
			for index, argument := range want {
				if execer.calls[0][index] != argument {
					t.Errorf("argument %d = %v, want %v", index+1, execer.calls[0][index], argument)
				}
			}
			if !strings.Contains(execer.sql, "olp_go.worker_task_health") {
				t.Errorf("sql = %s", execer.sql)
			}
		})
	}
}

func TestCheckpointTaskRejectsAnUnknownOutcome(t *testing.T) {
	execer := &recordingHealthExecer{}
	if err := CheckpointTask(context.Background(), execer, TaskMaintenance, Outcome(7), false); err == nil {
		t.Fatal("accepted an unknown outcome")
	}
	if len(execer.calls) != 0 {
		t.Error("an unknown outcome must not write")
	}
}

func TestCheckpointTaskReportsWriteFailures(t *testing.T) {
	failure := errors.New("connection lost")
	execer := &recordingHealthExecer{err: failure}
	err := CheckpointTask(context.Background(), execer, TaskEpochDetection, OutcomeSuccess, true)
	if !errors.Is(err, failure) {
		t.Fatalf("err = %v, want the write failure", err)
	}
}

func TestReportConsumerActivityIgnoresAnIdlePass(t *testing.T) {
	// A nil pool proves the idle pass never reaches the database.
	if err := ReportConsumerActivity(context.Background(), nil, Activity{}); err != nil {
		t.Fatalf("idle pass: %v", err)
	}
}

func TestConsumerNameIsBoundedAndSafe(t *testing.T) {
	epoch := uuid.MustParse("0195f3c2-6a1e-7c8d-9e0f-1a2b3c4d5e6f")
	simple := "0195f3c26a1e7c8d9e0f1a2b3c4d5e6f"
	cases := []struct {
		name string
		host string
		want string
	}{
		{name: "sanitized host", host: "GW.Node_1", want: "gw-node_1-42-" + simple},
		{name: "missing host", host: "", want: "olp-42-" + simple},
		{name: "non-ascii host", host: "网关", want: "---42-" + simple},
		{name: "long host", host: strings.Repeat("h", 60), want: strings.Repeat("h", 48) + "-42-" + simple},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := consumerName(tc.host, 42, epoch); got != tc.want {
				t.Errorf("name = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestConsumerNameIsUniquePerProcess(t *testing.T) {
	t.Setenv("HOSTNAME", "gw-1")
	first, second := ConsumerName(), ConsumerName()
	if first == second {
		t.Error("two processes on one host would share a consumer name")
	}
	if !strings.HasPrefix(first, "gw-1-") {
		t.Errorf("name = %s, want the host as its prefix", first)
	}
}

func TestGatewayInstanceIsBoundedAndLogSafe(t *testing.T) {
	cases := []struct {
		name string
		host string
		want string
	}{
		{name: "plain host", host: "gateway-1", want: "gateway-1"},
		{name: "missing host", host: "", want: "olp"},
		{name: "blank host", host: "  \t ", want: "olp"},
		{name: "control characters", host: "gate\x00way\n1", want: "gateway1"},
		{name: "only control characters", host: "\x00\x07", want: "olp"},
		{name: "unicode host", host: "gateway-ü", want: "gateway-ü"},
		{name: "long host", host: strings.Repeat("g", 260), want: strings.Repeat("g", 200)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gatewayInstanceLabel(tc.host)
			if got != tc.want {
				t.Errorf("instance = %q, want %q", got, tc.want)
			}
			if len(got) > 200 {
				t.Errorf("instance is %d bytes, want at most 200", len(got))
			}
		})
	}
}

func TestGatewayInstanceReadsTheHostname(t *testing.T) {
	t.Setenv("HOSTNAME", " gateway-2 ")
	first, second := GatewayInstance(), GatewayInstance()
	if !strings.HasPrefix(first, "gateway-2-") || first == second || len(first) > 200 {
		t.Errorf("process labels = %q / %q, want distinct bounded host labels", first, second)
	}
}

func TestGatewayInstanceTruncatesOnARuneBoundary(t *testing.T) {
	got := gatewayInstanceLabel(strings.Repeat("é", 150))
	if len(got) > 200 {
		t.Fatalf("instance is %d bytes, want at most 200", len(got))
	}
	if strings.ContainsRune(got, '�') || len(got)%2 != 0 {
		t.Errorf("instance = %q, want whole runes", got)
	}
}
