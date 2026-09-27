package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/sdk/plugin"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// The authority grants access with the OAuth 2.0 authorization code flow and
// PKCE (RFC 7636). It returns the operator's browser to a loopback address
// where nothing listens, so the operator pastes the callback URL from the
// address bar into OLP; or, for an operator on another machine, it displays
// the code as code#state to paste instead.
const (
	clientID = "olp-reference"
	redirect = "http://127.0.0.1:1455/callback"
)

// session is what grant enrollment carries from StartGrant to ExchangeGrant.
type session struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
}

func (reference) StartGrant(_ context.Context, start plugin.GrantStart) (plugin.GrantAuthorization, error) {
	if start.Profile == deviceProfile {
		return startDeviceAuthorization()
	}
	s := session{State: random(), Verifier: random()}
	challenge := sha256.Sum256([]byte(s.Verifier))
	query := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "state": {s.State},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"},
	}
	data, err := json.Marshal(s)
	return plugin.GrantAuthorization{URL: authority + "/authorize?" + query.Encode(), Session: string(data)}, err
}

func (reference) ExchangeGrant(_ context.Context, exchange plugin.GrantExchange) (plugin.Grant, error) {
	var s session
	if err := json.Unmarshal([]byte(exchange.Session), &s); err != nil {
		return plugin.Grant{}, err
	}
	code, err := authorizationCode(exchange.Input, s.State)
	if err != nil {
		return plugin.Grant{}, err
	}
	return token(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "client_id": {clientID}, "code_verifier": {s.Verifier}})
}

// token requests a grant from the authority's token endpoint and asks the
// authority who signed in.
func token(form url.Values) (plugin.Grant, error) {
	var issued struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Account      string `json:"account"`
	}
	if err := call("POST", authority+"/token", form, "", &issued); err != nil {
		return plugin.Grant{}, err
	}
	// The token response names the account; who signed in is the
	// authority's to say.
	var user struct {
		Subject string `json:"sub"`
	}
	if err := call("GET", authority+"/userinfo", nil, issued.AccessToken, &user); err != nil {
		return plugin.Grant{}, err
	}
	return plugin.Grant{
		AccessToken: issued.AccessToken, RefreshToken: issued.RefreshToken, ExpiresIn: issued.ExpiresIn,
		Principal: user.Subject, Facts: map[string]string{"account": issued.Account},
	}, nil
}

// authorizationCode reads the authorization code from what the operator
// pasted: the callback URL, or the code#state the authority displays.
func authorizationCode(input, state string) (string, error) {
	input = strings.TrimSpace(input)
	var code, returned string
	if callback, err := url.Parse(input); err == nil && callback.Scheme != "" {
		query := callback.Query()
		if refusal := query.Get("error"); refusal != "" {
			return "", &plugin.Error{Code: refusal, Message: "The authority did not authorize the account: " + query.Get("error_description")}
		}
		code, returned = query.Get("code"), query.Get("state")
	} else {
		code, returned, _ = strings.Cut(input, "#")
	}
	if returned != state {
		return "", &plugin.Error{Code: abi.CodeStateMismatch, Message: "The pasted value answers another sign-in. Paste the one this sign-in returned."}
	}
	if code == "" {
		return "", &plugin.Error{Code: "invalid_input", Message: "Paste the callback URL, or the code the authority displayed."}
	}
	return code, nil
}

// call sends a request to the authority through OLP and decodes its JSON
// reply. A refusal fails with the OAuth error code the authority reported,
// such as a device authorization's authorization_pending, which is the code
// OLP expects.
func call(method, address string, form url.Values, bearer string, reply any) error {
	request := plugin.HTTPRequest{Method: method, URL: address, Header: map[string][]string{"Accept": {"application/json"}}}
	if form != nil {
		request.Header["Content-Type"] = []string{"application/x-www-form-urlencoded"}
		request.Body = []byte(form.Encode())
	}
	if bearer != "" {
		request.Header["Authorization"] = []string{"Bearer " + bearer}
	}
	response, err := plugin.Fetch(request)
	if err != nil {
		return err
	}
	if response.Status != 200 {
		var refusal struct {
			Code        string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(response.Body, &refusal) == nil && refusal.Code != "" {
			return &plugin.Error{Code: refusal.Code, Message: "The authority refused: " + refusal.Description}
		}
		return &plugin.Error{Code: "authority_failed", Message: "The authority answered with status " + strconv.Itoa(response.Status) + "."}
	}
	return json.Unmarshal(response.Body, reply)
}

// random returns 256 random bits, URL-safe encoded: an unguessable state or
// PKCE verifier.
func random() string {
	var b [32]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}
