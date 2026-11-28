package main

import (
	"context"

	"github.com/tyk-swe/olp/internal/grants"
)

func (r *running) providerWorkers() []func(context.Context) {
	refresher := &grants.Refresher{Pool: r.stores.pool, Keys: r.core.keys, Installation: r.stores.installation, Plugins: r.core.pluginHost, Egress: r.core.policy, Log: r.log}
	return []func(context.Context){refresher.Run}
}
