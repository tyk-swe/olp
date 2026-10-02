package codexauth

import (
	"context"
	"encoding/json"
	"maps"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Adapter uses the plugin host's origin-restricted, redirect-free HTTP capability.
type Adapter struct {
	Fetch func(context.Context, abi.HTTPRequest) (abi.HTTPResponse, error)
}

func (Adapter) Manifest() abi.Manifest { return Manifest() }

type deviceSession struct {
	DeviceID string `json:"device_auth_id"`
	UserCode string `json:"user_code"`
}

func (a Adapter) StartGrant(ctx context.Context, start abi.GrantStart) (abi.GrantAuthorization, error) {
	if start.Profile != ProfileID {
		return abi.GrantAuthorization{}, invalid()
	}
	var issued struct {
		DeviceID string `json:"device_auth_id"`
		UserCode string `json:"user_code"`
		Usercode string `json:"usercode"`
		Interval string `json:"interval"`
	}
	err := a.jsonCall(ctx, "/api/accounts/deviceauth/usercode", map[string]string{"client_id": ClientID}, &issued, false)
	if err != nil {
		return abi.GrantAuthorization{}, err
	}
	if issued.UserCode == "" {
		issued.UserCode = issued.Usercode
	}
	interval := int64(5)
	if issued.Interval != "" {
		interval, err = strconv.ParseInt(strings.TrimSpace(issued.Interval), 10, 64)
	}
	if err != nil || interval < 1 || interval > 300 || !identifier(issued.DeviceID) || !identifier(issued.UserCode) || len(issued.UserCode) > 64 {
		return abi.GrantAuthorization{}, invalid()
	}
	state, _ := json.Marshal(deviceSession{DeviceID: issued.DeviceID, UserCode: issued.UserCode})
	return abi.GrantAuthorization{Session: string(state), Device: &abi.DeviceAuthorization{
		VerificationURL: Issuer + "/codex/device", UserCode: issued.UserCode, Interval: interval, ExpiresIn: 15 * 60,
	}}, nil
}

func (Adapter) ExchangeGrant(context.Context, abi.GrantExchange) (abi.Grant, error) {
	return abi.Grant{}, &abi.Error{Code: abi.CodeInvalidRequest, Message: "Approve the device code upstream and poll this enrollment; no callback or token is accepted."}
}

func (a Adapter) PollGrant(ctx context.Context, poll abi.GrantPoll) (abi.Grant, error) {
	var state deviceSession
	if poll.Profile != ProfileID || len(poll.Session) > 16<<10 || json.Unmarshal([]byte(poll.Session), &state) != nil || !identifier(state.DeviceID) || !identifier(state.UserCode) {
		return abi.Grant{}, invalid()
	}
	var issued struct {
		Code     string `json:"authorization_code"`
		Verifier string `json:"code_verifier"`
	}
	if err := a.jsonCall(ctx, "/api/accounts/deviceauth/token", state, &issued, true); err != nil {
		return abi.Grant{}, err
	}
	if issued.Code == "" || issued.Verifier == "" {
		return abi.Grant{}, invalid()
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {issued.Code}, "redirect_uri": {Issuer + "/deviceauth/callback"}, "client_id": {ClientID}, "code_verifier": {issued.Verifier}}
	var tokens tokenResponse
	if err := a.call(ctx, "/oauth/token", "application/x-www-form-urlencoded", []byte(form.Encode()), &tokens, false); err != nil {
		return abi.Grant{}, err
	}
	if tokens.IDToken == "" || tokens.RefreshToken == "" {
		return abi.Grant{}, invalid()
	}
	return tokens.grant(time.Now())
}

func (a Adapter) RefreshGrant(ctx context.Context, refresh abi.GrantRefresh) (abi.Grant, error) {
	if refresh.Profile != ProfileID || refresh.RefreshToken == "" || len(refresh.RefreshToken) > 16<<10 {
		return abi.Grant{}, invalid()
	}
	var tokens tokenResponse
	if err := a.jsonCall(ctx, "/oauth/token", map[string]string{"grant_type": "refresh_token", "refresh_token": refresh.RefreshToken, "client_id": ClientID}, &tokens, false); err != nil {
		return abi.Grant{}, err
	}
	grant, err := tokens.grant(time.Now())
	if err == nil && !maps.Equal(grant.Facts, refresh.Facts) {
		return abi.Grant{}, &abi.Error{Code: abi.CodeInvalidGrant, Message: "Refreshed Codex authorization names a different account or user; enroll again."}
	}
	return grant, err
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
}

func (t tokenResponse) grant(now time.Time) (abi.Grant, error) {
	i, err := TokenIdentity(t.AccessToken, now)
	if err != nil {
		return abi.Grant{}, invalid()
	}
	if t.IDToken != "" {
		id, err := TokenIdentity(t.IDToken, now)
		if err != nil || id.Principal() != i.Principal() {
			return abi.Grant{}, invalid()
		}
	}
	if len(t.RefreshToken) > 16<<10 || strings.ContainsAny(t.RefreshToken, "\r\n\x00") {
		return abi.Grant{}, invalid()
	}
	return abi.Grant{AccessToken: t.AccessToken, RefreshToken: t.RefreshToken, Principal: i.Principal(), Facts: i.Facts(), ExpiresIn: i.ExpiresAt.Unix() - now.Unix()}, nil
}

func (a Adapter) jsonCall(ctx context.Context, path string, input, output any, pending bool) error {
	body, err := json.Marshal(input)
	if err != nil {
		return invalid()
	}
	return a.call(ctx, path, "application/json", body, output, pending)
}

func (a Adapter) call(ctx context.Context, path, contentType string, body []byte, output any, pending bool) error {
	response, err := a.Fetch(ctx, abi.HTTPRequest{Method: "POST", URL: Issuer + path, Header: map[string][]string{"Content-Type": {contentType}}, Body: body})
	if err != nil {
		return &abi.Error{Code: abi.CodeHTTPFailed, Message: "Codex authorization request did not complete."}
	}
	if pending && (response.Status == 403 || response.Status == 404) {
		return &abi.Error{Code: abi.CodeAuthorizationPending, Message: "Codex device authorization is pending."}
	}
	if response.Status != 200 {
		code := "codex_authorization_failed"
		var refusal struct {
			Error json.RawMessage `json:"error"`
		}
		var detail struct {
			Code string `json:"code"`
		}
		var reported string
		if json.Unmarshal(response.Body, &refusal) == nil {
			if json.Unmarshal(refusal.Error, &reported) != nil && json.Unmarshal(refusal.Error, &detail) == nil {
				reported = detail.Code
			}
		}
		switch strings.ToLower(reported) {
		case "invalid_grant", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
			code = abi.CodeInvalidGrant
		}
		if response.Status == 401 {
			code = abi.CodeInvalidGrant
		}
		return &abi.Error{Code: code, Message: "Codex authorization endpoint refused the request."}
	}
	if len(response.Body) > 64<<10 || json.Unmarshal(response.Body, output) != nil {
		return invalid()
	}
	return nil
}

func invalid() error {
	return &abi.Error{Code: "codex_authorization_invalid", Message: "Codex authorization response or identity is unsupported; enroll again."}
}
