package process

import (
	"context"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/usage"
)

// startWorkers runs the recovery plane of one replica: the consumer that turns
// emitted request metadata into accounting, the detector that turns an unclean
// gateway shutdown into a recorded completeness gap, the maintenance loop that
// rolls up and expires stored usage, and the cost reconciliation leader that
// republishes budget windows into the shared limiter. Each task supervises
// itself, checkpoints its own liveness, and stops when ctx is cancelled; the
// returned function waits for all of them to have stopped.
// vk is the consumer's own Valkey client: its blocking stream reads would
// otherwise stall every command the gateway sends on a shared connection.
func startWorkers(ctx context.Context, pool *pgxpool.Pool, vk *coordination.Client, limiter *limits.Limiter, stream string, mediaService *media.Service, log *slog.Logger) func() {
	var wg sync.WaitGroup
	if mediaService != nil {
		wg.Go(func() { mediaService.RunReconciler(ctx) })
	}
	if limiter == nil {
		return wg.Wait
	}
	wg.Go(func() {
		// The consumer identity is stable for this process and unique among
		// live ones, so a restart reclaims exactly its own deliveries.
		if err := usage.RunConsumer(ctx, pool, vk, stream, usage.ConsumerName(), limiter, log); err != nil {
			log.Error("request metadata consumer stopped", "error", err)
		}
	})
	wg.Go(func() { usage.RunEpochDetection(ctx, pool, log) })
	wg.Go(func() { usage.RunMaintenanceLoop(ctx, pool, log) })
	wg.Go(func() {
		connect := func(context.Context) (*limits.Limiter, error) { return limiter, nil }
		limits.RunCostReconciliation(ctx, pool, connect, costCheckpoint(pool), log)
	})
	return wg.Wait
}

// costCheckpoint records how one cost reconciliation pass ended in the shared
// worker health table. The limiter and the metadata plane keep their own
// outcome vocabularies, so the two are translated here rather than aliased.
func costCheckpoint(pool *pgxpool.Pool) func(context.Context, limits.Outcome, bool) error {
	return func(ctx context.Context, outcome limits.Outcome, progress bool) error {
		recorded := usage.OutcomeSuccess
		switch outcome {
		case limits.OutcomeFailure:
			recorded = usage.OutcomeFailure
		case limits.OutcomeSkipped:
			recorded = usage.OutcomeSkipped
		}
		return usage.CheckpointTask(ctx, pool, usage.TaskCostReconciliation, recorded, progress)
	}
}
