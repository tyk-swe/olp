// Command reference is the reference provider plugin built on the Go SDK. It
// declares one profile that serves the OpenAI Chat Completions dialect at a
// fictional upstream, with the upstream's model listing and failure
// classification, and the origins that upstream uses.
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o reference.wasm ./sdk/plugin/reference
package main

import (
	"net/url"

	"github.com/tyk-swe/olp/sdk/plugin"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
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
				// It lists its models a page at a time.
				Discovery: &plugin.Discovery{Path: "/models", Models: "data", ID: "id",
					Pagination: &plugin.Pagination{Parameter: "page_token", Cursor: "next_page_token"}},
				// It answers an exhausted quota with its own code on a 400,
				// which is a rate limit rather than a rejected request.
				Classification: []plugin.FailureRule{{Status: 400, Code: "quota_exhausted", Class: abi.ClassRateLimited}},
			},
		}},
	}
}

func init() { plugin.Register(reference{}) }

func main() {}
