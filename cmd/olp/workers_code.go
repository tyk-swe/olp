package main

import (
	"context"
	"time"

	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/codesessions/codeaccounts"
	"github.com/tyk-swe/olp/internal/codesessions/codeallowance"
)

func (r *running) codeWorkers() []func(context.Context) {
	pool, core := r.stores.pool, r.core
	allowanceReader := &codeallowance.Reader{
		Pool: pool, Store: codesessions.New(pool),
		Authorizer: &codeaccounts.Authorizer{Pool: pool, Credentials: core.rt, Plugins: core.pluginHost},
		Client:     core.policy.Client(30 * time.Second), Log: r.log,
	}
	return []func(context.Context){allowanceReader.Run}
}
