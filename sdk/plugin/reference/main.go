// Command reference is the reference provider plugin built on the Go SDK. It
// declares one profile that serves the OpenAI Chat Completions dialect at a
// fictional upstream, and the origins that upstream uses.
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o reference.wasm ./sdk/plugin/reference
package main

import "github.com/tyk-swe/olp/sdk/plugin"

type reference struct{}

func (reference) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Name:        "reference",
		Version:     "0.1.0",
		Description: "Reference plugin for the OpenLLMProxy plugin SDK.",
		Origins:     []string{"https://api.example.com", "https://login.example.com"},
		Profiles: []plugin.Profile{
			{ID: "reference-chat", Label: "Reference Chat Completions", Dialect: "openai-chat"},
		},
	}
}

func init() { plugin.Register(reference{}) }

func main() {}
