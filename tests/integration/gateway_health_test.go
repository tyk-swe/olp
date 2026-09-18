//go:build integration

package integration_test

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/usage"
)

// persistedUsageStream connects the fixture's accounting writer to the durable
// facts read by provider health, without starting a separate worker process.
type persistedUsageStream struct{ pool *pgxpool.Pool }

func (s persistedUsageStream) Do(ctx context.Context, args ...string) (any, error) {
	payload := []byte(args[4])
	event, _, err := usage.Decode(payload)
	if err != nil {
		return nil, err
	}
	return usage.PersistEvent(ctx, s.pool, event, payload)
}
