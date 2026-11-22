package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/tyk-swe/olp/internal/admission"
	"github.com/tyk-swe/olp/internal/agents"
	"github.com/tyk-swe/olp/internal/agents/a2a"
	"github.com/tyk-swe/olp/internal/agents/runs"
	"github.com/tyk-swe/olp/internal/agents/tasks"
	"github.com/tyk-swe/olp/internal/apikeys"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/gateway/codeforward"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/grants/personal"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/mcp"
	mcpaccounting "github.com/tyk-swe/olp/internal/mcp/accounting"
	"github.com/tyk-swe/olp/internal/mcp/registration"
	"github.com/tyk-swe/olp/internal/mcp/sessions"
	"github.com/tyk-swe/olp/internal/mcp/upstream"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/providers/codeaccounts"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/staging"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/completeness"
	"github.com/tyk-swe/olp/internal/usage/delivery"
	"github.com/tyk-swe/olp/internal/usage/ingest"
)

// metadataBuffer is how many request metadata events one producer replica may
// hold while the stream writer catches up. Beyond it events are dropped and
// counted as loss, so a slow or unreachable stream costs completeness rather
// than memory or inference latency.
const metadataBuffer = 8192

type foundation struct {
	config Config
	log    *slog.Logger
	stores datastores
	core   runtimeCore
	media  mediaPlane
	public *http.ServeMux
	// The inference pool is shared with the gateway so middleware and direct
	// handler calls bound one capacity.
	inferencePool  *admission.Pool
	managementPool *admission.Pool
	health         *observability.Cache
	loss           *completeness.LossCounters
	mediaGaps      *atomic.Uint64
}

type agentPlane struct {
	tasks  *tasks.Store
	peers  *a2a.Peers
	grants *personal.Store
}

type serving struct {
	gateway    *kernel.Server
	prober     *kernel.Server
	accounting *delivery.Delivery
	overlaps   *apikeys.OverlapRecorder
	agents     agentPlane
	monitor    *agents.TaskMonitor
}

func newServing(f foundation) (serving, error) {
	c, stores := f.config, f.stores
	assembly := gatewayTemplates(gatewayInputs{config: c, pool: stores.pool, core: f.core, inferencePool: f.inferencePool, limiter: stores.limiter, shared: stores.shared, outage: stores.outage, log: f.log})
	plane := f.agentPlane()
	accounting := f.accounting()
	overlaps := f.overlapRecorder()
	baseGW := f.baseGateway(assembly, accounting, overlaps)
	inference, err := f.inference(plane, baseGW)
	if err != nil {
		return serving{}, err
	}
	gw := baseGW
	if inference.workflowTools != nil {
		gw = baseGW.WithWorkflow(inference.workflowRuns, inference.workflowTools)
	}
	built := serving{gateway: gw, prober: f.prober(assembly, accounting), accounting: accounting, overlaps: overlaps, agents: plane, monitor: inference.monitor}
	if c.Mode.Inference() {
		gateway.Register(gw, f.public)
	}
	return built, nil
}

func (f foundation) overlapRecorder() *apikeys.OverlapRecorder {
	if !f.config.Mode.Inference() || f.stores.pool == nil {
		return nil
	}
	return apikeys.NewOverlapRecorder(f.stores.pool, f.log)
}

func (f foundation) baseGateway(assembly gatewayAssembly, accounting *delivery.Delivery, overlaps *apikeys.OverlapRecorder) *kernel.Server {
	if !f.config.Mode.Management() && !f.config.Mode.Inference() {
		return nil
	}
	deps := assembly.deps
	if f.config.Mode.Inference() {
		deps = f.inferenceDependencies(deps)
		if overlaps != nil {
			deps.ObserveOverlap = overlaps.Observe
		}
	}
	return kernel.New(f.accounted(deps, accounting), assembly.config)
}

func (f foundation) prober(assembly gatewayAssembly, accounting *delivery.Delivery) *kernel.Server {
	if assembly.probeDeps.Admission == nil {
		return nil
	}
	return kernel.New(f.accounted(assembly.probeDeps, accounting), assembly.probeConfig)
}

func (f foundation) agentPlane() agentPlane {
	c, stores, core := f.config, f.stores, f.core
	if !c.Mode.Management() && !c.Mode.Inference() {
		return agentPlane{}
	}
	return agentPlane{
		grants: personal.NewStore(stores.pool, stores.installation, core.auth, core.keys, core.policy),
		tasks:  tasks.New(stores.pool, f.taskLimits(), core.auth, f.log),
		peers: a2a.NewPeers(core.policy, func(ctx context.Context, id string) ([]byte, error) {
			return core.keys.Read(ctx, stores.pool, stores.installation, id, secrets.AgentCredential)
		}),
	}
}

func (f foundation) taskLimits() tasks.Limits {
	if f.stores.shared == nil {
		return tasks.Limits{Debits: f.stores.debitStore}
	}
	return tasks.Limits{Debits: f.stores.debitStore, Syncer: f.stores.shared.Syncer, Limiter: f.stores.limiter}
}

type inferencePlane struct {
	workflowRuns  kernel.WorkflowRunStore
	workflowTools kernel.WorkflowToolRunner
	monitor       *agents.TaskMonitor
}

func (f foundation) inference(plane agentPlane, evaluator *kernel.Server) (inferencePlane, error) {
	c, stores, core := f.config, f.stores, f.core
	if !c.Mode.Inference() {
		return inferencePlane{}, nil
	}
	executor := agents.New(agents.Options{Tasks: plane.tasks, Peers: plane.peers, Grants: plane.grants, Entra: core.rt})
	a2a.New(a2a.Options{Runtime: core.rt, Executor: executor, Peers: plane.peers, Grants: plane.grants, TrustedProxies: c.TrustedProxyCIDRs, PublicOrigin: c.PublicOrigin}).Register(f.public)
	monitor := agents.NewMonitor(plane.tasks, agents.NewTaskOperations(plane.tasks, plane.peers, plane.grants), core.rt, f.log)
	sessionStore, err := f.toolSessions()
	if err != nil {
		return inferencePlane{}, err
	}
	toolExecutor := mcp.NewExecutor(mcp.ExecutorOptions{
		Authority: core.rt, Grants: plane.grants, Connect: upstream.NewDialer(core.policy, registration.NewExecutables(core.unconfined, stores.pool)),
		Ledger: f.toolLedger(), Auth: core.auth, Limiter: f.toolLimiter(), Log: f.log,
		Credentials: func(ctx context.Context, credentialID string) ([]byte, error) {
			return core.keys.Read(ctx, stores.pool, stores.installation, credentialID, secrets.MCPCredential)
		},
		Guardrails: func(ctx context.Context, in mcp.GuardrailEvaluation) (guardrails.Result, error) {
			return evaluator.EvaluateToolGuardrails(ctx, in.Authority, in.Release, in.Engine, in.RequestID, in.Phase, in.Segments, in.Masks)
		},
	})
	toolServer := mcp.New(mcp.ServerOptions{Runtime: core.rt, Executor: toolExecutor, Auth: core.auth, Grants: plane.grants, Sessions: sessionStore, TrustedProxies: c.TrustedProxyCIDRs})
	toolServer.Register(f.public)
	if stores.limiter == nil {
		return inferencePlane{monitor: monitor}, nil
	}
	return inferencePlane{
		workflowRuns:  runs.NewWorkflowStore(stores.pool, core.auth, core.keys, stores.installation),
		workflowTools: toolServer,
		monitor:       monitor,
	}, nil
}

func (f foundation) inferenceDependencies(templates kernel.Dependencies) kernel.Dependencies {
	pool, core, media := f.stores.pool, f.core, f.media
	deps := templates
	deps.Capture = core.capturer
	deps.Media = &kernel.MediaDeps{Transport: media.transport, Jobs: media.service, Store: jobs.NewStore(pool)}
	deps.Staging = media.spool
	deps.Uploads = staging.NewAdmission(media.spool.CapacityBytes())
	if pool == nil {
		return deps
	}
	deps.Code = &codeforward.Services{
		Ledger:     codesessions.New(pool),
		Authorizer: &codeaccounts.Authorizer{Pool: pool, Credentials: core.rt, Plugins: core.pluginHost},
	}
	deps.Resources = resources.NewEncrypted(pool, f.stores.installation, core.keys)
	deps.Resolver = resources.NewResolver(pool)
	return deps
}

func (f foundation) toolLedger() mcpaccounting.Ledger {
	if f.stores.limiter == nil {
		return nil
	}
	return mcpaccounting.NewDurableLedger(f.stores.pool, f.stores.debitStore, f.stores.shared.Syncer, f.core.auth)
}

func (f foundation) toolLimiter() mcp.RateLimiter {
	if f.stores.limiter == nil {
		return nil
	}
	return f.stores.limiter
}

func (f foundation) toolSessions() (*sessions.Store, error) {
	if f.stores.vk == nil {
		return nil, nil
	}
	return sessions.New(f.stores.vk, f.stores.prefix+"mcp")
}

// Every process that can spend against shared budgets owns a durable producer
// epoch, including control-only playground calls and worker-only health
// probes. No producer may log and forget a failed ledger write. Without
// shared state, hard limits are refused and metadata stays logged.
func (f foundation) accounting() *delivery.Delivery {
	if f.stores.limiter == nil {
		return nil
	}
	return delivery.NewDelivery(f.stores.pool, completeness.GatewayInstance(f.config.Hostname), metadataBuffer, f.loss, f.log)
}

func (f foundation) accounted(templates kernel.Dependencies, accounting *delivery.Delivery) kernel.Dependencies {
	if accounting == nil || templates.Admission == nil {
		return templates
	}
	pool, syncer := f.stores.pool, f.stores.shared.Syncer
	deps := templates
	deps.Debits = &kernel.SessionDebits{
		Session: debits.NewSession(f.stores.debitStore, syncer),
		SettleWorkflow: func(ctx context.Context, event *usage.Event, attempt int) ([]limits.Snapshot, error) {
			return ingest.PersistWorkflowAttempt(ctx, pool, event, attempt)
		},
	}
	deps.Admission.PendingCostAccountingReady = syncer.CheckOwnerAccounting
	deps.Admission.PendingTokenAccountingReady = resources.New(pool).CheckPendingTokenOwner
	deps.Admission.CostAccountingReady = accounting.Check
	deps.Sink = &events.AccountingSink{Delivery: accounting, Next: templates.Sink}
	return deps
}
