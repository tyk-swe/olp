// Command oauth-client-credentials is a template for a provider plugin whose
// upstream authenticates API calls with short-lived access tokens issued to
// OAuth 2.0 clients by the client credentials grant (RFC 6749, section 4.4).
//
// The operator enrolls a grant by pasting the client's ID and secret as
// CLIENT_ID:CLIENT_SECRET. The plugin exchanges them for an access token,
// which OLP's gateways send as a bearer token, and exchanges them again
// before the token expires: the client credentials are the grant's refresh
// token, which OLP stores encrypted and never hands to a gateway.
//
// Copy this directory, replace the constants below with your upstream's, and
// build it as a WASI reactor:
//
//	GOOS=wasip1 GOARCH=wasm CGO_ENABLED=0 go build -trimpath -buildvcs=false -buildmode=c-shared -o plugin.wasm .
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/sdk/plugin"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Replace these with your upstream's.
const (
	// api is the upstream's API base URL, which the dialect's paths extend.
	api = "https://api.example.com/v1"
	// tokenEndpoint is the authorization server's token endpoint.
	tokenEndpoint = "https://login.example.com/oauth2/token"
	// clientsPage is where an operator creates a client and copies its
	// credentials, at one of the plugin's origins.
	clientsPage = "https://login.example.com/clients"
	// scope is the scope the access token needs, or empty for the client's
	// default.
	scope = "inference"
)

// fetch reaches the authorization server; tests replace it.
var fetch = plugin.Fetch

type clientCredentials struct{}

func init() { plugin.Register(clientCredentials{}) }

func main() {}

func (clientCredentials) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Name:        "oauth-client-credentials",
		Version:     "0.1.0",
		Description: "Chat Completions with access tokens from the OAuth 2.0 client credentials grant.",
		Origins:     []string{"https://api.example.com", "https://login.example.com"},
		Profiles: []plugin.Profile{{
			ID: "client-credentials-chat", Label: "Chat Completions, OAuth client", Dialect: "openai-chat",
			Grant:   &plugin.GrantAuthentication{Input: abi.GrantInputSecret},
			Hosting: plugin.Hosting{Address: api, Headers: map[string]string{"Authorization": "Bearer {credential}"}},
		}},
	}
}

// StartGrant sends the operator to the page that issues client credentials.
func (clientCredentials) StartGrant(context.Context, plugin.GrantStart) (plugin.GrantAuthorization, error) {
	return plugin.GrantAuthorization{URL: clientsPage}, nil
}

// ExchangeGrant exchanges the pasted CLIENT_ID:CLIENT_SECRET for a grant.
func (clientCredentials) ExchangeGrant(ctx context.Context, exchange plugin.GrantExchange) (plugin.Grant, error) {
	return issue(ctx, strings.TrimSpace(exchange.Input))
}

// RefreshGrant runs the client credentials grant again: the grant's refresh
// token is the client credentials.
func (clientCredentials) RefreshGrant(ctx context.Context, refresh plugin.GrantRefresh) (plugin.Grant, error) {
	grant, err := issue(ctx, refresh.RefreshToken)
	// A refresh keeps the refresh token it was given.
	grant.RefreshToken = ""
	return grant, err
}

// issue requests an access token for the client credentials, sent with HTTP
// Basic authentication as RFC 6749 recommends.
func issue(ctx context.Context, credentials string) (plugin.Grant, error) {
	id, secret, ok := strings.Cut(credentials, ":")
	if !ok || id == "" || secret == "" {
		return plugin.Grant{}, &abi.Error{Code: abi.CodeInvalidRequest, Message: "Paste the client credentials as CLIENT_ID:CLIENT_SECRET."}
	}
	form := url.Values{"grant_type": {"client_credentials"}}
	if scope != "" {
		form.Set("scope", scope)
	}
	basic := base64.StdEncoding.EncodeToString([]byte(url.QueryEscape(id) + ":" + url.QueryEscape(secret)))
	response, err := fetch(ctx, plugin.HTTPRequest{
		Method: http.MethodPost, URL: tokenEndpoint, Body: []byte(form.Encode()),
		Header: map[string][]string{"Authorization": {"Basic " + basic}, "Content-Type": {"application/x-www-form-urlencoded"}, "Accept": {"application/json"}},
	})
	if err != nil {
		return plugin.Grant{}, err
	}
	var token struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(response.Body, &token)
	switch {
	case token.Error == "invalid_client" || token.Error == "unauthorized_client" || response.Status == http.StatusUnauthorized:
		return plugin.Grant{}, &abi.Error{Code: abi.CodeInvalidGrant, Message: "The authorization server refused the client credentials."}
	case response.Status != http.StatusOK || token.AccessToken == "":
		return plugin.Grant{}, &abi.Error{Code: abi.CodeInternal, Message: "The authorization server issued no access token."}
	}
	return plugin.Grant{AccessToken: token.AccessToken, RefreshToken: credentials, ExpiresIn: token.ExpiresIn, Principal: id}, nil
}
