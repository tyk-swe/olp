// Command reference is the reference provider plugin built on the Go SDK. It
// declares two profiles at a fictional upstream, and the origins that upstream
// uses: one serves the OpenAI Chat Completions dialect, and one serves Gemini
// generateContent inside the upstream's own envelope.
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o reference.wasm ./sdk/plugin/reference
package main

import (
	"encoding/json"
	"net/url"

	"github.com/tyk-swe/olp/sdk/plugin"
)

// upstream is the fictional upstream's API base URL. Tests link the plugin
// against a fake upstream instead:
//
//	-ldflags=-X=main.upstream=http://127.0.0.1:8080/v1
var upstream = "https://api.example.com/v1"

// version labels the build, so tests can link several digests of the plugin.
var version = "0.1.0"

type reference struct{}

func (reference) Manifest() plugin.Manifest {
	api, err := url.Parse(upstream)
	if err != nil {
		panic("reference: upstream is not a URL: " + err.Error())
	}
	origin := api.Scheme + "://" + api.Host
	// The upstream takes its API key as a token in the Authorization header
	// and asks clients to identify themselves.
	headers := map[string]string{"Authorization": "Token {credential}", "X-Reference-Client": "olp"}
	return plugin.Manifest{
		Name:        "reference",
		Version:     version,
		Description: "Reference plugin for the OpenLLMProxy plugin SDK.",
		Origins:     []string{origin, "https://login.example.com"},
		Profiles: []plugin.Profile{{
			ID: "reference-chat", Label: "Reference Chat Completions", Dialect: "openai-chat",
			Hosting: plugin.Hosting{Address: upstream, Headers: headers},
		}, {
			ID: "reference-gemini", Label: "Reference Gemini, enveloped", Dialect: "gemini-generate-content",
			// The upstream also serves Gemini generateContent, wrapping each
			// request as {"model": ..., "request": {...}} and each response
			// and stream event as {"response": {...}}. It serves one
			// candidate, wants a system instruction and refuses seeds.
			Hosting: plugin.Hosting{
				Address: origin + "/enveloped/v1beta",
				Headers: headers,
				Envelope: &plugin.Envelope{
					Request:  "request",
					Fields:   map[string]string{"model": "{model}"},
					Response: "response",
				},
				Rewrites: []plugin.Rewrite{
					{Op: plugin.RewriteSet, Path: "/generationConfig/candidateCount", Value: json.RawMessage(`1`)},
					{Op: plugin.RewriteDefault, Path: "/systemInstruction", Value: json.RawMessage(`{"parts":[{"text":"You are the reference assistant."}]}`)},
					{Op: plugin.RewriteDelete, Path: "/generationConfig/seed"},
				},
			},
		}},
	}
}

func init() { plugin.Register(reference{}) }

func main() {}
