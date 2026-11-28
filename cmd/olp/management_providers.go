package main

import (
	"context"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/platform/secretstore"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers"
)

func (m Management) registerProviders(mux *http.ServeMux, activated func(context.Context, pgx.Tx, string, string) error) *providers.Server {
	providerServer := providers.New(providers.Deps{
		Access: m.Access, Egress: m.Egress, Getenv: os.Getenv, ExternalSecrets: secretstore.New(*m.Egress, os.Getenv),
		Unconfined: m.Unconfined, Log: m.Log, Plugins: m.PluginHost, Catalog: m.Catalog,
		Activated: activated,
		Quotas: func() providers.QuotaSource {
			if m.Limits == nil {
				return nil
			}
			return m.Limits.Fleet
		}(),
	})
	providerServer.Register(mux)
	(&plugins.Management{Access: m.Access, Runtime: m.PluginRuntime, Host: m.PluginHost, Unconfined: m.Unconfined, Index: m.PluginIndex}).Register(mux)
	return providerServer
}
