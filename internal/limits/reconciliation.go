package limits

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// costReconciliationLockID spells "OLP_CR". One PostgreSQL session-level
	// advisory lock elects the single replica allowed to rewrite cost state.
	costReconciliationLockID = int64(0x4f4c_505f_4352)
	reconciliationInterval   = time.Minute
	// A pass that cannot finish in two minutes has lost its grip on the lock's
	// session; it is abandoned so another replica can take over.
	reconciliationPassTimeout = 2 * time.Minute
	leaderCloseTimeout        = 5 * time.Second
)

// Report counts what one reconciliation pass installed in Valkey.
type Report struct {
	// LockAcquired is false when another replica held leadership.
	LockAcquired             bool
	KeysReconciled           int64
	DailyWindowsReconciled   int64
	MonthlyWindowsReconciled int64
}

// Leader owns the PostgreSQL session that holds the reconciliation lock. The
// caller must Close it, which releases the lock for the next replica.
type Leader struct{ conn *pgx.Conn }

// TryAcquireLeader elects this replica, returning a nil Leader and a nil error
// when another replica already holds the lock.
func TryAcquireLeader(ctx context.Context, pool *pgxpool.Pool) (*Leader, error) {
	acquired, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	// The lock belongs to the session, not to the pool: hijacking the
	// connection takes it out of the pool for good, so it can never be handed
	// to an unrelated caller while it still holds the lock, and releases the
	// lock exactly when this leader is closed.
	conn := acquired.Hijack()
	var held bool
	if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", costReconciliationLockID).Scan(&held); err != nil {
		closeLeaderConn(ctx, conn)
		return nil, err
	}
	if !held {
		closeLeaderConn(ctx, conn)
		return nil, nil
	}
	return &Leader{conn: conn}, nil
}

// Close releases the advisory lock by ending its session. It runs even when the
// caller's context is already cancelled, because a lock left behind would block
// every replica until the session times out.
func (l *Leader) Close(ctx context.Context) { closeLeaderConn(ctx, l.conn) }

func closeLeaderConn(ctx context.Context, conn *pgx.Conn) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaderCloseTimeout)
	defer cancel()
	conn.Close(ctx)
}

// Reconcile recomputes every active key's durable spend and installs it in
// Valkey. A key whose Valkey state cannot be repaired does not stop the others:
// the first failure is returned once the rest have been attempted, and the
// caller must then Close this leader because the pass is no longer trustworthy.
func (l *Leader) Reconcile(ctx context.Context, limiter *Limiter, now time.Time) (Report, error) {
	report := Report{LockAcquired: true}
	snapshots, err := ReconciliationSnapshots(ctx, l.conn, now)
	if err != nil {
		return report, err
	}
	var failure error
	for _, snapshot := range snapshots {
		daily, monthly, err := limiter.ApplyCostSnapshot(ctx, snapshot)
		if err != nil {
			if failure == nil {
				failure = err
			}
			continue
		}
		if daily || monthly {
			report.KeysReconciled++
		}
		if daily {
			report.DailyWindowsReconciled++
		}
		if monthly {
			report.MonthlyWindowsReconciled++
		}
	}
	if failure != nil {
		return report, failure
	}
	// A pass cannot report success after losing the session that owns its lock:
	// another replica may have reconciled the same keys in the meantime.
	if err := l.conn.Ping(ctx); err != nil {
		return report, err
	}
	return report, nil
}

// Outcome is how one reconciliation pass ended, as recorded in the worker
// health checkpoint.
type Outcome int

// The outcomes a reconciliation pass reports.
const (
	OutcomeSuccess Outcome = iota
	OutcomeFailure
	OutcomeSkipped
)

// String names the outcome for checkpoints and logs.
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

// RunCostReconciliation reconciles cost budgets once a minute until ctx is
// done. The first pass runs immediately: until PostgreSQL has installed the
// current windows, every cost-limited request fails closed on uninitialised
// state, so a freshly started replica must not wait a minute to publish them.
// Leadership is acquired lazily and then held, so the elected replica keeps
// reconciling without contending for the lock every minute, while the others
// skip cheaply. checkpoint records each pass; its second argument reports
// whether the pass changed anything.
func RunCostReconciliation(ctx context.Context, pool *pgxpool.Pool, connect func(context.Context) (*Limiter, error), checkpoint func(context.Context, Outcome, bool) error, log *slog.Logger) {
	runner := &reconciliationRunner{pool: pool, connect: connect, log: log}
	defer runner.release(ctx)
	ticker := time.NewTicker(reconciliationInterval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		outcome, progress := runner.pass(ctx)
		// A pass that ended because the process is shutting down has nothing
		// worth recording, and the checkpoint write would fail anyway.
		if ctx.Err() != nil {
			return
		}
		if err := checkpoint(ctx, outcome, progress); err != nil {
			log.Warn("cost reconciliation checkpoint failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

type reconciliationRunner struct {
	pool    *pgxpool.Pool
	connect func(context.Context) (*Limiter, error)
	log     *slog.Logger
	leader  *Leader
}

// pass runs one bounded reconciliation and reports the outcome to checkpoint.
func (r *reconciliationRunner) pass(ctx context.Context) (Outcome, bool) {
	passCtx, cancel := context.WithTimeout(ctx, reconciliationPassTimeout)
	defer cancel()
	if r.leader == nil {
		leader, err := TryAcquireLeader(passCtx, r.pool)
		if err != nil {
			r.log.Warn("cost reconciliation could not elect a leader", "error", err)
			return OutcomeFailure, false
		}
		if leader == nil {
			return OutcomeSkipped, false
		}
		r.leader = leader
	}
	limiter, err := r.connect(passCtx)
	if err != nil {
		r.drop(ctx, "cost reconciliation could not reach Valkey", err)
		return OutcomeFailure, false
	}
	report, err := r.leader.Reconcile(passCtx, limiter, time.Now())
	if err != nil {
		r.drop(ctx, "cost reconciliation pass failed", err)
		return OutcomeFailure, false
	}
	if report.KeysReconciled > 0 {
		r.log.Info("reconciled cost budgets",
			"keys", report.KeysReconciled,
			"daily_windows", report.DailyWindowsReconciled,
			"monthly_windows", report.MonthlyWindowsReconciled)
	}
	return OutcomeSuccess, report.KeysReconciled > 0
}

// drop gives leadership up after a failed or timed-out pass: the session may be
// unusable, and another replica must be able to take over immediately.
func (r *reconciliationRunner) drop(ctx context.Context, message string, err error) {
	r.log.Warn(message, "error", err)
	r.release(ctx)
}

func (r *reconciliationRunner) release(ctx context.Context) {
	if r.leader == nil {
		return
	}
	r.leader.Close(ctx)
	r.leader = nil
}
