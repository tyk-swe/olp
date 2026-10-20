package codeclient

import (
	"bytes"
	"encoding/json"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
)

// openCodeConfiguration lists each selected model under the provider of the
// adapter it is sent through, which picks its protocol, and every other native
// model under the provider of the first adapter that offers it, and enables
// those providers. A planning model other than the main one becomes the plan
// agent's.
func openCodeConfiguration(adapters []codemode.Adapter, offers Offers, through map[string]codemode.Adapter, native []string, baseURL, model, plan, small string) (string, error) {
	type options struct {
		BaseURL string `json:"baseURL"`
		APIKey  string `json:"apiKey"`
	}
	type settings struct {
		Options   options  `json:"options"`
		Whitelist []string `json:"whitelist"`
	}
	type agent struct {
		Model string `json:"model"`
	}
	whitelists := map[codemode.Adapter][]string{}
	qualified := map[string]string{}
	for _, m := range native {
		adapters := offers.adapters(m)
		if adapter, ok := through[m]; ok {
			adapters = []codemode.Adapter{adapter}
		}
		for _, adapter := range adapters {
			vendor, _ := codeadapter.Lookup(adapter)
			if client, ok := vendor.Client(codeadapter.ClientOpenCode); ok && client.Provider != "" {
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
		Agent            map[string]agent    `json:"agent,omitempty"`
		EnabledProviders []string            `json:"enabled_providers"`
		Provider         map[string]settings `json:"provider"`
	}{Schema: "https://opencode.ai/config.json", Model: qualified[model], SmallModel: qualified[small], Provider: map[string]settings{}}
	if plan != model {
		document.Agent = map[string]agent{"plan": {Model: qualified[plan]}}
	}
	for _, adapter := range adapters {
		if models := whitelists[adapter]; len(models) != 0 {
			vendor, _ := codeadapter.Lookup(adapter)
			client, _ := vendor.Client(codeadapter.ClientOpenCode)
			provider := client.Provider
			document.EnabledProviders = append(document.EnabledProviders, provider)
			document.Provider[provider] = settings{Options: options{BaseURL: baseURL + client.URLSuffix, APIKey: "{env:OLP_API_KEY}"}, Whitelist: models}
		}
	}
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err := encoder.Encode(document)
	return b.String(), err
}
