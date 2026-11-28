package main

import (
	"context"
	"slices"

	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/sinks"
	"github.com/tyk-swe/olp/internal/usage/completeness"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

func (r *running) accountingWorkers() []func(context.Context) {
	pool, core := r.stores.pool, r.core
	managedExporter := &sinks.Worker{Pool: pool, Keys: core.keys, Installation: r.stores.installation, Egress: core.policy}
	return slices.Concat([]func(context.Context){
		func(ctx context.Context) { managedExporter.Run(ctx, r.log) },
		core.exporter.Run,
		core.focusWorker.Run,
		core.importer.Run,
		func(ctx context.Context) {
			runDebitReconciliation(ctx, pool, r.stores.debitStore, r.stores.limitState().Syncer, r.log)
		},
		func(ctx context.Context) { recoverWorkflowModels(ctx, r.stores.debitStore, r.log) },
		func(ctx context.Context) { completeness.RunEpochDetection(ctx, pool, r.log) },
	}, r.sharedStateAccountingWorkers())
}

func (r *running) sharedStateAccountingWorkers() []func(context.Context) {
	limiter := r.stores.limiter
	if limiter == nil {
		return nil
	}
	pool, region := r.stores.pool, r.config.Region
	return []func(context.Context){
		func(ctx context.Context) {
			consumer := ingest.Consumer{Pool: pool, Client: r.feed.reader, Stream: r.feed.stream, Name: ingest.ConsumerName(r.config.Hostname), Region: region, Limiter: limiter, Log: r.log}
			if err := consumer.Run(ctx); err != nil {
				r.log.Error("request metadata consumer stopped", "error", err)
			}
		},
		func(ctx context.Context) {
			connect := func(context.Context) (ledger.Installer, error) { return limiter, nil }
			ledger.Worker{Pool: pool, Region: region, Connect: connect, Checkpoint: costCheckpoint(pool, region), Log: r.log}.Run(ctx)
		},
	}
}
