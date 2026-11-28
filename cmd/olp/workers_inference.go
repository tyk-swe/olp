package main

import (
	"context"
	"os"
	"time"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/reconcile"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/resources/retainedworker"
)

func (r *running) inferenceWorkers() []func(context.Context) {
	pool, core := r.stores.pool, r.core
	batchAuth := connectors.NewSigningAuth(core.policy, os.Getenv, core.pluginHost)
	batchWorker := reconcile.New(reconcile.Deps{
		Pool: pool, Store: batches.NewStore(pool),
		Resources: resources.NewEncrypted(pool, r.stores.installation, core.keys),
		Resolver:  resources.NewResolver(pool), Credentials: core.rt,
		DigestKey: core.auth, Readers: upstream.New(batchAuth, core.policy), Egress: core.policy,
		Client: core.policy.Client(5 * time.Minute), Connections: egress.NewConnectionClientCache(128),
		Limiter: r.stores.limiter, Log: r.log, Now: time.Now,
	})
	retained := &retainedworker.Worker{Store: resources.New(pool), Resolver: resources.NewResolver(pool), Credentials: core.rt, Auth: batchAuth, Egress: core.policy, Connections: egress.NewConnectionClientCache(128), Log: r.log}
	return []func(context.Context){r.media.service.Run, batchWorker.Run, retained.Run}
}
