package codeclient

import (
	"slices"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

func Catalog(route codemode.Route, offers Offers) []codemode.Model {
	if !route.Enabled || route.RevisionID == "" {
		return []codemode.Model{}
	}
	return slices.AppendSeq([]codemode.Model{}, func(yield func(codemode.Model) bool) {
		for _, id := range route.Models {
			declarations := slices.Collect(func(yield func(codeadapter.Vendor) bool) {
				for _, adapter := range offers.adapters(id) {
					if declaration, ok := codeadapter.Lookup(adapter); ok && !yield(declaration) {
						return
					}
				}
			})
			capabilities := modelCapabilities(id, declarations)
			if len(capabilities) > 0 && !yield(codemode.Model{ID: id, Object: "model", Capabilities: capabilities, Clients: modelClients(id, declarations)}) {
				return
			}
		}
	})
}

func modelCapabilities(id string, declarations []codeadapter.Vendor) []codemode.ModelCapability {
	capabilities := slices.Collect(func(yield func(codemode.ModelCapability) bool) {
		for _, declaration := range declarations {
			for _, endpoint := range declaration.Endpoints() {
				if !declaration.ServesModel(endpoint.Protocol, id) {
					continue
				}
				transports := slices.Collect(func(yield func(string) bool) {
					for _, transport := range []struct {
						flag codeadapter.Transport
						name string
					}{{codeadapter.HTTP, "http"}, {codeadapter.WebSocket, "websocket"}} {
						if endpoint.Supports(transport.flag) && !yield(transport.name) {
							return
						}
					}
				})
				if !yield(codemode.ModelCapability{Protocol: endpoint.Protocol, Path: endpoint.ClientPath, Transports: transports}) {
					return
				}
			}
		}
	})
	return slices.AppendSeq([]codemode.ModelCapability{}, func(yield func(codemode.ModelCapability) bool) {
		for i, capability := range capabilities {
			duplicate := slices.ContainsFunc(capabilities[:i], func(previous codemode.ModelCapability) bool {
				return previous.Protocol == capability.Protocol && previous.Path == capability.Path && slices.Equal(previous.Transports, capability.Transports)
			})
			if !duplicate && !yield(capability) {
				return
			}
		}
	})
}

func modelClients(id string, declarations []codeadapter.Vendor) []string {
	clients := slices.Collect(func(yield func(string) bool) {
		for _, declaration := range declarations {
			for _, client := range declaration.Clients() {
				if declaration.Reaches(client, id) && !yield(client) {
					return
				}
			}
		}
	})
	return slices.AppendSeq([]string{}, func(yield func(string) bool) {
		for i, client := range clients {
			if !slices.Contains(clients[:i], client) && !yield(client) {
				return
			}
		}
	})
}
