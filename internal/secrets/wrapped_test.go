package secrets

import (
	"context"

	"github.com/tyk-swe/olp/internal/secretstore"
)

type unwrapFunc func(context.Context, secretstore.WrappedKey) ([]byte, error)

func (f unwrapFunc) Unwrap(ctx context.Context, key secretstore.WrappedKey) ([]byte, error) {
	return f(ctx, key)
}
