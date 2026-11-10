package client

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
)

func openCodeConfiguration(route code.Route, adapters []code.Adapter, offers Offers, through map[string]code.Adapter, native []string, baseURL, model, plan, small string) (string, error) {
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
	whitelists := map[code.Adapter][]string{}
	qualified := map[string]string{}
	for _, m := range native {
		if protocol, paid := offers.paidProtocol(route, m, code.ClientOpenCode); paid {
			qualified[m] = "olp-paid-" + string(protocol) + "/" + m
			continue
		}
		candidates := offers.adapters(route, m)
		if adapter, ok := through[m]; ok {
			candidates = []code.Adapter{adapter}
		}
		for _, adapter := range candidates {
			v, _ := vendor.Lookup(adapter)
			resolved, err := route.ResolveModel(code.Operation{Model: m})
			if err != nil || !routeReaches(route, v, code.ClientOpenCode, resolved.Model) {
				continue
			}
			if c, ok := vendor.RouteClient(route, v, code.ClientOpenCode); ok && c.Provider != "" {
				whitelists[adapter] = append(whitelists[adapter], m)
				qualified[m] = c.Provider + "/" + m
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
	}{
		Schema:     "https://opencode.ai/config.json",
		Model:      qualified[model],
		SmallModel: qualified[small],
		Agent:      map[string]agent{"plan": {Model: qualified[plan]}},
		Provider:   map[string]settings{},
	}
	for _, adapter := range adapters {
		if models := whitelists[adapter]; len(models) != 0 {
			v, _ := vendor.Lookup(adapter)
			c, _ := vendor.RouteClient(route, v, code.ClientOpenCode)
			provider := c.Provider
			document.EnabledProviders = append(document.EnabledProviders, provider)
			definitions := maps.Collect(func(yield func(string, modelDefinition) bool) {
				for _, m := range models {
					resolved, _ := route.ResolveModel(code.Operation{Model: m})
					protocol, _ := vendor.RouteClientProtocol(route, v, code.ClientOpenCode, resolved.Model)
					if !yield(m, modelDefinition{ID: m, Name: m, Provider: modelProvider{NPM: openCodeSDK(protocol)}}) {
						return
					}
				}
			})
			document.Provider[provider] = settings{Options: options{BaseURL: baseURL + c.URLSuffix, APIKey: "{env:OLP_API_KEY}"}, Whitelist: models, Models: definitions}
		}
	}
	for _, protocol := range []code.Protocol{code.ProtocolResponses, code.ProtocolMessages, code.ProtocolChat} {
		models := slices.DeleteFunc(slices.Clone(native), func(m string) bool {
			p, ok := offers.paidProtocol(route, m, code.ClientOpenCode)
			return !ok || p != protocol
		})
		if len(models) == 0 {
			continue
		}
		provider := "olp-paid-" + string(protocol)
		suffix := "/v1"
		if protocol == code.ProtocolMessages {
			suffix = ""
		}
		sdk := openCodeSDK(protocol)
		definitions := maps.Collect(func(yield func(string, modelDefinition) bool) {
			for _, m := range models {
				if !yield(m, modelDefinition{ID: m, Name: m, Provider: modelProvider{NPM: sdk}}) {
					return
				}
			}
		})
		document.EnabledProviders = append(document.EnabledProviders, provider)
		document.Provider[provider] = settings{Options: options{BaseURL: baseURL + suffix, APIKey: "{env:OLP_API_KEY}"}, Whitelist: models, Models: definitions}
	}
	return configurationJSON(document)
}

func openCodeSDK(protocol code.Protocol) string {
	switch protocol {
	case code.ProtocolResponses:
		return "@ai-sdk/openai"
	case code.ProtocolMessages:
		return "@ai-sdk/anthropic"
	default:
		return "@ai-sdk/openai-compatible"
	}
}

func qualificationGaps(adapters []code.Adapter, client string) []string {
	var gaps []string
	for _, adapter := range adapters {
		v, _ := vendor.Lookup(adapter)
		for _, note := range v.EnrollmentNotes() {
			if !slices.Contains(gaps, note) {
				gaps = append(gaps, note)
			}
		}
	}
	for _, adapter := range adapters {
		v, _ := vendor.Lookup(adapter)
		gaps = append(gaps, v.Notes()...)
	}
	if len(adapters) > 1 {
		gaps = append(gaps,
			"Each request carries the conversation so far, so every subscription a conversation mixes receives the turns the others produced.",
			"No live vendor is qualified to accept another vendor's reasoning and tool calls in that history; mixed conversations are qualified against scripted vendors only.")
	}
	switch client {
	case code.ClientClaudeCode:
		gaps = append(gaps,
			"Token counting and the connectivity probe are not served; Claude Code falls back to local estimates.",
			"Server tools such as web search and web fetch, and claude.ai login features, are outside code mode.",
			"Each generation is limited to ten minutes; raising API_TIMEOUT_MS cannot extend it.")
	case code.ClientOpenCode:
		gaps = append(gaps,
			"Native Chat Completions usage settles only from the final usage chunk; a stream without it leaves consumption uncertain.")
	}
	for _, adapter := range adapters {
		v, _ := vendor.Lookup(adapter)
		gaps = append(gaps, v.ClientNotes(client)...)
	}
	return gaps
}

type researchServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	OAuth   *bool             `json:"oauth,omitempty"`
	Headers map[string]string `json:"headers"`
}

func addResearchConfiguration(out *code.ClientConfiguration, tools *code.ResearchTools) error {
	if tools == nil {
		return nil
	}
	if err := tools.Validate(); err != nil {
		return err
	}
	servers := map[string]researchServer{}
	for _, service := range tools.Services {
		name := "olp_" + out.RouteSlug + "_" + service
		endpoint := out.BaseURL + "/mcp/" + service
		switch out.Client {
		case code.ClientCodex:
			out.Configuration += fmt.Sprintf("\n[mcp_servers.%s]\nurl = %s\nbearer_token_env_var = \"OLP_API_KEY\"\n", strconv.Quote(name), strconv.Quote(endpoint))
		case code.ClientClaudeCode:
			servers[name] = researchServer{Type: "http", URL: endpoint, Headers: map[string]string{"Authorization": "Bearer ${OLP_API_KEY}"}}
		case code.ClientOpenCode:
			oauth := false
			servers[name] = researchServer{Type: "remote", URL: endpoint, OAuth: &oauth, Headers: map[string]string{"Authorization": "Bearer {env:OLP_API_KEY}"}}
		}
	}
	switch out.Client {
	case code.ClientClaudeCode:
		configuration, err := configurationJSON(struct {
			MCPServers map[string]researchServer `json:"mcpServers"`
		}{MCPServers: servers})
		if err != nil {
			return err
		}
		out.AdditionalFiles = []code.ConfigurationFile{{File: ".mcp.json", Format: "json", Configuration: configuration}}
	case code.ClientOpenCode:
		var document map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out.Configuration), &document); err != nil {
			return err
		}
		encoded, err := json.Marshal(servers)
		if err != nil {
			return err
		}
		document["mcp"] = encoded
		out.Configuration, err = configurationJSON(document)
		return err
	}
	return nil
}

func configurationJSON(value any) (string, error) {
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err := encoder.Encode(value)
	return b.String(), err
}
