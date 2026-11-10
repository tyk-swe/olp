package client

import (
	"slices"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
)

func Catalog(route code.Route, offers Offers) []code.Model {
	if !route.Enabled || route.RevisionID == "" {
		return []code.Model{}
	}
	return slices.AppendSeq([]code.Model{}, func(yield func(code.Model) bool) {
		for _, id := range route.PublicModels() {
			declarations := slices.Collect(func(yield func(vendor.Vendor) bool) {
				for _, adapter := range offers.adapters(route, id) {
					if declaration, ok := vendor.Lookup(adapter); ok && !yield(declaration) {
						return
					}
				}
			})
			resolved, err := route.ResolveModel(code.Operation{Model: id})
			if err != nil {
				continue
			}
			if _, mapped := route.PaidTargets[resolved.Model]; mapped {
				if protocol, streaming, ok := offers.nativePaidProtocol(route, id); ok {
					capability, clients := paidCapability(protocol, streaming)
					if !yield(code.Model{ID: id, Object: "model", Paid: true, Capabilities: []code.ModelCapability{capability}, Clients: clients}) {
						return
					}
				}
				continue
			}
			protocol, streaming, paid := offers.nativePaidProtocol(route, id)
			var fallbackCapabilities []code.ModelCapability
			var fallbackClients []string
			if paid {
				capability, clients := paidCapability(protocol, streaming)
				fallbackCapabilities, fallbackClients = []code.ModelCapability{capability}, clients
			}
			capabilities := modelCapabilities(route, resolved.Model, declarations, fallbackCapabilities...)
			if len(capabilities) > 0 && !yield(code.Model{
				ModelPool:    slices.Clone(route.ModelPools[id]),
				ID:           id,
				Object:       "model",
				Paid:         paid,
				Capabilities: capabilities,
				Clients:      modelClients(route, resolved.Model, declarations, fallbackClients...),
			}) {
				return
			}
		}
	})
}

func modelCapabilities(route code.Route, id string, declarations []vendor.Vendor, extra ...code.ModelCapability) []code.ModelCapability {
	capabilities := slices.Concat(extra, slices.Collect(func(yield func(code.ModelCapability) bool) {
		for _, declaration := range declarations {
			for _, translated := range []struct {
				path     string
				protocol code.Protocol
			}{{"v1/messages", code.ProtocolMessages}, {"v1/chat/completions", code.ProtocolChat}} {
				if target, ok := vendor.RouteEndpoint(route, declaration, translated.path); ok && target.ClientPath != translated.path && declaration.ServesModel(target.Protocol, id) {
					if !yield(code.ModelCapability{Protocol: translated.protocol, Path: translated.path, Transports: []string{"http"}}) {
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
						flag code.Transport
						name string
					}{{code.HTTP, "http"}, {code.WebSocket, "websocket"}} {
						if endpoint.Supports(transport.flag) && !yield(transport.name) {
							return
						}
					}
				})
				if !yield(code.ModelCapability{Protocol: endpoint.Protocol, Path: endpoint.ClientPath, Transports: transports}) {
					return
				}
			}
		}
	}))
	return slices.AppendSeq([]code.ModelCapability{}, func(yield func(code.ModelCapability) bool) {
		for i, capability := range capabilities {
			duplicate := slices.ContainsFunc(capabilities[:i], func(previous code.ModelCapability) bool {
				return previous.Protocol == capability.Protocol && previous.Path == capability.Path && slices.Equal(previous.Transports, capability.Transports)
			})
			if !duplicate && !yield(capability) {
				return
			}
		}
	})
}

func modelClients(route code.Route, id string, declarations []vendor.Vendor, extra ...string) []string {
	clients := slices.Concat(extra, slices.Collect(func(yield func(string) bool) {
		for _, declaration := range declarations {
			for _, client := range vendor.RouteClients(route, declaration) {
				if _, ok := vendor.RouteClientProtocol(route, declaration, client, id); ok && !yield(client) {
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

func (o Offers) nativePaidProtocol(route code.Route, model string) (code.Protocol, bool, bool) {
	resolved, err := route.ResolveModel(code.Operation{Model: model})
	if err != nil || !route.PaidParticipation {
		return "", false, false
	}
	target := func() string {
		if _, direct := route.PaidTargets[resolved.Model]; direct {
			return resolved.Model
		}
		if route.InitialRecoveryAttempts < 1 {
			return ""
		}
		fallback, err := route.PaidFallbackModel(code.Operation{Model: model}, nil)
		if err != nil || fallback == nil {
			return ""
		}
		return fallback.Model
	}()
	for _, offer := range o.Paid {
		if offer.Protocol != "" && offer.Model == target {
			return offer.Protocol, offer.Streaming, true
		}
	}
	return "", false, false
}

func (o Offers) paidProtocol(route code.Route, model, client string) (code.Protocol, bool) {
	protocol, streaming, ok := o.nativePaidProtocol(route, model)
	if !ok {
		return "", false
	}
	_, clients := paidCapability(protocol, streaming)
	if !slices.Contains(clients, client) {
		return "", false
	}
	resolved, _ := route.ResolveModel(code.Operation{Model: model})
	if _, direct := route.PaidTargets[resolved.Model]; !direct {
		for _, adapter := range o.adapters(route, model) {
			declaration, _ := vendor.Lookup(adapter)
			if routeReaches(route, declaration, client, resolved.Model) {
				return "", false
			}
		}
	}
	return protocol, true
}

func paidCapability(protocol code.Protocol, streaming bool) (code.ModelCapability, []string) {
	clients := func(values ...string) []string {
		if !streaming {
			return []string{}
		}
		return values
	}
	switch protocol {
	case code.ProtocolResponses:
		return code.ModelCapability{Protocol: protocol, Path: "v1/responses", Transports: []string{"http"}}, clients(code.ClientCodex, code.ClientOpenCode)
	case code.ProtocolMessages:
		return code.ModelCapability{Protocol: protocol, Path: "v1/messages", Transports: []string{"http"}}, clients(code.ClientClaudeCode, code.ClientOpenCode)
	default:
		return code.ModelCapability{Protocol: protocol, Path: "v1/chat/completions", Transports: []string{"http"}}, clients(code.ClientOpenCode)
	}
}
