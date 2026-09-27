// Command reference is the reference provider plugin built on the Go SDK. It
// declares two profiles that serve the OpenAI Chat Completions dialect at a
// fictional upstream, one of them at a workspace the operator names, and the
// origins that upstream uses.
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o reference.wasm ./sdk/plugin/reference
package main

import (
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
	return plugin.Manifest{
		Name:        "reference",
		Version:     version,
		Description: "Reference plugin for the OpenLLMProxy plugin SDK.",
		Origins:     []string{api.Scheme + "://" + api.Host, "https://login.example.com"},
		Profiles: []plugin.Profile{{
			ID: "reference-chat", Label: "Reference Chat Completions", Dialect: "openai-chat",
			// The upstream takes its API key as a token in the Authorization
			// header and asks clients to identify themselves.
			Hosting: plugin.Hosting{
				Address: upstream,
				Headers: map[string]string{"Authorization": "Token {credential}", "X-Reference-Client": "olp"},
			},
		}, {
			ID: "reference-workspace-chat", Label: "Reference workspace Chat Completions", Dialect: "openai-chat",
			// Each workspace has its own API under the upstream's, which the
			// provider's workspace option places.
			Options: []plugin.Option{{
				Name: "workspace", Label: "Workspace", Description: "The upstream workspace that serves this provider.",
				Pattern: "^[a-z0-9][a-z0-9-]{0,39}$",
			}},
			Hosting: plugin.Hosting{
				Address: upstream + "/workspaces/{options.workspace}",
				Headers: map[string]string{"Authorization": "Token {credential}", "X-Reference-Client": "olp"},
			},
		}},
	}
}

func init() { plugin.Register(reference{}) }

func main() {}
