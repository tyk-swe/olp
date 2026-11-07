package grokauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/plugin/abi"
)

type Adapter struct {
	Fetch func(context.Context, abi.HTTPRequest) (abi.HTTPResponse, error)
}

func (Adapter) Manifest() abi.Manifest { return Manifest() }

func (a Adapter) StartGrant(ctx context.Context, start abi.GrantStart) (abi.GrantAuthorization, error) {
	if start.Profile != ProfileID {
		return abi.GrantAuthorization{}, invalid()
	}
	var device struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int64  `json:"expires_in"`
		Interval        int64  `json:"interval"`
	}
	if err := a.form(ctx, "/oauth2/device/code", url.Values{"client_id": {ClientID}, "scope": {"openid profile email offline_access grok-cli:access api:access"}}, &device); err != nil {
		return abi.GrantAuthorization{}, err
	}
	verification, err := url.Parse(device.VerificationURI)
	if err != nil || verification.Scheme != "https" || verification.Host != "auth.x.ai" || verification.User != nil || verification.Fragment != "" || !credential(device.DeviceCode) || !credential(device.UserCode) || len(device.UserCode) > 64 || device.ExpiresIn <= 0 || device.ExpiresIn > 1800 || device.Interval < 0 || device.Interval > 300 {
		return abi.GrantAuthorization{}, invalid()
	}
	return abi.GrantAuthorization{Session: device.DeviceCode, Device: &abi.DeviceAuthorization{VerificationURL: device.VerificationURI, UserCode: device.UserCode, ExpiresIn: device.ExpiresIn, Interval: device.Interval}}, nil
}

func (Adapter) ExchangeGrant(context.Context, abi.GrantExchange) (abi.Grant, error) {
	return abi.Grant{}, &abi.Error{Code: abi.CodeInvalidRequest, Message: "Approve the device upstream and poll this enrollment."}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
}

func (a Adapter) PollGrant(ctx context.Context, poll abi.GrantPoll) (abi.Grant, error) {
	if poll.Profile != ProfileID || !credential(poll.Session) {
		return abi.Grant{}, invalid()
	}
	var tokens tokenResponse
	if err := a.form(ctx, "/oauth2/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "client_id": {ClientID}, "device_code": {poll.Session}}, &tokens); err != nil {
		return abi.Grant{}, err
	}
	if !credential(tokens.RefreshToken) {
		return abi.Grant{}, invalid()
	}
	return a.grant(ctx, tokens)
}

func (a Adapter) RefreshGrant(ctx context.Context, refresh abi.GrantRefresh) (abi.Grant, error) {
	if refresh.Profile != ProfileID || !credential(refresh.RefreshToken) || len(refresh.Facts) != 1 || Principal(refresh.Facts["subject"]) == "" {
		return abi.Grant{}, invalid()
	}
	var tokens tokenResponse
	if err := a.form(ctx, "/oauth2/token", url.Values{"grant_type": {"refresh_token"}, "client_id": {ClientID}, "refresh_token": {refresh.RefreshToken}}, &tokens); err != nil {
		return abi.Grant{}, err
	}
	grant, err := a.grant(ctx, tokens)
	if err != nil {
		return abi.Grant{}, err
	}
	if grant.Facts["subject"] != refresh.Facts["subject"] {
		return abi.Grant{}, &abi.Error{Code: abi.CodeInvalidGrant, Message: "Refreshed authorization names a different subject. Enroll again."}
	}
	return grant, nil
}

func (a Adapter) grant(ctx context.Context, tokens tokenResponse) (abi.Grant, error) {
	if !credential(tokens.AccessToken) || tokens.RefreshToken != "" && !credential(tokens.RefreshToken) || !strings.EqualFold(tokens.TokenType, "Bearer") || tokens.ExpiresIn <= 0 || tokens.ExpiresIn > 365*24*60*60 {
		return abi.Grant{}, invalid()
	}
	response, err := a.Fetch(ctx, abi.HTTPRequest{Method: http.MethodGet, URL: Issuer + "/oauth2/userinfo", Header: map[string][]string{"Authorization": {"Bearer " + tokens.AccessToken}}})
	if err != nil || response.Status != http.StatusOK || len(response.Body) > 64<<10 {
		return abi.Grant{}, invalid()
	}
	document, parseErr := oif.ParseJSON(response.Body, oif.Limits{})
	if parseErr != nil || document.Root().Kind() != oif.Object {
		return abi.Grant{}, invalid()
	}
	value, _ := document.Root().Lookup("sub")
	subject, _ := value.Text()
	if Principal(subject) == "" {
		return abi.Grant{}, invalid()
	}
	return abi.Grant{AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, ExpiresIn: tokens.ExpiresIn, Principal: Principal(subject), Facts: map[string]string{"subject": subject}}, nil
}

func (a Adapter) form(ctx context.Context, path string, form url.Values, output any) error {
	response, err := a.Fetch(ctx, abi.HTTPRequest{Method: http.MethodPost, URL: Issuer + path, Header: map[string][]string{"Content-Type": {"application/x-www-form-urlencoded"}, "User-Agent": {"OLP"}}, Body: []byte(form.Encode())})
	if err != nil {
		return &abi.Error{Code: abi.CodeHTTPFailed, Message: "Authorization request did not complete."}
	}
	if len(response.Body) > 64<<10 {
		return invalid()
	}
	if response.Status != http.StatusOK {
		var refusal struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(response.Body, &refusal) != nil {
			return invalid()
		}
		switch refusal.Error {
		case abi.CodeAuthorizationPending, abi.CodeSlowDown, abi.CodeAccessDenied, abi.CodeExpiredToken, abi.CodeInvalidGrant:
			return &abi.Error{Code: refusal.Error, Message: "Authorization endpoint refused the request."}
		case "authorization_denied":
			return &abi.Error{Code: abi.CodeAccessDenied, Message: "Device authorization was denied."}
		default:
			return invalid()
		}
	}
	if json.Unmarshal(response.Body, output) != nil {
		return invalid()
	}
	return nil
}

func credential(value string) bool {
	return value != "" && len(value) <= 16<<10 && !strings.ContainsFunc(value, unicode.IsControl)
}

func invalid() error {
	return &abi.Error{Code: abi.CodeInvalidGrant, Message: "Grok authorization or verified subject is unsupported. Enroll again."}
}
