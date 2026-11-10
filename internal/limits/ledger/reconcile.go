package ledger

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/limits"
)

const (
	costReconciliationLockID  = int64(0x4f4c_505f_4352)
	reconciliationInterval    = time.Minute
	reconciliationPassTimeout = 2 * time.Minute
	leaderCloseTimeout        = 5 * time.Second
)

type Report struct {
	LockAcquired             bool
	KeysReconciled           int64
	DailyWindowsReconciled   int64
	MonthlyWindowsReconciled int64
}

type Leader struct{ conn *pgx.Conn }

func TryAcquireLeader(ctx context.Context, pool *pgxpool.Pool, region string) (*Leader, error) {
	acquired, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	conn := acquired.Hijack()
	var held bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", lockID(region)).Scan(&held); err != nil {
		closeLeaderConn(ctx, conn)
		return nil, err
	}
	if !held {
		closeLeaderConn(ctx, conn)
		return nil, nil
	}
	return &Leader{conn: conn}, nil
}

func lockID(region string) int64 {
	if region == "" {
		return costReconciliationLockID
	}
	digest := sha256.Sum256([]byte("openllmproxy:cost-reconciliation:" + region))
	return int64(binary.BigEndian.Uint64(digest[:8]))
}

func (l *Leader) Close(ctx context.Context) { closeLeaderConn(ctx, l.conn) }

func closeLeaderConn(ctx context.Context, conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaderCloseTimeout)
	defer cancel()
	conn.Close(ctx)
}

type Installer interface {
	InstallSnapshot(ctx context.Context, s limits.Snapshot) (limits.Installed, error)
}

func (l *Leader) Reconcile(ctx context.Context, installer Installer, now time.Time) (Report, error) {
	snapshots, err := Snapshots(ctx, l.conn, now)
	if err != nil {
		return Report{LockAcquired: true}, err
	}
	report, failures := Report{LockAcquired: true}, []error{}
	for _, snapshot := range snapshots {
		installed, err := installer.InstallSnapshot(ctx, snapshot)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if snapshot.Basis.Normalized() == limits.BasisCost {
			report = report.counting(installed)
		}
	}
	if len(failures) > 0 {
		return report, failures[0]
	}
	return report, l.conn.Ping(ctx)
}

func (r Report) counting(installed limits.Installed) Report {
	count := func(on bool) int64 {
		if on {
			return 1
		}
		return 0
	}
	return Report{
		LockAcquired:             r.LockAcquired,
		KeysReconciled:           r.KeysReconciled + count(installed.Daily || installed.Monthly),
		DailyWindowsReconciled:   r.DailyWindowsReconciled + count(installed.Daily),
		MonthlyWindowsReconciled: r.MonthlyWindowsReconciled + count(installed.Monthly),
	}
}

type Outcome int

const (
	OutcomeSuccess Outcome = iota
	OutcomeFailure
	OutcomeSkipped
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSuccess:
		return "success"
	case OutcomeFailure:
		return "failure"
	case OutcomeSkipped:
		return "skipped"
	default:
		return "unknown"
	}
}

type Worker struct {
	Pool       *pgxpool.Pool
	Region     string
	Connect    func(context.Context) (Installer, error)
	Checkpoint func(context.Context, Outcome, bool) error
	Log        *slog.Logger
}

func (w Worker) Run(ctx context.Context) {
	runner := &reconciliationRunner{Worker: w}
	defer runner.release(ctx)
	ticker := time.NewTicker(reconciliationInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		outcome, progress := runner.pass(ctx)
		if ctx.Err() != nil {
			return
		}
		if err := w.Checkpoint(ctx, outcome, progress); err != nil {
			w.Log.Warn("cost reconciliation checkpoint failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type reconciliationRunner struct {
	Worker
	leader *Leader
}

func (r *reconciliationRunner) pass(ctx context.Context) (Outcome, bool) {
	passCtx, cancel := context.WithTimeout(ctx, reconciliationPassTimeout)
	defer cancel()
	if r.leader == nil {
		leader, err := TryAcquireLeader(passCtx, r.Pool, r.Region)
		if err != nil {
			r.Log.Warn("cost reconciliation could not elect a leader", "error", err)
			return OutcomeFailure, false
		}
		if leader == nil {
			return OutcomeSkipped, false
		}
		r.leader = leader
	}
	installer, err := r.Connect(passCtx)
	if err != nil {
		r.drop(ctx, "cost reconciliation could not reach Valkey", err)
		return OutcomeFailure, false
	}
	report, err := r.leader.Reconcile(passCtx, installer, time.Now())
	if err != nil {
		r.drop(ctx, "cost reconciliation pass failed", err)
		return OutcomeFailure, false
	}
	if report.KeysReconciled > 0 {
		r.Log.Info("reconciled cost budgets",
			"keys", report.KeysReconciled,
			"daily_windows", report.DailyWindowsReconciled,
			"monthly_windows", report.MonthlyWindowsReconciled)
	}
	return OutcomeSuccess, report.KeysReconciled > 0
}

func (r *reconciliationRunner) drop(ctx context.Context, message string, err error) {
	r.Log.Warn(message, "error", err)
	r.release(ctx)
}

func (r *reconciliationRunner) release(ctx context.Context) {
	if r.leader == nil {
		return
	}
	r.leader.Close(ctx)
	r.leader = nil
}
