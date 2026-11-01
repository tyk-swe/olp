package main

import (
	"context"

	"github.com/tyk-swe/olp/internal/antigravityauth"
	"github.com/tyk-swe/olp/sdk/plugin"
)

func init() {
	plugin.Register(antigravityauth.Adapter{Fetch: plugin.Fetch, ClientID: func(ctx context.Context) string {
		provider, _ := plugin.ProviderOf(ctx)
		return provider.Options["client_id"]
	}})
}

func main() { plugin.Serve() }
