package codeclient

import (
	"slices"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func Catalog(route codemode.Route, offers Offers) []codemode.Model {
	models := []codemode.Model{}
	if !route.Enabled || route.RevisionID == "" {
		return models
	}
	for _, id := range route.Models {
		model := codemode.Model{ID: id, Object: "model", Capabilities: []codemode.ModelCapability{}, Clients: []string{}}
		for _, adapter := range offers.adapters(id) {
			vendor, ok := codeadapter.Lookup(adapter)
			if !ok {
				continue
			}
			for _, endpoint := range vendor.Endpoints() {
				if !vendor.ServesModel(endpoint.Protocol, id) {
					continue
				}
				capability := codemode.ModelCapability{Protocol: endpoint.Protocol, Path: endpoint.ClientPath, Transports: []string{}}
				for _, transport := range []codeadapter.Transport{codeadapter.HTTP, codeadapter.WebSocket} {
					if endpoint.Supports(transport) {
						capability.Transports = append(capability.Transports, string(transport))
					}
				}
				if !slices.ContainsFunc(model.Capabilities, func(c codemode.ModelCapability) bool {
					return c.Protocol == capability.Protocol && c.Path == capability.Path && slices.Equal(c.Transports, capability.Transports)
				}) {
					model.Capabilities = append(model.Capabilities, capability)
				}
			}
			for _, client := range vendor.Clients() {
				if vendor.Reaches(client, id) && !slices.Contains(model.Clients, client) {
					model.Clients = append(model.Clients, client)
				}
			}
		}
		if len(model.Capabilities) > 0 {
			models = append(models, model)
		}
	}
	return models
}
