package main

import (
	"context"
	"encoding/json"
	"net/url"

	"github.com/tyk-swe/olp/sdk/plugin"
)

// deviceProfile is the ID of the profile whose grants the authority issues by
// device authorization.
const deviceProfile = "reference-device-chat"

// deviceGrantType is the token request's grant type in the OAuth 2.0 device
// authorization grant (RFC 8628).
const deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"

// deviceChat serves Chat Completions, like reference-grant-chat, for accounts
// that sign in on another device: the operator enters a user code on the
// authority's verification page and approves, while OLP polls the authority
// through the plugin. Its grants name the account and the API that serves it,
// and refresh, like reference-grant-chat's.
func deviceChat() plugin.Profile {
	return plugin.Profile{
		ID: deviceProfile, Label: "Reference Chat Completions with device sign-in", Dialect: "openai-chat",
		Grant: &plugin.GrantAuthentication{Facts: []string{"account", "api_base"}},
		Hosting: plugin.Hosting{
			Address: "{grant.api_base}",
			Headers: map[string]string{"Authorization": "Bearer {credential}", "X-Reference-Account": "{grant.account}", "X-Reference-Client": "olp"},
		},
	}
}

// deviceSession is what a device authorization carries from StartGrant to
// each PollGrant.
type deviceSession struct {
	DeviceCode string `json:"device_code"`
}

// startDeviceAuthorization requests a device authorization from the
// authority, whose user code the operator enters on its verification page.
func startDeviceAuthorization(ctx context.Context) (plugin.GrantAuthorization, error) {
	var issued struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int64  `json:"expires_in"`
		Interval        int64  `json:"interval"`
	}
	if err := call(ctx, "POST", authority+"/device/code", url.Values{"client_id": {clientID}}, "", &issued); err != nil {
		return plugin.GrantAuthorization{}, err
	}
	data, err := json.Marshal(deviceSession{DeviceCode: issued.DeviceCode})
	return plugin.GrantAuthorization{
		Device: &plugin.DeviceAuthorization{
			VerificationURL: issued.VerificationURI, UserCode: issued.UserCode, ExpiresIn: issued.ExpiresIn, Interval: issued.Interval,
		},
		Session: string(data),
	}, err
}

// PollGrant asks the authority's token endpoint for the device's grant. Until
// the operator approves, the authority refuses with the RFC 8628 codes OLP
// expects, which call reports as they are.
func (reference) PollGrant(ctx context.Context, poll plugin.GrantPoll) (plugin.Grant, error) {
	var s deviceSession
	if err := json.Unmarshal([]byte(poll.Session), &s); err != nil {
		return plugin.Grant{}, err
	}
	return issue(ctx, url.Values{"grant_type": {deviceGrantType}, "device_code": {s.DeviceCode}, "client_id": {clientID}}, upstream)
}
