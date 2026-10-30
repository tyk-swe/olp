package gateway

import (
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/routing"
)

type codeTarget struct {
	config   routing.Configuration
	vendor   codeadapter.Vendor
	endpoint codeadapter.Endpoint
}

func resolveCodeTarget(snapshot *routing.Snapshot, route codemode.Route, providerID string, ingress codeIngress, transport codeadapter.Transport) (codeTarget, error) {
	config, exists := snapshot.CodeConnection(route, providerID)
	if !exists {
		return codeTarget{}, codemode.Refuse(503, "code_connection_unpublished")
	}
	vendor, known := codeadapter.ForConnection(config.Kind, config.AuthMode, config.ProfileID)
	endpoint, served := vendor.Endpoint(ingress.path)
	if !known || !served || endpoint.Protocol != ingress.protocol || !endpoint.Supports(transport) {
		return codeTarget{}, codemode.Refuse(503, "code_configuration_unsupported")
	}
	return codeTarget{config: config, vendor: vendor, endpoint: endpoint}, nil
}

func (t codeTarget) dispatch() codemode.Dispatch {
	return codemode.Dispatch{Adapter: t.vendor.Adapter, Protocol: t.endpoint.Protocol, ClientPath: t.endpoint.ClientPath}
}
