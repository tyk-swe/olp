package reconcile

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	concurrency      = 4
	passInterval     = 5 * time.Second
	leaseHeartbeat   = 30 * time.Second
	reconcileTimeout = 5 * time.Minute
)

type Deps struct {
	Pool        *pgxpool.Pool
	Store       *batches.Store
	Resources   *resources.Store
	Resolver    *resources.Resolver
	Credentials release.Credentials
	DigestKey   *secrets.AuthKey
	Readers     upstream.Readers
	Egress      *egress.Policy
	Client      *http.Client
	Connections *egress.ConnectionClientCache
	Limiter     *limits.Limiter
	Log         *slog.Logger
	Now         func() time.Time
}

type Worker struct {
	deps      Deps
	health    workerhealth.Reporter
	gapDigest []byte
}

func New(deps Deps) *Worker {
	return &Worker{
		deps:      deps,
		health:    workerhealth.New(workerhealth.TaskBatchSettlement, ""),
		gapDigest: batches.CustomIDDigest(deps.DigestKey, "*"),
	}
}

type outcome int

const (
	outcomeProgress outcome = iota
	outcomeUncertain
	outcomeFailed
)

type pass struct {
	claimed, progressed, uncertain, failed int
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(passInterval)
	defer ticker.Stop()
	for {
		w.runPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) runPass(ctx context.Context) {
	result, err := w.reconcileOnce(ctx)
	if err != nil {
		w.deps.Log.Warn("batch settlement pass failed", "error", err)
	}
	health := workerhealth.OutcomeSuccess
	if err != nil || result.failed > 0 {
		health = workerhealth.OutcomeFailure
	}
	if err := w.health.Checkpoint(ctx, w.deps.Pool, health, result.claimed > 0); err != nil {
		w.deps.Log.Debug("batch settlement checkpoint failed", "error", err)
	}
	if result.claimed > 0 {
		w.deps.Log.Info("batch settlement pass completed", "claimed", result.claimed,
			"progressed", result.progressed, "uncertain", result.uncertain, "failed", result.failed)
	}
}

func (w *Worker) reconcileOnce(ctx context.Context) (pass, error) {
	claims, err := w.deps.Store.Claim(ctx, w.deps.Now(), concurrency)
	if err != nil {
		return pass{}, err
	}
	outcomes := make([]outcome, len(claims))
	var wg sync.WaitGroup
	for i, settlement := range claims {
		wg.Go(func() { outcomes[i] = w.reconcileClaimed(ctx, settlement) })
	}
	wg.Wait()
	return tally(outcomes), nil
}

func tally(outcomes []outcome) pass {
	count := func(want outcome) int {
		n := 0
		for _, got := range outcomes {
			if got == want {
				n++
			}
		}
		return n
	}
	return pass{claimed: len(outcomes), progressed: count(outcomeProgress),
		uncertain: count(outcomeUncertain), failed: count(outcomeFailed)}
}

func (w *Worker) reconcileClaimed(ctx context.Context, settlement batches.Settlement) outcome {
	leaseCtx, stopLease := w.keepLease(ctx, settlement)
	defer stopLease()
	reader, ok := w.deps.Readers.For(settlement.Adapter)
	if !ok {
		return w.failOrClose(leaseCtx, settlement, errorAdapterUnavailable)
	}
	switch settlement.State {
	case batches.StateSubmitting:
		return w.awaitCreator(leaseCtx, settlement)
	case batches.StateConflict:
		return w.conflict(leaseCtx, settlement)
	}
	target, err := w.target(leaseCtx, settlement)
	if err != nil {
		return w.failOrClose(leaseCtx, settlement, targetFailure(err))
	}
	if settlement.State == batches.StateSubmissionUnknown {
		return w.reconcileSubmission(leaseCtx, settlement, reader, target)
	}
	snapshot, err := reader.Poll(leaseCtx, settlement, target)
	if err != nil {
		return w.failure(leaseCtx, settlement, err, errorPollUnavailable)
	}
	return w.reconcileActive(leaseCtx, settlement, reader, target, snapshot)
}

func (w *Worker) awaitCreator(ctx context.Context, settlement batches.Settlement) outcome {
	if w.deps.Now().Before(settlement.CreatorDeadline) {
		return w.finish(ctx, settlement, batches.StateSubmitting, settlement.CreatorDeadline, "")
	}
	return w.finish(ctx, settlement, batches.StateSubmissionUnknown, w.deps.Now(), errorSubmissionInterrupted)
}

func (w *Worker) keepLease(ctx context.Context, settlement batches.Settlement) (context.Context, func()) {
	leaseCtx, cancel := context.WithTimeout(ctx, reconcileTimeout)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(leaseHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
			}
			if err := w.deps.Store.ExtendClaim(leaseCtx, settlement, w.deps.Now()); err != nil {
				w.deps.Log.Warn("batch settlement lease renewal failed", "resource_id", settlement.ResourceID, "error", err)
				cancel()
				return
			}
			if err := w.health.Checkpoint(leaseCtx, w.deps.Pool, workerhealth.OutcomeSuccess, false); err != nil {
				w.deps.Log.Debug("batch settlement heartbeat failed", "error", err)
			}
		}
	}()
	return leaseCtx, func() {
		cancel()
		<-done
	}
}
