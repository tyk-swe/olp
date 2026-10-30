package claudeauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
	"golang.org/x/oauth2"
)

func TestHeadlessEnrollmentVerifiesProfileBeforeIssuingGrant(t *testing.T) {
	start, startErr := (Adapter{}).StartGrant(t.Context(), abi.GrantStart{Profile: ProfileID})
	if startErr != nil {
		t.Fatal(startErr)
	}
	address, parseErr := url.Parse(start.URL)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	state := address.Query().Get("state")
	challenge := address.Query().Get("code_challenge")
	if state == "" || challenge == "" || address.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("authorization did not advertise state and an S256 challenge")
	}

	adapter := Adapter{Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
		switch request.URL {
		case TokenURL:
			var exchange struct {
				Code, State string
				Verifier    string `json:"code_verifier"`
				ClientID    string `json:"client_id"`
				RedirectURI string `json:"redirect_uri"`
			}
			if request.Method != "POST" || json.Unmarshal(request.Body, &exchange) != nil || exchange.Code != "authorized" || exchange.State != state || oauth2.S256ChallengeFromVerifier(exchange.Verifier) != challenge || exchange.ClientID != ClientID || exchange.RedirectURI != RedirectURI {
				t.Fatal("invalid authorization exchange")
			}
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"access_token":"fixture-access","refresh_token":"fixture-refresh","token_type":"Bearer","expires_in":3600}`)}, nil
		case ProfileURL:
			if request.Method != "GET" || request.Header["Authorization"][0] != "Bearer fixture-access" {
				t.Fatal("profile was not authenticated with the issued token")
			}
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"account":{"uuid":"11111111-1111-4111-8111-111111111111"},"organization":{"uuid":"22222222-2222-4222-8222-222222222222"}}`)}, nil
		default:
			t.Fatal("enrollment attempted an unsupported request")
			return abi.HTTPResponse{}, nil
		}
	}}
	for _, input := range []string{"authorized#" + state, RedirectURI + "?code=authorized&state=" + url.QueryEscape(state)} {
		grant, exchangeErr := adapter.ExchangeGrant(t.Context(), abi.GrantExchange{Profile: ProfileID, Session: start.Session, Input: input})
		if exchangeErr != nil {
			t.Fatal(exchangeErr)
		}
		if grant.Principal == "" || grant.ExpiresIn != 3600 || grant.RefreshToken != "fixture-refresh" || grant.Facts["account_uuid"] != "11111111-1111-4111-8111-111111111111" {
			t.Fatal("verified grant was not returned")
		}
	}
	_, wrongStateErr := adapter.ExchangeGrant(t.Context(), abi.GrantExchange{Profile: ProfileID, Session: start.Session, Input: "authorized#another-state"})
	if reported, ok := errors.AsType[*abi.Error](wrongStateErr); !ok || reported.Code != abi.CodeStateMismatch {
		t.Fatalf("wrong state accepted: %v", wrongStateErr)
	}
}

func TestRefreshOnlyIssuesGrantsWithBoundedPositiveLifetimes(t *testing.T) {
	facts := map[string]string{"account_uuid": "11111111-1111-4111-8111-111111111111", "organization_uuid": "22222222-2222-4222-8222-222222222222"}
	const maximumLifetime = int64(366 * 24 * 60 * 60)
	for _, test := range []struct {
		name    string
		seconds int64
		valid   bool
	}{
		{"minimum", 1, true},
		{"maximum", maximumLifetime, true},
		{"zero", 0, false},
		{"negative", -1, false},
		{"above maximum", maximumLifetime + 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tokens, err := json.Marshal(struct {
				AccessToken string `json:"access_token"`
				TokenType   string `json:"token_type"`
				ExpiresIn   int64  `json:"expires_in"`
			}{"fixture-access", "bearer", test.seconds})
			if err != nil {
				t.Fatal(err)
			}
			adapter := Adapter{Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
				switch request.URL {
				case TokenURL:
					return abi.HTTPResponse{Status: 200, Body: tokens}, nil
				case ProfileURL:
					return abi.HTTPResponse{Status: 200, Body: []byte(`{"account":{"uuid":"11111111-1111-4111-8111-111111111111"},"organization":{"uuid":"22222222-2222-4222-8222-222222222222"}}`)}, nil
				default:
					t.Fatal("refresh attempted an unsupported request")
					return abi.HTTPResponse{}, nil
				}
			}}
			grant, refreshErr := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: ProfileID, RefreshToken: "fixture-refresh", Facts: facts})
			if !test.valid {
				if refreshErr == nil || grant.AccessToken != "" {
					t.Fatalf("unsupported lifetime issued a grant %+v, error %v", grant, refreshErr)
				}
				return
			}
			if refreshErr != nil || grant.ExpiresIn != test.seconds || grant.Principal != Principal(facts) {
				t.Fatalf("valid lifetime lost during refresh %+v, error %v", grant, refreshErr)
			}
		})
	}
}

func TestRefreshRequiresVerifiedStableIdentity(t *testing.T) {
	facts := map[string]string{"account_uuid": "11111111-1111-4111-8111-111111111111", "organization_uuid": "22222222-2222-4222-8222-222222222222"}
	for _, scenario := range []struct {
		name, profile string
		status        int
		want          bool
	}{
		{"same identity", `{"account":{"uuid":"11111111-1111-4111-8111-111111111111"},"organization":{"uuid":"22222222-2222-4222-8222-222222222222"}}`, 200, true},
		{"changed account", `{"account":{"uuid":"33333333-3333-4333-8333-333333333333"},"organization":{"uuid":"22222222-2222-4222-8222-222222222222"}}`, 200, false},
		{"changed organization", `{"account":{"uuid":"11111111-1111-4111-8111-111111111111"},"organization":{"uuid":"33333333-3333-4333-8333-333333333333"}}`, 200, false},
		{"missing identity", `{}`, 200, false},
		{"unavailable identity", `{"error":"fixture-secret"}`, 503, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			adapter := Adapter{Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
				if request.URL == ProfileURL {
					return abi.HTTPResponse{Status: scenario.status, Body: []byte(scenario.profile)}, nil
				}
				if request.URL != TokenURL {
					t.Fatal("refresh attempted inference")
				}
				return abi.HTTPResponse{Status: 200, Body: []byte(`{"access_token":"fixture-rotated","token_type":"bearer","expires_in":7200}`)}, nil
			}}
			grant, err := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: ProfileID, RefreshToken: "fixture-refresh", Facts: facts})
			if scenario.want {
				if err != nil || grant.Principal != Principal(facts) || grant.AccessToken != "fixture-rotated" || grant.RefreshToken != "" {
					t.Fatalf("identity continuity was lost: %v", err)
				}
				return
			}
			if err == nil || grant.AccessToken != "" {
				t.Fatal("unverified or changed identity received a grant")
			}
		})
	}
}

func TestEnrollmentRefusesUnverifiedResponsesWithoutExposingSecrets(t *testing.T) {
	for _, scenario := range []struct {
		name, body string
		status     int
		transport  bool
	}{
		{"revoked refresh", `{"error":"invalid_grant","error_description":"fixture-secret"}`, 400, false},
		{"ambiguous provider refusal", `{"error":"fixture-secret"}`, 503, false},
		{"duplicate access token", `{"access_token":"fixture-secret","access_token":"another","refresh_token":"refresh","token_type":"bearer","expires_in":3600}`, 200, false},
		{"transport failure", "", 0, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			adapter := Adapter{Fetch: func(_ context.Context, _ abi.HTTPRequest) (abi.HTTPResponse, error) {
				if scenario.transport {
					return abi.HTTPResponse{}, errors.New("fixture-secret")
				}
				return abi.HTTPResponse{Status: scenario.status, Body: []byte(scenario.body)}, nil
			}}
			grant, err := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: ProfileID, RefreshToken: "fixture-secret", Facts: map[string]string{}})
			if err == nil || grant.AccessToken != "" || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("unverified token or secret escaped a refusal")
			}
			if scenario.name == "revoked refresh" {
				reported, ok := errors.AsType[*abi.Error](err)
				if !ok || reported.Code != abi.CodeInvalidGrant {
					t.Fatal("revocation did not permanently refuse refresh")
				}
			}
		})
	}
}

func TestCredentialImportVerifiesOpaqueTokensAndOwnsStableRefresh(t *testing.T) {
	credentials, err := ParseCredentialImport(`{"claudeAiOauth":{"accessToken":"fixture-local-access","refreshToken":"fixture-local-refresh","expiresAt":1900000000000,"scopes":["user:profile","user:inference"],"subscriptionType":"fixture-plan","rateLimitTier":null}}`, time.UnixMilli(1800000000000))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: importTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != "GET" || request.URL.String() != ProfileURL || request.Header.Get("Authorization") != "Bearer fixture-local-access" {
			t.Fatal("import did not verify the pasted access token at the fixed profile endpoint")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"account":{"uuid":"11111111-1111-4111-8111-111111111111"},"organization":{"uuid":"22222222-2222-4222-8222-222222222222"}}`))}, nil
	})}
	refresh, err := credentials.Verify(t.Context(), client)
	if err != nil {
		t.Fatal(err)
	}
	adapter := Adapter{Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
		if request.URL == ProfileURL {
			if request.Header["Authorization"][0] != "Bearer fixture-imported-access" {
				t.Fatal("refresh did not authenticate the issued access token")
			}
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"account":{"uuid":"11111111-1111-4111-8111-111111111111"},"organization":{"uuid":"22222222-2222-4222-8222-222222222222"}}`)}, nil
		}
		var fields map[string]string
		if request.URL != TokenURL || request.Method != "POST" || json.Unmarshal(request.Body, &fields) != nil {
			t.Fatal("import refresh attempted inference or left the fixed issuer")
		}
		if fields["grant_type"] != "refresh_token" || fields["refresh_token"] != "fixture-local-refresh" || fields["client_id"] != ClientID {
			t.Fatal("import lost supported refresh ownership")
		}
		return abi.HTTPResponse{Status: 200, Body: []byte(`{"access_token":"fixture-imported-access","token_type":"bearer","expires_in":3600}`)}, nil
	}}
	grant, err := adapter.RefreshGrant(t.Context(), refresh)
	if err != nil || grant.Principal != Principal(refresh.Facts) || grant.AccessToken != "fixture-imported-access" || grant.RefreshToken != "" {
		t.Fatal("import refresh lost verified identity or required refresh token rotation")
	}
	if !slices.Contains(credentials.Secrets(), "fixture-local-refresh") || !slices.Contains(credentials.Secrets(), "fixture-local-access") {
		t.Fatal("imported tokens were not marked secret")
	}
}

type importTransport func(*http.Request) (*http.Response, error)

func (transport importTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func TestCredentialImportRejectsUnsupportedDocumentsWithoutDisclosingTokens(t *testing.T) {
	const supported = `{"claudeAiOauth":{"accessToken":"fixture-import-access","refreshToken":"fixture-import-refresh","expiresAt":1900000000000,"scopes":["user:profile","user:inference"],"subscriptionType":null,"rateLimitTier":null}}`
	for name, input := range map[string]string{
		"empty":             "",
		"other store":       `{"accessToken":"fixture-import-access","refreshToken":"fixture-import-refresh"}`,
		"additional store":  strings.Replace(supported, `{"claudeAiOauth":`, `{"otherCredential":"fixture-import-refresh","claudeAiOauth":`, 1),
		"ambiguous access":  strings.Replace(supported, `"accessToken":`, `"accessToken":"injected","accessToken":`, 1),
		"unknown field":     strings.Replace(supported, `"scopes":`, `"issuer":"https://untrusted.invalid","scopes":`, 1),
		"unsafe access":     strings.Replace(supported, "fixture-import-access", `fixture\nimport-access`, 1),
		"unsafe refresh":    strings.Replace(supported, "fixture-import-refresh", "fixture import-refresh", 1),
		"missing refresh":   strings.Replace(supported, "fixture-import-refresh", "", 1),
		"wrong expiry type": strings.Replace(supported, "1900000000000", `"1900000000000"`, 1),
		"missing expiry":    strings.Replace(supported, `"expiresAt":1900000000000,`, "", 1),
		"missing profile":   strings.Replace(supported, `"user:profile",`, "", 1),
		"missing inference": strings.Replace(supported, `,"user:inference"`, "", 1),
		"wrong client":      strings.Replace(supported, `"scopes":`, `"clientId":"unsupported-client","scopes":`, 1),
		"wrong metadata":    strings.Replace(supported, `"subscriptionType":null`, `"subscriptionType":false`, 1),
		"null scope":        strings.Replace(supported, `"scopes":[`, `"scopes":[null,`, 1),
		"null client":       strings.Replace(supported, `"scopes":`, `"clientId":null,"scopes":`, 1),
		"empty source":      strings.Replace(supported, `"scopes":`, `"subscriptionTypeSource":"","scopes":`, 1),
		"null timestamp":    strings.Replace(supported, `"scopes":`, `"subscriptionTypeVerifiedAt":null,"scopes":`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseCredentialImport(input, time.UnixMilli(1800000000000))
			if err == nil || strings.Contains(err.Error(), "fixture-import") {
				t.Fatal("unsupported credentials were accepted or disclosed")
			}
		})
	}
	withMetadata := strings.Replace(supported, `"scopes":`, `"clientId":"`+ClientID+`","refreshTokenExpiresAt":1900001000000,"subscriptionTypeSource":"profile","subscriptionTypeVerifiedAt":1800000000000,"scopes":`, 1)
	if _, err := ParseCredentialImport(withMetadata, time.UnixMilli(1800000000000)); err != nil {
		t.Fatal("supported current metadata was refused")
	}
}

func TestCredentialImportRefusesUnverifiedOriginalIdentity(t *testing.T) {
	credentials, err := ParseCredentialImport(`{"claudeAiOauth":{"accessToken":"fixture-import-access","refreshToken":"fixture-import-refresh","expiresAt":1700000000000,"scopes":["user:profile","user:inference"]}}`, time.UnixMilli(1800000000000))
	if err != nil {
		t.Fatal(err)
	}
	for name, response := range map[string]struct {
		status int
		body   string
	}{
		"expired access":     {401, `{"message":"fixture-import-access"}`},
		"redirected issuer":  {302, ""},
		"missing identity":   {200, `{"account":{},"organization":{}}`},
		"invalid identity":   {200, `{"account":{"uuid":"fixture-account"},"organization":{"uuid":"fixture-organization"}}`},
		"duplicate identity": {200, `{"account":{"uuid":"11111111-1111-4111-8111-111111111111"},"account":{"uuid":"33333333-3333-4333-8333-333333333333"},"organization":{"uuid":"22222222-2222-4222-8222-222222222222"}}`},
		"oversized identity": {200, strings.Repeat("x", (64<<10)+1)},
	} {
		t.Run(name, func(t *testing.T) {
			client := &http.Client{Transport: importTransport(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() != ProfileURL {
					t.Fatal("original credentials left the trusted profile endpoint")
				}
				return &http.Response{StatusCode: response.status, Header: http.Header{"Location": {"https://untrusted.invalid/credential"}}, Body: io.NopCloser(strings.NewReader(response.body))}, nil
			})}
			if _, err := credentials.Verify(t.Context(), client); err == nil || strings.Contains(err.Error(), "fixture-import") {
				t.Fatal("unverified original identity was accepted or disclosed")
			}
		})
	}
}
