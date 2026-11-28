package main

import (
	"context"
	"time"

	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/notifications"
)

func (r *running) operationsWorkers() []func(context.Context) {
	pool, core := r.stores.pool, r.core
	return []func(context.Context){
		func(ctx context.Context) {
			notifications.RunDelivery(ctx, notifications.DeliveryConfig{
				Pool: pool, Keys: core.keys, Installation: r.stores.installation, Log: r.log,
				Sender: notifications.NewSender(*core.policy), Fleet: r.stores.limitState().Fleet, Catalog: r.reference,
				NewID: ids.New, Now: time.Now,
			})
		},
		func(ctx context.Context) { runMaintenance(ctx, pool, r.log) },
	}
}
