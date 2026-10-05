// Command token-exchange is a template for a provider plugin whose upstream
// takes short-lived bearer tokens that an authorization server issues in
// exchange for a long-lived API key, as OAuth 2.0 token exchange (RFC 8693)
// describes and cloud IAM services commonly do.
//
// The operator enrolls a grant by pasting the API key. The plugin exchanges it
// for an access token, which OLP's gateways send as a bearer token, and
// exchanges it again before the token expires: the API key is the grant's
// refresh token, which OLP stores encrypted and never hands to a gateway.
//
// Copy this directory, replace the constants below with your upstream's, and
// build it as a WASI reactor:
//
//	GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=c-shared -o plugin.wasm .
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/sdk/plugin"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Replace these with your upstream's.
const (
	api           = "https://api.example.com/v1"
	tokenEndpoint = "https://iam.example.com/identity/token"
	// keysPage is where an operator creates an API key, at one of the
	// plugin's origins.
	keysPage = "https://iam.example.com/api-keys"
	// subjectTokenType names what the subject token is; RFC 8693 lets an
	// authorization server define its own type for an API key.
	subjectTokenType = "urn:example:params:oauth:token-type:api-key"
)

// fetch reaches the authorization server; tests replace it.
var fetch = plugin.Fetch

type tokenExchange struct{}

func init() { plugin.Register(tokenExchange{}) }

func main() {}

func (tokenExchange) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Name:        "token-exchange",
		Version:     "0.1.0",
		Description: "Chat Completions with bearer tokens exchanged for an API key.",
		Origins:     []string{"https://api.example.com", "https://iam.example.com"},
		Profiles: []plugin.Profile{{
			ID: "token-exchange-chat", Label: "Chat Completions, exchanged token", Dialect: "openai-chat",
			Grant:   &plugin.GrantAuthentication{Input: abi.GrantInputSecret},
			Hosting: plugin.Hosting{Address: api, Headers: map[string]string{"Authorization": "Bearer {credential}"}},
		}},
	}
}

// StartGrant sends the operator to the page that issues API keys.
func (tokenExchange) StartGrant(context.Context, plugin.GrantStart) (plugin.GrantAuthorization, error) {
	return plugin.GrantAuthorization{URL: keysPage}, nil
}

// ExchangeGrant exchanges the pasted API key for a grant.
func (tokenExchange) ExchangeGrant(ctx context.Context, exchange plugin.GrantExchange) (plugin.Grant, error) {
	return exchangeKey(ctx, strings.TrimSpace(exchange.Input))
}

// RefreshGrant exchanges the API key again: it is the grant's refresh token.
func (tokenExchange) RefreshGrant(ctx context.Context, refresh plugin.GrantRefresh) (plugin.Grant, error) {
	grant, err := exchangeKey(ctx, refresh.RefreshToken)
	grant.RefreshToken = ""
	return grant, err
}

func exchangeKey(ctx context.Context, key string) (plugin.Grant, error) {
	if key == "" {
		return plugin.Grant{}, &abi.Error{Code: abi.CodeInvalidRequest, Message: "Paste the API key."}
	}
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {key},
		"subject_token_type":   {subjectTokenType},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
	}
	response, err := fetch(ctx, plugin.HTTPRequest{
		Method: http.MethodPost, URL: tokenEndpoint, Body: []byte(form.Encode()),
		Header: map[string][]string{"Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}},
	})
	if err != nil {
		return plugin.Grant{}, err
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		// Account names the upstream account, where the authorization
		// server reports one.
		Account string `json:"account"`
		Error   string `json:"error"`
	}
	_ = json.Unmarshal(response.Body, &token)
	switch {
	case token.Error == "invalid_grant" || token.Error == "invalid_request" && response.Status == http.StatusBadRequest || response.Status == http.StatusUnauthorized:
		return plugin.Grant{}, &abi.Error{Code: abi.CodeInvalidGrant, Message: "The authorization server refused the API key."}
	case response.Status != http.StatusOK || token.AccessToken == "":
		return plugin.Grant{}, &abi.Error{Code: abi.CodeInternal, Message: "The authorization server issued no access token."}
	}
	principal := token.Account
	if principal == "" {
		// Without a reported account, the key itself identifies who the
		// grant authorizes, by a digest that reveals nothing of it.
		digest := sha256.Sum256([]byte(key))
		principal = "key-" + hex.EncodeToString(digest[:8])
	}
	return plugin.Grant{AccessToken: token.AccessToken, RefreshToken: key, ExpiresIn: token.ExpiresIn, Principal: principal}, nil
}
