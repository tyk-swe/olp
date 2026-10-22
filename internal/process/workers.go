package process

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/billing"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/export"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/sinks"
	"github.com/tyk-swe/olp/internal/usage"
)

// startWorkers runs the recovery plane of one replica: the media and batch
// reconcilers that poll accepted provider work, the grant refresher that renews
// grants' access tokens through their plugins, the consumer that turns emitted
// request metadata into accounting, the detector that turns an unclean
// gateway shutdown into a recorded completeness gap, the maintenance loop that
// rolls up and expires stored usage, and the cost reconciliation leader that
// republishes budget windows into the shared limiter. Export delivery drains
// the durable queue, the FOCUS worker maintains its dataset ledger, and the
// independent billing importer polls provider cost reports. These three tasks
// need PostgreSQL and egress, not the coordination store. Each task supervises
// itself, checkpoints its own liveness, and stops when ctx is cancelled; the
// returned function waits for all of them to have stopped.
// vk is the consumer's own Valkey client: its blocking stream reads would
// otherwise stall every command the gateway sends on a shared connection.
func startWorkers(ctx context.Context, pool *pgxpool.Pool, vk *coordination.Client, limiter *limits.Limiter, stream string, mediaService *media.Service, pluginHost *plugins.Host, keys *secrets.KeyRing, auth *secrets.AuthKey, installation, region string, policy *egress.Policy, log *slog.Logger, exporter *export.Worker, focusWorker *export.FocusWorker, importer *billing.Importer, reference *catalog.Signed) func() {
	ctx = usage.WithWorkerRegion(ctx, region)
	var wg sync.WaitGroup
	managedExporter := &sinks.Worker{Pool: pool, Keys: keys, Installation: installation, Egress: policy}
	wg.Go(func() { managedExporter.Run(ctx, log) })
	wg.Go(func() { mediaService.RunReconciler(ctx) })
	batchAuth := connectors.NewAuth(policy)
	batchAuth.Signer = pluginHost
	batchWorker := &batches.Worker{
		Pool: pool, Store: batches.NewStore(pool),
		Resources: resources.NewEncrypted(pool, installation, keys),
		Resolver:  resources.NewResolver(pool), Credentials: mediaService.Credentials,
		Keys: keys, Installation: installation, DigestKey: auth,
		Readers: map[string]batches.Reader{
			"openai":   &batches.OpenAIReader{Auth: batchAuth, Egress: policy},
			"together": batches.NewTogetherReader(batchAuth, policy),
			"mistral":  batches.NewMistralReader(batchAuth, policy),
		},
		Auth: batchAuth, Egress: policy,
		Client: policy.Client(5 * time.Minute), Connections: egress.NewConnectionClientCache(128),
		Limiter: limiter, Log: log,
	}
	wg.Go(func() { batchWorker.Run(ctx) })
	// Grant refresh needs only PostgreSQL and the providers' network paths.
	refresher := &grants.Refresher{Pool: pool, Keys: keys, Installation: installation, Plugins: pluginHost, Egress: policy, Log: log}
	wg.Go(func() { refresher.Run(ctx) })
	if exporter != nil {
		wg.Go(func() { exporter.Run(ctx) })
	}
	if focusWorker != nil {
		wg.Go(func() { focusWorker.Run(ctx) })
	}
	// The billing importer needs only PostgreSQL and provider egress, so it
	// runs on every worker replica, coordination store or not.
	if importer != nil {
		wg.Go(func() { importer.Run(ctx) })
	}
	wg.Go(func() {
		usage.RunNotificationDelivery(ctx, pool, keys, installation, policy, log,
			usage.NotificationDependencies{Limiter: limiter, Catalog: reference})
	})
	wg.Go(func() { runDebitReconciliation(ctx, pool, limiter, log) })
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
	wg.Go(func() { runMaintenance(ctx, pool, log) })
	wg.Go(func() {
		connect := func(context.Context) (*limits.Limiter, error) { return limiter, nil }
		limits.RunRegionalCostReconciliation(ctx, pool, region, connect, costCheckpoint(pool), log)
	})
	return wg.Wait
}

// probeCheckpoint records how one health probe pass ended in the shared
// worker health table.
func probeCheckpoint(pool *pgxpool.Pool, log *slog.Logger) func(context.Context, int, error) {
	return func(ctx context.Context, probed int, err error) {
		outcome := usage.OutcomeSuccess
		if err != nil {
			outcome = usage.OutcomeFailure
			log.Warn("health probes failed", "error", err.Error())
		}
		if err := usage.CheckpointTask(ctx, pool, usage.TaskHealthProbes, outcome, probed > 0); err != nil {
			log.Warn("health probe checkpoint failed", "error", err.Error())
		}
	}
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
