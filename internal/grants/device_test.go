package grants

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/plugin/abi"
)

type recordedExec struct {
	sql         string
	args        []any
	ctxErr      error
	deadline    time.Time
	hasDeadline bool
}

type fakeSettleTx struct {
	pgx.Tx
	responses []execResponse
	calls     []recordedExec
}

type execResponse struct {
	tag pgconn.CommandTag
	err error
}

func (f *fakeSettleTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	index := len(f.calls)
	deadline, hasDeadline := ctx.Deadline()
	f.calls = append(f.calls, recordedExec{sql: sql, args: args, ctxErr: ctx.Err(), deadline: deadline, hasDeadline: hasDeadline})
	if index >= len(f.responses) {
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return f.responses[index].tag, f.responses[index].err
}

func TestEnrollmentSettle(t *testing.T) {
	t.Parallel()

	t.Run("authorization_pending keeps enrollment pending without deleting secret", func(t *testing.T) {
		t.Parallel()
		tx := &fakeSettleTx{responses: []execResponse{{tag: pgconn.NewCommandTag("UPDATE 1")}}}
		enrollment := Enrollment{ID: "enroll-1", interval: 10 * time.Second}
		standing, err := enrollment.Settle(t.Context(), tx, &abi.Error{Code: abi.CodeAuthorizationPending, Message: "wait"}, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if standing.Status != Pending || standing.Interval != 10*time.Second || standing.Ended || standing.Failure != nil {
			t.Fatalf("unexpected standing: %+v", standing)
		}
		if len(tx.calls) != 1 || !strings.Contains(tx.calls[0].sql, "poll_interval=$2") {
			t.Fatalf("unexpected SQL calls: %+v", tx.calls)
		}
		if len(tx.calls[0].args) != 2 || tx.calls[0].args[0] != "enroll-1" || tx.calls[0].args[1] != int64(10) {
			t.Fatalf("unexpected args: %+v", tx.calls[0].args)
		}
	})

	t.Run("slow_down grows interval and caps at maxInterval", func(t *testing.T) {
		t.Parallel()
		tx := &fakeSettleTx{responses: []execResponse{{tag: pgconn.NewCommandTag("UPDATE 1")}}}
		enrollment := Enrollment{ID: "enroll-2", interval: 10 * time.Second}
		standing, err := enrollment.Settle(t.Context(), tx, &abi.Error{Code: abi.CodeSlowDown, Message: "slower"}, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if standing.Status != Pending || standing.Interval != 15*time.Second || standing.Ended || standing.Failure != nil {
			t.Fatalf("unexpected standing: %+v", standing)
		}
		if len(tx.calls) != 1 || tx.calls[0].args[1] != int64(15) {
			t.Fatalf("unexpected SQL calls: %+v", tx.calls)
		}

		cappedTx := &fakeSettleTx{responses: []execResponse{{tag: pgconn.NewCommandTag("UPDATE 1")}}}
		capped := Enrollment{ID: "enroll-2b", interval: maxInterval}
		cappedStanding, err := capped.Settle(t.Context(), cappedTx, &abi.Error{Code: abi.CodeSlowDown}, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cappedStanding.Interval != maxInterval {
			t.Fatalf("interval = %v, want %v", cappedStanding.Interval, maxInterval)
		}
	})

	t.Run("interrupted poll resets terminal failure to pending under caller deadline", func(t *testing.T) {
		t.Parallel()
		wantDeadline := time.Now().Add(15 * time.Second)
		settleCtx, cancel := context.WithDeadline(t.Context(), wantDeadline)
		defer cancel()
		tx := &fakeSettleTx{responses: []execResponse{{tag: pgconn.NewCommandTag("UPDATE 1")}}}
		enrollment := Enrollment{ID: "enroll-3", interval: 10 * time.Second}
		standing, err := enrollment.Settle(settleCtx, tx, &abi.Error{Code: "server_error", Message: "aborted"}, true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if standing.Status != Pending || standing.Interval != 10*time.Second || standing.Ended || standing.Failure != nil {
			t.Fatalf("unexpected standing: %+v", standing)
		}
		if len(tx.calls) != 1 {
			t.Fatalf("expected 1 SQL call without secret delete, got %d", len(tx.calls))
		}
		if tx.calls[0].ctxErr != nil {
			t.Fatalf("tx.Exec saw canceled context: %v", tx.calls[0].ctxErr)
		}
		if !tx.calls[0].hasDeadline || !tx.calls[0].deadline.Equal(wantDeadline) {
			t.Fatalf("tx.Exec deadline = (%v, %t), want (%v, true)", tx.calls[0].deadline, tx.calls[0].hasDeadline, wantDeadline)
		}
	})

	t.Run("access_denied ends enrollment and deletes secret under caller deadline", func(t *testing.T) {
		t.Parallel()
		wantDeadline := time.Now().Add(15 * time.Second)
		settleCtx, cancel := context.WithDeadline(t.Context(), wantDeadline)
		defer cancel()
		tx := &fakeSettleTx{responses: []execResponse{
			{tag: pgconn.NewCommandTag("UPDATE 1")},
			{tag: pgconn.NewCommandTag("DELETE 1")},
		}}
		enrollment := Enrollment{ID: "enroll-4", interval: 5 * time.Second}
		standing, err := enrollment.Settle(settleCtx, tx, &abi.Error{Code: abi.CodeAccessDenied, Message: "denied"}, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if standing.Status != Denied || !standing.Ended || standing.Failure != nil {
			t.Fatalf("unexpected standing: %+v", standing)
		}
		assertEndedSecretDelete(t, tx.calls, "outcome='denied'", "enroll-4")
		for i, call := range tx.calls {
			if !call.hasDeadline || !call.deadline.Equal(wantDeadline) {
				t.Fatalf("call %d deadline = (%v, %t), want (%v, true)", i, call.deadline, call.hasDeadline, wantDeadline)
			}
		}
	})

	t.Run("expired_token ends enrollment and deletes secret", func(t *testing.T) {
		t.Parallel()
		tx := &fakeSettleTx{responses: []execResponse{
			{tag: pgconn.NewCommandTag("UPDATE 1")},
			{tag: pgconn.NewCommandTag("DELETE 1")},
		}}
		enrollment := Enrollment{ID: "enroll-5", interval: 5 * time.Second}
		standing, err := enrollment.Settle(t.Context(), tx, &abi.Error{Code: abi.CodeExpiredToken, Message: "expired"}, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if standing.Status != Expired || !standing.Ended || standing.Failure != nil {
			t.Fatalf("unexpected standing: %+v", standing)
		}
		assertEndedSecretDelete(t, tx.calls, "outcome='expired'", "enroll-5")
	})

	t.Run("terminal abi error ends enrollment and returns Standing.Failure with nil error", func(t *testing.T) {
		t.Parallel()
		tx := &fakeSettleTx{responses: []execResponse{
			{tag: pgconn.NewCommandTag("UPDATE 1")},
			{tag: pgconn.NewCommandTag("DELETE 1")},
		}}
		enrollment := Enrollment{ID: "enroll-6", interval: 5 * time.Second}
		standing, err := enrollment.Settle(t.Context(), tx, &abi.Error{Code: "server_error", Message: "upstream refused"}, false)
		if err != nil {
			t.Fatalf("expected nil tx error so caller commits, got: %v", err)
		}
		if !standing.Ended || standing.Status != "" {
			t.Fatalf("unexpected standing: %+v", standing)
		}
		problem, ok := errors.AsType[*httpkit.Problem](standing.Failure)
		if !ok || problem.Status != 422 || problem.Code != "grant_enrollment_failed" {
			t.Fatalf("unexpected Standing.Failure: %#v", standing.Failure)
		}
		assertEndedSecretDelete(t, tx.calls, "continued_at=now()", "enroll-6")
	})

	t.Run("terminal non-abi error preserves problem on Standing.Failure", func(t *testing.T) {
		t.Parallel()
		tx := &fakeSettleTx{responses: []execResponse{
			{tag: pgconn.NewCommandTag("UPDATE 1")},
			{tag: pgconn.NewCommandTag("DELETE 1")},
		}}
		stale := httpkit.Fail(409, "grant_enrollment_stale", "slot changed")
		enrollment := Enrollment{ID: "enroll-7", interval: 5 * time.Second}
		standing, err := enrollment.Settle(t.Context(), tx, stale, false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !standing.Ended || !errors.Is(standing.Failure, stale) {
			t.Fatalf("unexpected standing: %+v", standing)
		}
		assertEndedSecretDelete(t, tx.calls, "continued_at=now()", "enroll-7")
	})

	t.Run("cancelled enrollment returns pgx.ErrNoRows without deleting secret", func(t *testing.T) {
		t.Parallel()
		tx := &fakeSettleTx{responses: []execResponse{{tag: pgconn.NewCommandTag("UPDATE 0")}}}
		enrollment := Enrollment{ID: "enroll-8", interval: 5 * time.Second}
		standing, err := enrollment.Settle(t.Context(), tx, &abi.Error{Code: abi.CodeAccessDenied}, false)
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("err = %v, want pgx.ErrNoRows", err)
		}
		if standing != (Standing{}) {
			t.Fatalf("standing = %+v, want zero", standing)
		}
		if len(tx.calls) != 1 {
			t.Fatalf("expected 1 SQL call, got %d", len(tx.calls))
		}
	})

	t.Run("secret deletion failure propagates error and returns zero standing", func(t *testing.T) {
		t.Parallel()
		deleteErr := errors.New("delete failed")
		tx := &fakeSettleTx{responses: []execResponse{
			{tag: pgconn.NewCommandTag("UPDATE 1")},
			{err: deleteErr},
		}}
		enrollment := Enrollment{ID: "enroll-9", interval: 5 * time.Second}
		standing, err := enrollment.Settle(t.Context(), tx, &abi.Error{Code: abi.CodeExpiredToken}, false)
		if !errors.Is(err, deleteErr) {
			t.Fatalf("err = %v, want %v", err, deleteErr)
		}
		if standing != (Standing{}) {
			t.Fatalf("standing = %+v, want zero", standing)
		}
	})
}

func assertEndedSecretDelete(t *testing.T, calls []recordedExec, wantUpdateSnippet, wantID string) {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("expected 2 SQL calls (update + delete secret), got %d: %+v", len(calls), calls)
	}
	if !strings.Contains(calls[0].sql, wantUpdateSnippet) {
		t.Fatalf("update SQL %q missing %q", calls[0].sql, wantUpdateSnippet)
	}
	if !strings.Contains(calls[1].sql, "DELETE FROM olp.secrets") {
		t.Fatalf("second SQL %q did not delete grant enrollment secret", calls[1].sql)
	}
	if len(calls[1].args) != 2 || calls[1].args[0] != wantID || calls[1].args[1] != secrets.GrantEnrollment {
		t.Fatalf("unexpected delete args: %+v", calls[1].args)
	}
}
