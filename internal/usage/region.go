package usage

import "context"

type workerRegionKey struct{}

// WithWorkerRegion scopes checkpoints for tasks backed by a regional Valkey.
// Tasks that work only in PostgreSQL retain their installation-wide summary.
func WithWorkerRegion(ctx context.Context, region string) context.Context {
	return context.WithValue(ctx, workerRegionKey{}, region)
}

// RegionalTask reports whether one region's success cannot vouch for another
// region's independent coordination store.
func RegionalTask(task Task) bool {
	return task == TaskRequestMetadataConsumer || task == TaskCostReconciliation || task == TaskHealthProbes
}
