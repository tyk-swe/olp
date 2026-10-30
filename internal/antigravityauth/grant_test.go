package antigravityauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func TestEnrollmentBindsAuthenticatedSubjectAndProject(t *testing.T) {
	clientID := "fixture.apps.googleusercontent.com"
	adapter := Adapter{
		ClientID: func(context.Context) string { return clientID },
		Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
			switch request.URL {
			case TokenEndpoint:
				form, err := url.ParseQuery(string(request.Body))
				if err != nil || form.Get("client_id") != clientID || form.Get("client_secret") != "operator-secret" || form.Get("code") != "approved-code" || form.Get("redirect_uri") != RedirectURI || form.Get("code_verifier") == "" {
					t.Fatalf("token exchange did not bind the authorization request")
				}
				return response(`{"access_token":"opaque-access","refresh_token":"opaque-refresh","token_type":"Bearer","expires_in":3600}`), nil
			case UserInfoEndpoint:
				if request.Header["Authorization"][0] != "Bearer opaque-access" {
					t.Fatal("profile request omitted the issued access token")
				}
				return response(`{"sub":"verified-subject","email":"display@example.invalid","email_verified":true}`), nil
			case ProjectEndpoint:
				return response(`{"cloudaicompanionProject":{"id":"verified-project"}}`), nil
			default:
				t.Fatalf("unexpected authorization destination %q", request.URL)
				return abi.HTTPResponse{}, nil
			}
		},
	}
	authorization, err := adapter.StartGrant(t.Context(), abi.GrantStart{Profile: ProfileID})
	if err != nil {
		t.Fatal(err)
	}
	login, err := url.Parse(authorization.URL)
	if err != nil || login.Scheme+"://"+login.Host != Issuer || login.Query().Get("client_id") != clientID || login.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("unsupported authorization request %v", err)
	}
	var session struct {
		Verifier string `json:"verifier"`
	}
	if json.Unmarshal([]byte(authorization.Session), &session) != nil {
		t.Fatal("invalid encrypted session payload")
	}
	digest := sha256.Sum256([]byte(session.Verifier))
	if login.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(digest[:]) {
		t.Fatal("authorization did not bind its PKCE verifier")
	}
	callback := RedirectURI + "?" + url.Values{"code": {"approved-code"}, "state": {login.Query().Get("state")}}.Encode()
	input, _ := json.Marshal(map[string]string{"callback_url": callback, "client_secret": "operator-secret"})
	grant, err := adapter.ExchangeGrant(t.Context(), abi.GrantExchange{Profile: ProfileID, Session: authorization.Session, Input: string(input)})
	if err != nil || grant.AccessToken != "opaque-access" || grant.Principal != Principal("verified-subject", "verified-project") || grant.Facts["subject"] != "verified-subject" || grant.Facts["project_id"] != "verified-project" || grant.ExpiresIn <= 0 {
		t.Fatalf("enrollment did not retain verified identity, grant=%+v error=%v", grant, err)
	}
	var refresh struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Token        string `json:"refresh_token"`
	}
	if json.Unmarshal([]byte(grant.RefreshToken), &refresh) != nil || refresh.ClientID != clientID || refresh.ClientSecret != "operator-secret" || refresh.Token != "opaque-refresh" {
		t.Fatal("encrypted refresh payload omitted required client authorization")
	}
}

func response(body string) abi.HTTPResponse {
	return abi.HTTPResponse{Status: http.StatusOK, Body: []byte(body)}
}

func TestSigningCannotSendSubscriptionCredentialsToOrdinaryInference(t *testing.T) {
	_, err := (Adapter{}).Sign(t.Context(), abi.SignRequest{})
	if err == nil || !strings.Contains(err.Error(), "code-mode") {
		t.Fatalf("ordinary inference signing was allowed, error=%v", err)
	}
}

func TestRefreshKeepsVerifiedIdentityAndEncryptedClientAuthorization(t *testing.T) {
	for _, changed := range []string{"", "subject", "project"} {
		t.Run(changed, func(t *testing.T) {
			clientID := "fixture.apps.googleusercontent.com"
			adapter := Adapter{ClientID: func(context.Context) string { return clientID }, Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
				switch request.URL {
				case TokenEndpoint:
					form, _ := url.ParseQuery(string(request.Body))
					if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "stored-refresh" || form.Get("client_secret") != "operator-secret" || form.Get("client_id") != clientID {
						t.Fatal("refresh did not use encrypted authorization")
					}
					return response(`{"access_token":"renewed-access","token_type":"Bearer","expires_in":3600}`), nil
				case UserInfoEndpoint:
					if changed == "subject" {
						return response(`{"sub":"another-subject"}`), nil
					}
					return response(`{"sub":"verified-subject"}`), nil
				case ProjectEndpoint:
					if changed == "project" {
						return response(`{"cloudaicompanionProject":"another-project"}`), nil
					}
					return response(`{"cloudaicompanionProject":"verified-project"}`), nil
				default:
					t.Fatal("refresh reached an unapproved destination")
					return abi.HTTPResponse{}, nil
				}
			}}
			grant, err := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: ProfileID, RefreshToken: `{"client_id":"fixture.apps.googleusercontent.com","client_secret":"operator-secret","refresh_token":"stored-refresh"}`, Facts: map[string]string{"subject": "verified-subject", "project_id": "verified-project"}})
			if changed != "" {
				if err == nil || grant.AccessToken != "" || strings.Contains(err.Error(), "operator-secret") {
					t.Fatalf("changed verified identity was retained, error=%v", err)
				}
				return
			}
			if err != nil || grant.Principal != Principal("verified-subject", "verified-project") || grant.AccessToken != "renewed-access" {
				t.Fatalf("stable refresh lost verified identity, error=%v", err)
			}
			var refresh struct {
				Token string `json:"refresh_token"`
			}
			if json.Unmarshal([]byte(grant.RefreshToken), &refresh) != nil || refresh.Token != "stored-refresh" {
				t.Fatal("nonrotating upstream refresh lost its encrypted authorization")
			}
		})
	}
}

func TestEnrollmentRefusesCrossSessionCallbacksBeforeExchangingCredentials(t *testing.T) {
	adapter := Adapter{ClientID: func(context.Context) string { return "fixture.apps.googleusercontent.com" }, Fetch: func(context.Context, abi.HTTPRequest) (abi.HTTPResponse, error) {
		t.Fatal("unbound callback reached the authorization server")
		return abi.HTTPResponse{}, nil
	}}
	authorization, err := adapter.StartGrant(t.Context(), abi.GrantStart{Profile: ProfileID})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(map[string]string{"callback_url": RedirectURI + "?code=unbound&state=another-session", "client_secret": "operator-secret"})
	grant, err := adapter.ExchangeGrant(t.Context(), abi.GrantExchange{Profile: ProfileID, Session: authorization.Session, Input: string(input)})
	if err == nil || grant.AccessToken != "" {
		t.Fatal("unbound callback enrolled")
	}
}
