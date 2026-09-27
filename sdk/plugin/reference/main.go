// Command reference is the reference provider plugin built on the Go SDK. It
// declares three profiles at a fictional upstream, and the origins that
// upstream uses: one serves the OpenAI Chat Completions dialect with the API
// key in a header, the upstream's model listing and failure classification;
// one serves Gemini generateContent inside the upstream's own envelope; and
// one serves Chat Completions signing each request with the API key.
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o reference.wasm ./sdk/plugin/reference
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strconv"
	"time"

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
			Hosting: plugin.Hosting{
				Address: upstream,
				Headers: headers,
				// It lists its models a page at a time.
				Discovery: &plugin.Discovery{Path: "/models", Models: "data", ID: "id",
					Pagination: &plugin.Pagination{Parameter: "page_token", Cursor: "next_page_token"}},
				// It answers an exhausted quota with its own code on a 400,
				// which is a rate limit rather than a rejected request.
				Classification: []plugin.FailureRule{{Status: 400, Code: "quota_exhausted", Class: abi.ClassRateLimited}},
			},
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
		}, {
			ID: "reference-signed-chat", Label: "Reference Signed Chat Completions", Dialect: "openai-chat",
			// The upstream authenticates each request by a signature made with
			// the API key, which never travels itself.
			Hosting: plugin.Hosting{Address: upstream, Headers: map[string]string{"X-Reference-Client": "olp"}},
			Signing: true,
		}},
	}
}

// Sign signs a request of reference-signed-chat: X-Reference-Signature is the
// hex HMAC-SHA256, keyed with the API key, of the X-Reference-Timestamp value
// (Unix seconds), the method, the request URI and the body, each of the first
// three followed by a line feed.
func (reference) Sign(r plugin.SignRequest) (plugin.SignResult, error) {
	target, err := url.Parse(r.URL)
	if err != nil {
		return plugin.SignResult{}, err
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(r.Credential))
	mac.Write([]byte(timestamp + "\n" + r.Method + "\n" + target.RequestURI() + "\n"))
	mac.Write(r.Body)
	return plugin.SignResult{Headers: map[string]string{
		"X-Reference-Timestamp": timestamp,
		"X-Reference-Signature": hex.EncodeToString(mac.Sum(nil)),
	}}, nil
}

func init() { plugin.Register(reference{}) }

func main() {}
