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
		for _, id := range route.PublicModels() {
			declarations := slices.Collect(func(yield func(codeadapter.Vendor) bool) {
				for _, adapter := range offers.adapters(route, id) {
					if declaration, ok := codeadapter.Lookup(adapter); ok && !yield(declaration) {
						return
					}
				}
			})
			resolved, err := route.ResolveModel(codemode.Operation{Model: id})
			if err != nil {
				continue
			}
			if _, mapped := route.PaidTargets[resolved.Model]; mapped {
				if protocol, streaming, ok := offers.nativePaidProtocol(route, id); ok {
					capability, clients := paidCapability(protocol, streaming)
					if !yield(codemode.Model{ID: id, Object: "model", Paid: true, Capabilities: []codemode.ModelCapability{capability}, Clients: clients}) {
						return
					}
				}
				continue
			}
			protocol, streaming, paid := offers.nativePaidProtocol(route, id)
			fallbackCapabilities, fallbackClients := func() ([]codemode.ModelCapability, []string) {
				if !paid {
					return nil, nil
				}
				capability, clients := paidCapability(protocol, streaming)
				return []codemode.ModelCapability{capability}, clients
			}()
			capabilities := modelCapabilities(route, resolved.Model, declarations, fallbackCapabilities...)
			if len(capabilities) > 0 && !yield(codemode.Model{ModelPool: slices.Clone(route.ModelPools[id]), ID: id, Object: "model", Paid: paid, Capabilities: capabilities, Clients: modelClients(route, resolved.Model, declarations, fallbackClients...)}) {
				return
			}
		}
	})
}

func modelCapabilities(route codemode.Route, id string, declarations []codeadapter.Vendor, extra ...codemode.ModelCapability) []codemode.ModelCapability {
	capabilities := slices.Concat(extra, slices.Collect(func(yield func(codemode.ModelCapability) bool) {
		for _, declaration := range declarations {
			for _, translated := range []struct {
				path     string
				protocol codemode.Protocol
			}{{"v1/messages", codemode.ProtocolMessages}, {"v1/chat/completions", codemode.ProtocolChat}} {
				if target, ok := codeadapter.RouteEndpoint(route, declaration, translated.path); ok && target.ClientPath != translated.path && declaration.ServesModel(target.Protocol, id) {
					if !yield(codemode.ModelCapability{Protocol: translated.protocol, Path: translated.path, Transports: []string{"http"}}) {
						return
					}
				}
			}
			for _, endpoint := range declaration.Endpoints() {
				if !endpoint.Target.Enabled(route) || !declaration.ServesModel(endpoint.Protocol, id) {
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
	}))
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

func modelClients(route codemode.Route, id string, declarations []codeadapter.Vendor, extra ...string) []string {
	clients := slices.Concat(extra, slices.Collect(func(yield func(string) bool) {
		for _, declaration := range declarations {
			for _, client := range codeadapter.RouteClients(route, declaration) {
				if _, ok := codeadapter.RouteClientProtocol(route, declaration, client, id); ok && !yield(client) {
					return
				}
			}
		}
	}))
	return slices.AppendSeq([]string{}, func(yield func(string) bool) {
		for i, client := range clients {
			if !slices.Contains(clients[:i], client) && !yield(client) {
				return
			}
		}
	})
}

func (offers Offers) nativePaidProtocol(route codemode.Route, model string) (codemode.Protocol, bool, bool) {
	resolved, err := route.ResolveModel(codemode.Operation{Model: model})
	if err != nil || !route.PaidParticipation {
		return "", false, false
	}
	if _, direct := route.PaidTargets[resolved.Model]; !direct {
		if _, fallback := route.PaidFallbackTargets[resolved.Model]; !fallback || route.InitialRecoveryAttempts < 1 {
			return "", false, false
		}
	}
	for _, offer := range offers {
		if offer.PaidProtocol != "" && slices.Contains(offer.Models, resolved.Model) {
			return offer.PaidProtocol, offer.PaidStreaming, true
		}
	}
	return "", false, false
}

func (offers Offers) paidProtocol(route codemode.Route, model, client string) (codemode.Protocol, bool) {
	protocol, streaming, ok := offers.nativePaidProtocol(route, model)
	if !ok {
		return "", false
	}
	_, clients := paidCapability(protocol, streaming)
	if !slices.Contains(clients, client) {
		return "", false
	}
	resolved, _ := route.ResolveModel(codemode.Operation{Model: model})
	if _, direct := route.PaidTargets[resolved.Model]; !direct {
		for _, adapter := range offers.adapters(route, model) {
			declaration, _ := codeadapter.Lookup(adapter)
			if routeReaches(route, declaration, client, resolved.Model) {
				return "", false
			}
		}
	}
	return protocol, true
}

func paidCapability(protocol codemode.Protocol, streaming bool) (codemode.ModelCapability, []string) {
	clients := func(values ...string) []string {
		if !streaming {
			return []string{}
		}
		return values
	}
	switch protocol {
	case codemode.ProtocolResponses:
		return codemode.ModelCapability{Protocol: protocol, Path: "v1/responses", Transports: []string{"http"}}, clients(codeadapter.ClientCodex, codeadapter.ClientOpenCode)
	case codemode.ProtocolMessages:
		return codemode.ModelCapability{Protocol: protocol, Path: "v1/messages", Transports: []string{"http"}}, clients(codeadapter.ClientClaudeCode, codeadapter.ClientOpenCode)
	default:
		return codemode.ModelCapability{Protocol: protocol, Path: "v1/chat/completions", Transports: []string{"http"}}, clients(codeadapter.ClientOpenCode)
	}
}
