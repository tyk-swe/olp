// Command signed-request is a template for a provider plugin whose upstream
// authenticates each request by a signature rather than a bearer token: the
// secret never travels, so a captured request cannot be replayed against
// another path, body or time.
//
// The operator stores the static credential KEY_ID:SECRET. For every
// upstream request OLP calls Sign, which adds the key ID, the request time and
// an HMAC-SHA256 of a canonical form of the request, keyed with the secret.
//
// Copy this directory, replace the constants and canonicalRequest with your
// upstream's scheme, and build it as a WASI reactor:
//
//	GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=c-shared -o plugin.wasm .
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strings"
	"time"

	"github.com/tyk-swe/olp/sdk/plugin"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// api is the upstream's API base URL; replace it with yours.
const api = "https://api.example.com/v1"

// now is the signing clock; tests fix it.
var now = time.Now

type signedRequest struct{}

func init() { plugin.Register(signedRequest{}) }

func main() {}

func (signedRequest) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Name:        "signed-request",
		Version:     "0.1.0",
		Description: "Chat Completions authenticated by an HMAC request signature.",
		Origins:     []string{"https://api.example.com"},
		Profiles: []plugin.Profile{{
			ID: "signed-chat", Label: "Chat Completions, signed requests", Dialect: "openai-chat",
			Hosting: plugin.Hosting{Address: api},
			Signing: true,
		}},
	}
}

// Sign adds X-Api-Key-Id, X-Api-Date and X-Api-Signature: the hex
// HMAC-SHA256, keyed with the secret, of the canonical request.
func (signedRequest) Sign(_ context.Context, r plugin.SignRequest) (plugin.SignResult, error) {
	keyID, secret, ok := strings.Cut(r.Credential, ":")
	if !ok || keyID == "" || secret == "" {
		return plugin.SignResult{}, &abi.Error{Code: abi.CodeInvalidRequest, Message: "Store the credential as KEY_ID:SECRET."}
	}
	date := now().UTC().Format("20060102T150405Z")
	canonical, err := canonicalRequest(r.Method, r.URL, date, r.Body)
	if err != nil {
		return plugin.SignResult{}, err
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	return plugin.SignResult{Headers: map[string]string{
		"X-Api-Key-Id":    keyID,
		"X-Api-Date":      date,
		"X-Api-Signature": hex.EncodeToString(mac.Sum(nil)),
	}}, nil
}

// canonicalRequest is the text a signature covers, one line each: the method,
// the escaped path, the query sorted by name, the request time and the hex
// SHA-256 of the body. Sorting the query makes the signature independent of
// the order a client wrote it in.
func canonicalRequest(method, rawURL, date string, body []byte) (string, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(body)
	return strings.Join([]string{method, target.EscapedPath(), target.Query().Encode(), date, hex.EncodeToString(digest[:])}, "\n"), nil
}
