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
		Access: m.Access, Egress: m.core.policy, Getenv: os.Getenv, ExternalSecrets: secretstore.New(*m.core.policy, os.Getenv),
		Unconfined: m.core.unconfined, Log: m.log, Plugins: m.core.pluginHost, Catalog: m.documents.catalog,
		Activated: activated,
		Quotas: func() providers.QuotaSource {
			if m.stores.shared == nil {
				return nil
			}
			return m.stores.shared.Fleet
		}(),
	})
	providerServer.Register(mux)
	(&plugins.Management{Access: m.Access, Runtime: m.PluginRuntime, Host: m.core.pluginHost, Unconfined: m.core.unconfined, Index: m.documents.pluginIndex}).Register(mux)
	return providerServer
}
