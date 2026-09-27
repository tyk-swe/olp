// Command reference is the reference provider plugin built on the Go SDK. It
// declares two profiles that serve the OpenAI Chat Completions dialect at a
// fictional upstream: one for API keys, and one for accounts that sign in
// through the upstream's authority, whose grants the plugin enrolls.
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o reference.wasm ./sdk/plugin/reference
package main

import (
	"net/url"
	"slices"

	"github.com/tyk-swe/olp/sdk/plugin"
)

// upstream is the fictional upstream's API base URL, and authority its OAuth
// authorization server. Tests link the plugin against fakes instead:
//
//	-ldflags='-X=main.upstream=http://127.0.0.1:8080/v1 -X=main.authority=http://127.0.0.1:8081'
var (
	upstream  = "https://api.example.com/v1"
	authority = "https://login.example.com"
)

// version labels the build, so tests can link several digests of the plugin.
var version = "0.1.0"

type reference struct{}

func (reference) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Name:        "reference",
		Version:     version,
		Description: "Reference plugin for the OpenLLMProxy plugin SDK.",
		Origins:     slices.Compact([]string{origin(upstream), origin(authority)}),
		Profiles: []plugin.Profile{{
			ID: "reference-chat", Label: "Reference Chat Completions", Dialect: "openai-chat",
			// The upstream takes its API key as a token in the Authorization
			// header and asks clients to identify themselves.
			Hosting: plugin.Hosting{
				Address: upstream,
				Headers: map[string]string{"Authorization": "Token {credential}", "X-Reference-Client": "olp"},
			},
		}, {
			ID: "reference-grant-chat", Label: "Reference Chat Completions with sign-in", Dialect: "openai-chat",
			// An account that signs in sends the grant's access token and
			// names the account the grant authorizes, a grant fact.
			Grant: &plugin.GrantAuthentication{Facts: []string{"account"}},
			Hosting: plugin.Hosting{
				Address: upstream,
				Headers: map[string]string{"Authorization": "Bearer {credential}", "X-Reference-Account": "{grant.account}", "X-Reference-Client": "olp"},
			},
		}},
	}
}

func origin(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		panic("reference: " + address + " is not a URL: " + err.Error())
	}
	return u.Scheme + "://" + u.Host
}

func init() { plugin.Register(reference{}) }

func main() {}
