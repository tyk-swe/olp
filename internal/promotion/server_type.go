package promotion

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/plugins"
)

type Server struct {
	Access *access.Server
	Egress *egress.Policy
	// Unconfined is the deployment's unconfined plugin tier, or nil where it
	// enables none, for plugin providers to pin.
	Unconfined *plugins.Unconfined
	// Limiter, when the deployment enforces limits, receives budget balances a
	// document changes before the change publishes.
	Limiter                *limits.Limiter
	VendorKind             func(vendor string) (string, bool)
	StoreNetworkCredential func(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error)
	StoreCredential        func(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error)
}
