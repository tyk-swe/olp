package main

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/notifications"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/providers/codeaccounts"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/usage/completeness"
	"github.com/tyk-swe/olp/internal/usage/ingest"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/reconcile"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/billing/imports"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/codeallowance"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/export/focus"
	"github.com/tyk-swe/olp/internal/export/stream"
	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/ledger"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedworker"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/sinks"
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
type workerDeps struct {
	pool         *pgxpool.Pool
	vk           *coordination.Client
	store        *debits.Store
	shared       *sharedLimits
	stream       string
	mediaService *jobs.Service
	credentials  release.Credentials
	pluginHost   *plugins.Host
	keys         *secrets.KeyRing
	auth         *secrets.AuthKey
	installation string
	region       string
	hostname     string
	policy       *egress.Policy
	log          *slog.Logger
	exporter     *stream.Worker
	focusWorker  *focus.Worker
	importer     *imports.Worker
	reference    *catalog.Signed
}

func startWorkers(ctx context.Context, supervise func(func()) func(), d workerDeps) func() {
	pool, vk, store, shared, streamName := d.pool, d.vk, d.store, d.shared, d.stream
	mediaService, credentials, pluginHost := d.mediaService, d.credentials, d.pluginHost
	keys, auth, policy, log := d.keys, d.auth, d.policy, d.log
	installation, region, hostname := d.installation, d.region, d.hostname
	exporter, focusWorker, importer, reference := d.exporter, d.focusWorker, d.importer, d.reference
	var wg sync.WaitGroup
	var limiter *admission.Limiter
	var syncer *debits.Syncer
	var fleetState *fleet.State
	if shared != nil {
		limiter, syncer, fleetState = shared.Limiter, shared.Syncer, shared.Fleet
	}
	managedExporter := &sinks.Worker{Pool: pool, Keys: keys, Installation: installation, Egress: policy}
	wg.Go(supervise(func() { managedExporter.Run(ctx, log) }))
	wg.Go(supervise(func() { mediaService.Run(ctx) }))
	batchAuth := connectors.NewAuth(policy, os.Getenv)
	batchAuth.Signer = pluginHost
	batchWorker := reconcile.New(reconcile.Deps{
		Pool: pool, Store: batches.NewStore(pool),
		Resources: resources.NewEncrypted(pool, installation, keys),
		Resolver:  resources.NewResolver(pool), Credentials: credentials,
		DigestKey: auth, Readers: upstream.New(batchAuth, policy), Egress: policy,
		Client: policy.Client(5 * time.Minute), Connections: egress.NewConnectionClientCache(128),
		Limiter: limiter, Log: log, Now: time.Now,
	})
	wg.Go(supervise(func() { batchWorker.Run(ctx) }))
	retained := &retainedworker.Worker{Store: resources.New(pool), Resolver: resources.NewResolver(pool), Credentials: credentials, Auth: batchAuth, Egress: policy, Connections: egress.NewConnectionClientCache(128), Log: log}
	wg.Go(supervise(func() { retained.Run(ctx) }))
	// Grant refresh needs only PostgreSQL and the providers' network paths.
	refresher := &grants.Refresher{Pool: pool, Keys: keys, Installation: installation, Plugins: pluginHost, Egress: policy, Log: log}
	wg.Go(supervise(func() { refresher.Run(ctx) }))
	allowanceReader := &codeallowance.Reader{
		Pool: pool, Store: codesessions.New(pool),
		Authorizer: &codeaccounts.Authorizer{Pool: pool, Credentials: credentials, Plugins: pluginHost},
		Client:     policy.Client(30 * time.Second), Log: log,
	}
	wg.Go(supervise(func() { allowanceReader.Run(ctx) }))
	wg.Go(supervise(func() { exporter.Run(ctx) }))
	wg.Go(supervise(func() { focusWorker.Run(ctx) }))
	// The billing importer needs only PostgreSQL and provider egress, so it
	// runs on every worker replica, coordination store or not.
	if importer != nil {
		wg.Go(supervise(func() { importer.Run(ctx) }))
	}
	wg.Go(supervise(func() {
		notifications.RunDelivery(ctx, notifications.DeliveryConfig{
			Pool: pool, Keys: keys, Installation: installation, Log: log,
			Sender: notifications.NewSender(*policy), Fleet: fleetState, Catalog: reference,
			NewID: httpkit.NewID, Now: time.Now,
		})
	}))
	wg.Go(supervise(func() { runDebitReconciliation(ctx, pool, store, syncer, log) }))
	wg.Go(supervise(func() { recoverWorkflowModels(ctx, store, log) }))
	wg.Go(supervise(func() { runMaintenance(ctx, pool, log) }))
	wg.Go(supervise(func() { completeness.RunEpochDetection(ctx, pool, log) }))
	if limiter == nil {
		return wg.Wait
	}
	wg.Go(supervise(func() {
		// The consumer identity is stable for this process and unique among
		// live ones, so a restart reclaims exactly its own deliveries.
		if err := (ingest.Consumer{Pool: pool, Client: vk, Stream: streamName, Name: ingest.ConsumerName(hostname), Region: region, Limiter: limiter, Log: log}).Run(ctx); err != nil {
			log.Error("request metadata consumer stopped", "error", err)
		}
	}))
	wg.Go(supervise(func() {
		connect := func(context.Context) (ledger.Installer, error) { return limiter, nil }
		ledger.Worker{Pool: pool, Region: region, Connect: connect, Checkpoint: costCheckpoint(pool, region), Log: log}.Run(ctx)
	}))
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
