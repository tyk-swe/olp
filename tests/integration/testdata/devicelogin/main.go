// Command devicelogin is a provider plugin whose upstream runs its own
// variant of device authorization rather than RFC 8628, as ChatGPT Codex's
// device login does: it issues a user code for a JSON request, answers polls
// with 403 until the operator approves, and then with an authorization code
// and the PKCE verifier to exchange it with. The plugin maps the variant onto
// OLP's grant enrollment steps, which is all it takes.
//
//	-ldflags=-X=main.authority=http://127.0.0.1:8081
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"strconv"

	"github.com/tyk-swe/olp/sdk/plugin"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// authority is the upstream's authorization server.
var authority = "https://auth.example.com"

const clientID = "olp-devicelogin"

type devicelogin struct{}

func (devicelogin) Manifest() plugin.Manifest {
	return plugin.Manifest{
		Name: "devicelogin", Version: "1.0.0", Origins: []string{authority},
		Profiles: []plugin.Profile{{
			ID: "devicelogin-chat", Label: "Device login Chat Completions", Dialect: "openai-chat",
			Grant:   &plugin.GrantAuthentication{},
			Hosting: plugin.Hosting{Address: authority + "/v1", Headers: map[string]string{"Authorization": "Bearer {credential}"}},
		}},
	}
}

type session struct {
	DeviceAuthID string `json:"device_auth_id"`
	UserCode     string `json:"user_code"`
}

func (devicelogin) StartGrant(context.Context, plugin.GrantStart) (plugin.GrantAuthorization, error) {
	var issued struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
		// The variant states its interval as a string.
		Interval string `json:"interval"`
	}
	if _, err := send("/api/accounts/deviceauth/usercode", "application/json", map[string]string{"client_id": clientID}, &issued); err != nil {
		return plugin.GrantAuthorization{}, err
	}
	interval, err := strconv.ParseInt(issued.Interval, 10, 64)
	if err != nil {
		return plugin.GrantAuthorization{}, err
	}
	data, err := json.Marshal(session{DeviceAuthID: issued.DeviceAuthID, UserCode: issued.UserCode})
	return plugin.GrantAuthorization{
		Device:  &plugin.DeviceAuthorization{VerificationURL: authority + "/codex/device", UserCode: issued.UserCode, ExpiresIn: 900, Interval: interval},
		Session: string(data),
	}, err
}

func (devicelogin) ExchangeGrant(context.Context, plugin.GrantExchange) (plugin.Grant, error) {
	return plugin.Grant{}, &plugin.Error{Code: "device_login_only", Message: "Accounts sign in by device login."}
}

// PollGrant reports a 403 or 404 as a pending authorization. Once approved,
// it exchanges the authorization code the upstream returns.
func (devicelogin) PollGrant(_ context.Context, poll plugin.GrantPoll) (plugin.Grant, error) {
	var s session
	if err := json.Unmarshal([]byte(poll.Session), &s); err != nil {
		return plugin.Grant{}, err
	}
	var approved struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	status, err := send("/api/accounts/deviceauth/token", "application/json", s, &approved)
	switch {
	case status == 403 || status == 404:
		return plugin.Grant{}, &plugin.Error{Code: abi.CodeAuthorizationPending, Message: "The operator has not approved the device yet."}
	case err != nil:
		return plugin.Grant{}, err
	}
	var issued struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		AccountID    string `json:"account_id"`
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {approved.AuthorizationCode}, "code_verifier": {approved.CodeVerifier},
		"redirect_uri": {authority + "/deviceauth/callback"}, "client_id": {clientID},
	}
	if _, err = send("/oauth/token", "application/x-www-form-urlencoded", form, &issued); err != nil {
		return plugin.Grant{}, err
	}
	return plugin.Grant{AccessToken: issued.AccessToken, RefreshToken: issued.RefreshToken, ExpiresIn: issued.ExpiresIn, Principal: issued.AccountID}, nil
}

// send posts a JSON body, or a form, to the authority through OLP and decodes
// its JSON reply. It fails for a reply other than 200, returning its status.
func send(path, contentType string, body, reply any) (int, error) {
	var data []byte
	if form, ok := body.(url.Values); ok {
		data = []byte(form.Encode())
	} else {
		var err error
		if data, err = json.Marshal(body); err != nil {
			return 0, err
		}
	}
	response, err := plugin.Fetch(plugin.HTTPRequest{Method: "POST", URL: authority + path, Header: map[string][]string{"Content-Type": {contentType}}, Body: data})
	switch {
	case err != nil:
		return 0, err
	case response.Status != 200:
		return response.Status, &plugin.Error{Code: "authority_failed", Message: "The authority answered with status " + strconv.Itoa(response.Status) + "."}
	}
	return response.Status, json.NewDecoder(bytes.NewReader(response.Body)).Decode(reply)
}

func init() { plugin.Register(devicelogin{}) }

func main() {}
