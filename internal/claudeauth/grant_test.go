package claudeauth

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

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
	grant, exchangeErr := adapter.ExchangeGrant(t.Context(), abi.GrantExchange{Profile: ProfileID, Session: start.Session, Input: "authorized#" + state})
	if exchangeErr != nil {
		t.Fatal(exchangeErr)
	}
	if grant.Principal == "" || grant.ExpiresIn != 3600 || grant.RefreshToken != "fixture-refresh" || grant.Facts["account_uuid"] != "11111111-1111-4111-8111-111111111111" {
		t.Fatal("verified grant was not returned")
	}
	_, wrongStateErr := adapter.ExchangeGrant(t.Context(), abi.GrantExchange{Profile: ProfileID, Session: start.Session, Input: "authorized#another-state"})
	if reported, ok := errors.AsType[*abi.Error](wrongStateErr); !ok || reported.Code != abi.CodeStateMismatch {
		t.Fatalf("wrong state accepted: %v", wrongStateErr)
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
