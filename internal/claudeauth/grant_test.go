package claudeauth_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/claudeauth"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
	"golang.org/x/oauth2"
)

func TestHeadlessEnrollmentVerifiesProfileBeforeIssuingGrant(t *testing.T) {
	adapter := claudeauth.Adapter{Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
		switch request.URL {
		case claudeauth.TokenURL:
			var exchange struct {
				Code, State string
				Verifier    string `json:"code_verifier"`
				ClientID    string `json:"client_id"`
				RedirectURI string `json:"redirect_uri"`
			}
			if request.Method != "POST" || json.Unmarshal(request.Body, &exchange) != nil || exchange.Code != "authorized" || exchange.State == "" || exchange.Verifier == "" || exchange.ClientID != claudeauth.ClientID || exchange.RedirectURI != claudeauth.RedirectURI {
				t.Fatal("invalid authorization exchange")
			}
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"access_token":"fixture-access","refresh_token":"fixture-refresh","token_type":"Bearer","expires_in":3600}`)}, nil
		case claudeauth.ProfileURL:
			if request.Method != "GET" || request.Header["Authorization"][0] != "Bearer fixture-access" {
				t.Fatal("profile was not authenticated with the issued token")
			}
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"account":{"uuid":"11111111-1111-4111-8111-111111111111"},"organization":{"uuid":"22222222-2222-4222-8222-222222222222"}}`)}, nil
		default:
			t.Fatal("enrollment attempted an unsupported request")
			return abi.HTTPResponse{}, nil
		}
	}}
	start, err := adapter.StartGrant(t.Context(), abi.GrantStart{Profile: claudeauth.ProfileID})
	if err != nil {
		t.Fatal(err)
	}
	address, err := url.Parse(start.URL)
	if err != nil {
		t.Fatal(err)
	}
	var session struct{ State, Verifier string }
	if json.Unmarshal([]byte(start.Session), &session) != nil || address.Query().Get("state") != session.State || address.Query().Get("code_challenge") != oauth2.S256ChallengeFromVerifier(session.Verifier) {
		t.Fatal("authorization is not bound to its PKCE session")
	}
	grant, err := adapter.ExchangeGrant(t.Context(), abi.GrantExchange{Profile: claudeauth.ProfileID, Session: start.Session, Input: "authorized#" + session.State})
	if err != nil {
		t.Fatal(err)
	}
	if grant.Principal == "" || grant.ExpiresIn != 3600 || grant.RefreshToken != "fixture-refresh" || grant.Facts["account_uuid"] != "11111111-1111-4111-8111-111111111111" {
		t.Fatal("verified grant was not returned")
	}
	_, err = adapter.ExchangeGrant(t.Context(), abi.GrantExchange{Profile: claudeauth.ProfileID, Session: start.Session, Input: "authorized#another-state"})
	if reported, ok := errors.AsType[*abi.Error](err); !ok || reported.Code != abi.CodeStateMismatch {
		t.Fatalf("wrong state accepted: %v", err)
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
			adapter := claudeauth.Adapter{Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
				if request.URL == claudeauth.ProfileURL {
					return abi.HTTPResponse{Status: scenario.status, Body: []byte(scenario.profile)}, nil
				}
				if request.URL != claudeauth.TokenURL {
					t.Fatal("refresh attempted inference")
				}
				return abi.HTTPResponse{Status: 200, Body: []byte(`{"access_token":"fixture-rotated","token_type":"bearer","expires_in":7200}`)}, nil
			}}
			grant, err := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: claudeauth.ProfileID, RefreshToken: "fixture-refresh", Facts: facts})
			if scenario.want {
				if err != nil || grant.Principal != claudeauth.Principal(facts) || grant.AccessToken != "fixture-rotated" || grant.RefreshToken != "" {
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
			adapter := claudeauth.Adapter{Fetch: func(_ context.Context, _ abi.HTTPRequest) (abi.HTTPResponse, error) {
				if scenario.transport {
					return abi.HTTPResponse{}, errors.New("fixture-secret")
				}
				return abi.HTTPResponse{Status: scenario.status, Body: []byte(scenario.body)}, nil
			}}
			grant, err := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: claudeauth.ProfileID, RefreshToken: "fixture-secret", Facts: map[string]string{}})
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
