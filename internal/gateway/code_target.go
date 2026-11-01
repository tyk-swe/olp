package gateway

import (
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/routing"
)

type codeTarget struct {
	config   routing.Configuration
	adapter  codeadapter.Vendor
	endpoint codeadapter.Endpoint
}

func resolveCodeTarget(snapshot *routing.Snapshot, route codemode.Route, providerID string, ingress codeIngress, transport codeadapter.Transport) (codeTarget, error) {
	config, exists := snapshot.CodeConnection(route, providerID)
	if !exists {
		return codeTarget{}, codemode.Refuse(503, "code_connection_unpublished")
	}
	adapter, known := codeadapter.ForConnection(config.Kind, config.AuthMode, config.ProfileID)
	endpoint, served := codeadapter.RouteEndpoint(route, adapter, ingress.path)
	if !known || !served || !endpoint.Supports(transport) {
		return codeTarget{}, codemode.Refuse(503, "code_configuration_unsupported")
	}
	return codeTarget{config: config, adapter: adapter, endpoint: endpoint}, nil
}

func (t codeTarget) dispatchDescriptor() codemode.Dispatch {
	return codemode.Dispatch{Adapter: t.adapter.Adapter, Protocol: t.endpoint.Protocol, ClientPath: t.endpoint.ClientPath}
}
