package main

import (
	"context"
	"log/slog"
	"slices"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/platform/workerhealth"
)

func (r *running) startRecoveryPlane() func() {
	var wg sync.WaitGroup
	ctx := r.workers
	for _, run := range slices.Concat(
		r.accountingWorkers(),
		r.codeWorkers(),
		r.inferenceWorkers(),
		r.operationsWorkers(),
		r.providerWorkers(),
	) {
		wg.Go(r.supervise(func() { run(ctx) }))
	}
	return wg.Wait
}

// probeCheckpoint records how one health probe pass ended in the shared
// worker health table.
func probeCheckpoint(pool *pgxpool.Pool, log *slog.Logger, region string) func(context.Context, int, error) {
	return func(ctx context.Context, probed int, err error) {
		outcome := workerhealth.OutcomeSuccess
		if err != nil {
			outcome = workerhealth.OutcomeFailure
			log.Warn("health probes failed", "error", err.Error())
		}
		if err := workerhealth.New(workerhealth.TaskHealthProbes, region).Checkpoint(ctx, pool, outcome, probed > 0); err != nil {
			log.Warn("health probe checkpoint failed", "error", err.Error())
		}
	}
}

// costCheckpoint records how one cost reconciliation pass ended in the shared
// worker health table. The limiter and the metadata plane keep their own
// outcome vocabularies, so the two are translated here rather than aliased.
func costCheckpoint(pool *pgxpool.Pool, region string) func(context.Context, ledger.Outcome, bool) error {
	return func(ctx context.Context, outcome ledger.Outcome, progress bool) error {
		recorded := workerhealth.OutcomeSuccess
		switch outcome {
		case ledger.OutcomeFailure:
			recorded = workerhealth.OutcomeFailure
		case ledger.OutcomeSkipped:
			recorded = workerhealth.OutcomeSkipped
		}
		return workerhealth.New(workerhealth.TaskCostReconciliation, region).Checkpoint(ctx, pool, recorded, progress)
	}
}
