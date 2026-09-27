// Command reference is the reference provider plugin built on the Go SDK. It
// declares profiles that serve the OpenAI Chat Completions dialect at a
// fictional upstream, and the origins that upstream uses: one places the API
// key in a header, and one signs each request with it.
//
//	GOOS=wasip1 GOARCH=wasm go build -buildmode=c-shared -o reference.wasm ./sdk/plugin/reference
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"time"

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
