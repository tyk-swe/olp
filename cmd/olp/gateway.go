package main

import (
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/admission"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/gateway/traceingest"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
)

type gatewayInputs struct {
	config        Config
	pool          *pgxpool.Pool
	core          runtimeCore
	inferencePool *admission.Pool
	limiter       *limitadmission.Limiter
	shared        *sharedLimits
	outage        *outagePolicy
	log           *slog.Logger
}

type gatewayAssembly struct {
	deps        gateway.Dependencies
	probeDeps   gateway.Dependencies
	config      gateway.Config
	probeConfig gateway.Config
}

func gatewayTemplates(in gatewayInputs) gatewayAssembly {
	c, core, log := in.config, in.core, in.log
	deps := gateway.Dependencies{
		Runtime: core.rt, Egress: core.policy, Getenv: os.Getenv, Log: log, Sink: events.LogSink{Log: log},
		Digester: core.rt, BudgetAuthority: core.rt, TaskAttemptAdmitter: core.rt,
		TaskCapabilityAuthenticator: core.rt, DelegationReauthenticator: core.rt,
	}
	assembly := gatewayAssembly{deps: deps, probeDeps: deps}
	if c.Mode.Management() || c.Mode.Inference() {
		assembly.config = gateway.Config{
			Region:             c.Region,
			MaxInFlight:        c.MaxInFlightInference,
			MaxShadowInFlight:  c.MaxInFlightShadow,
			CORSAllowedOrigins: c.GatewayCORSAllowedOrigins,
			InlineMedia:        protocols.InlineMediaLimits{Items: c.MaxInlineMediaItems, ItemBytes: c.MaxInlineMediaItemBytes, TotalBytes: c.MaxInlineMediaTotalBytes},
			MaxBodyBytes:       c.MaxJSONBodyBytes,
			MaxMediaBodyBytes:  c.MaxMediaBodyBytes,
			MaxResponseBytes:   c.ProviderMaxResponseBytes,
			MaxEventBytes:      c.ProviderMaxEventBytes,
			TrustedProxies:     c.TrustedProxyCIDRs,
			AdmissionPool:      in.inferencePool,
			Signer:             core.pluginHost,
			Predicates:         core.pluginHost,
			Carrier:            core.pluginHost,
			UnconfinedPlugins:  core.unconfined != nil,
		}
		assembly.deps.Traces = &traceingest.Store{Pool: in.pool, Limiter: in.limiter, Auth: core.auth}
		assembly.deps.Batches = batches.NewStore(in.pool)
		assembly.deps.Cache = core.responseCache
		assembly.deps.Business = core.business
		if in.limiter != nil {
			var outagePolicy func() limitadmission.OutagePolicy
			if in.outage != nil {
				outagePolicy = in.outage.Policy
			}
			// Control-only processes also execute playground requests.
			assembly.deps.Admission = quota.NewAdmission(in.limiter, in.shared.Fleet, outagePolicy, log)
		}
	}
	if in.limiter != nil && (c.Mode == Worker || c.Mode == All) {
		// The worker plane's health probes run through a gateway of their
		// own: planned, gated, priced and emitted like any request.
		assembly.probeConfig = gateway.Config{
			Region:            c.Region,
			MaxInFlight:       1,
			MaxResponseBytes:  c.ProviderMaxResponseBytes,
			MaxEventBytes:     c.ProviderMaxEventBytes,
			Signer:            core.pluginHost,
			Carrier:           core.pluginHost,
			UnconfinedPlugins: core.unconfined != nil,
		}
		assembly.probeDeps.Business = core.business
		assembly.probeDeps.Admission = quota.NewAdmission(in.limiter, in.shared.Fleet, nil, log)
	}
	return assembly
}
