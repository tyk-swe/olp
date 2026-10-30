package codeclient

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

// openCodeConfiguration lists each selected model under the provider of the
// adapter it is sent through, which picks its protocol, and every other native
// model under the provider of the first adapter that offers it, and enables
// those providers.
func openCodeConfiguration(route codemode.Route, adapters []codemode.Adapter, offers Offers, through map[string]codemode.Adapter, native []string, baseURL, model, plan, small string) (string, error) {
	type options struct {
		BaseURL string `json:"baseURL"`
		APIKey  string `json:"apiKey"`
	}
	type modelProvider struct {
		NPM string `json:"npm"`
	}
	type modelDefinition struct {
		ID       string        `json:"id"`
		Name     string        `json:"name"`
		Provider modelProvider `json:"provider"`
	}
	type settings struct {
		Options   options                    `json:"options"`
		Whitelist []string                   `json:"whitelist"`
		Models    map[string]modelDefinition `json:"models"`
	}
	type agent struct {
		Model string `json:"model"`
	}
	whitelists := map[codemode.Adapter][]string{}
	qualified := map[string]string{}
	for _, m := range native {
		if protocol, paid := offers.paidProtocol(route, m, codeadapter.ClientOpenCode); paid {
			qualified[m] = "olp-paid-" + string(protocol) + "/" + m
			continue
		}
		adapters := offers.adapters(route, m)
		if adapter, ok := through[m]; ok {
			adapters = []codemode.Adapter{adapter}
		}
		for _, adapter := range adapters {
			vendor, _ := codeadapter.Lookup(adapter)
			resolved, err := route.ResolveModel(codemode.Operation{Model: m})
			if err != nil || !routeReaches(route, vendor, codeadapter.ClientOpenCode, resolved.Model) {
				continue
			}
			if client, ok := codeadapter.RouteClient(route, vendor, codeadapter.ClientOpenCode); ok && client.Provider != "" {
				provider := client.Provider
				whitelists[adapter] = append(whitelists[adapter], m)
				qualified[m] = provider + "/" + m
				break
			}
		}
	}
	document := struct {
		Schema           string              `json:"$schema"`
		Model            string              `json:"model"`
		SmallModel       string              `json:"small_model"`
		Agent            map[string]agent    `json:"agent"`
		EnabledProviders []string            `json:"enabled_providers"`
		Provider         map[string]settings `json:"provider"`
	}{Schema: "https://opencode.ai/config.json", Model: qualified[model], SmallModel: qualified[small], Agent: map[string]agent{"plan": {Model: qualified[plan]}}, Provider: map[string]settings{}}
	for _, adapter := range adapters {
		if models := whitelists[adapter]; len(models) != 0 {
			vendor, _ := codeadapter.Lookup(adapter)
			client, _ := codeadapter.RouteClient(route, vendor, codeadapter.ClientOpenCode)
			provider := client.Provider
			document.EnabledProviders = append(document.EnabledProviders, provider)
			definitions := maps.Collect(func(yield func(string, modelDefinition) bool) {
				for _, model := range models {
					resolved, _ := route.ResolveModel(codemode.Operation{Model: model})
					protocol, _ := codeadapter.RouteClientProtocol(route, vendor, codeadapter.ClientOpenCode, resolved.Model)
					sdk := openCodeSDK(protocol)
					if !yield(model, modelDefinition{ID: model, Name: model, Provider: modelProvider{NPM: sdk}}) {
						return
					}
				}
			})
			document.Provider[provider] = settings{Options: options{BaseURL: baseURL + client.URLSuffix, APIKey: "{env:OLP_API_KEY}"}, Whitelist: models, Models: definitions}
		}
	}
	for _, protocol := range []codemode.Protocol{codemode.ProtocolResponses, codemode.ProtocolMessages, codemode.ProtocolChat} {
		models := slices.DeleteFunc(slices.Clone(native), func(model string) bool {
			p, ok := offers.paidProtocol(route, model, codeadapter.ClientOpenCode)
			return !ok || p != protocol
		})
		if len(models) == 0 {
			continue
		}
		provider := "olp-paid-" + string(protocol)
		suffix := func() string {
			if protocol == codemode.ProtocolMessages {
				return ""
			}
			return "/v1"
		}()
		sdk := openCodeSDK(protocol)
		definitions := maps.Collect(func(yield func(string, modelDefinition) bool) {
			for _, model := range models {
				if !yield(model, modelDefinition{ID: model, Name: model, Provider: modelProvider{NPM: sdk}}) {
					return
				}
			}
		})
		document.EnabledProviders = append(document.EnabledProviders, provider)
		document.Provider[provider] = settings{Options: options{BaseURL: baseURL + suffix, APIKey: "{env:OLP_API_KEY}"}, Whitelist: models, Models: definitions}
	}
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err := encoder.Encode(document)
	return b.String(), err
}

func openCodeSDK(protocol codemode.Protocol) string {
	switch protocol {
	case codemode.ProtocolResponses:
		return "@ai-sdk/openai"
	case codemode.ProtocolMessages:
		return "@ai-sdk/anthropic"
	default:
		return "@ai-sdk/openai-compatible"
	}
}
