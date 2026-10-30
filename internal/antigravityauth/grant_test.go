package antigravityauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func TestEnrollmentBindsAuthenticatedSubjectAndProject(t *testing.T) {
	clientID := "fixture.apps.googleusercontent.com"
	bootstrap := Adapter{ClientID: func(context.Context) string { return clientID }}
	authorization, err := bootstrap.StartGrant(t.Context(), abi.GrantStart{Profile: ProfileID})
	if err != nil {
		t.Fatal(err)
	}
	login, err := url.Parse(authorization.URL)
	if err != nil || login.Scheme+"://"+login.Host != Issuer || login.Query().Get("client_id") != clientID || login.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("unsupported authorization request %v", err)
	}
	adapter := Adapter{
		ClientID: func(context.Context) string { return clientID },
		Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
			switch request.URL {
			case TokenEndpoint:
				form, err := url.ParseQuery(string(request.Body))
				if err != nil || form.Get("client_id") != clientID || form.Get("client_secret") != "operator-secret" || form.Get("code") != "approved-code" || form.Get("redirect_uri") != RedirectURI || form.Get("code_verifier") == "" {
					t.Fatalf("token exchange did not bind the authorization request")
				}
				digest := sha256.Sum256([]byte(form.Get("code_verifier")))
				if login.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(digest[:]) {
					t.Fatal("token exchange did not match the public PKCE challenge")
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
	callback := RedirectURI + "?" + url.Values{"code": {"approved-code"}, "state": {login.Query().Get("state")}}.Encode()
	input, _ := json.Marshal(map[string]string{"callback_url": callback, "client_secret": "operator-secret"})
	grant, err := adapter.ExchangeGrant(t.Context(), abi.GrantExchange{Profile: ProfileID, Session: authorization.Session, Input: string(input)})
	if err != nil || grant.AccessToken != "opaque-access" || grant.Principal != Principal("verified-subject", "verified-project") || grant.Facts["subject"] != "verified-subject" || grant.Facts["project_id"] != "verified-project" || grant.ExpiresIn <= 0 {
		t.Fatalf("enrollment did not retain verified identity, grant=%+v error=%v", grant, err)
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

func TestRefreshKeepsVerifiedIdentityAndOpaqueClientAuthorization(t *testing.T) {
	for _, changed := range []string{"", "subject", "project"} {
		t.Run(changed, func(t *testing.T) {
			clientID := "fixture.apps.googleusercontent.com"
			adapter := Adapter{ClientID: func(context.Context) string { return clientID }, Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
				switch request.URL {
				case TokenEndpoint:
					form, _ := url.ParseQuery(string(request.Body))
					if form.Get("client_secret") != "operator-secret" || form.Get("client_id") != clientID {
						t.Fatal("token exchange lost client authorization")
					}
					if form.Get("grant_type") == "authorization_code" {
						return response(`{"access_token":"enrolled-access","refresh_token":"opaque-refresh","token_type":"Bearer","expires_in":3600}`), nil
					}
					if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "opaque-refresh" {
						t.Fatal("refresh lost opaque upstream authorization")
					}
					return response(`{"access_token":"renewed-access","token_type":"Bearer","expires_in":3600}`), nil
				case UserInfoEndpoint:
					if changed == "subject" && request.Header["Authorization"][0] == "Bearer renewed-access" {
						return response(`{"sub":"another-subject"}`), nil
					}
					return response(`{"sub":"verified-subject"}`), nil
				case ProjectEndpoint:
					if changed == "project" && request.Header["Authorization"][0] == "Bearer renewed-access" {
						return response(`{"cloudaicompanionProject":"another-project"}`), nil
					}
					return response(`{"cloudaicompanionProject":"verified-project"}`), nil
				default:
					t.Fatal("refresh reached an unapproved destination")
					return abi.HTTPResponse{}, nil
				}
			}}
			authorization, err := adapter.StartGrant(t.Context(), abi.GrantStart{Profile: ProfileID})
			if err != nil {
				t.Fatal(err)
			}
			login, err := url.Parse(authorization.URL)
			if err != nil {
				t.Fatal(err)
			}
			input, _ := json.Marshal(map[string]string{"callback_url": RedirectURI + "?" + url.Values{"code": {"approved-code"}, "state": {login.Query().Get("state")}}.Encode(), "client_secret": "operator-secret"})
			enrolled, err := adapter.ExchangeGrant(t.Context(), abi.GrantExchange{Profile: ProfileID, Session: authorization.Session, Input: string(input)})
			if err != nil {
				t.Fatal(err)
			}
			refreshed, err := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: ProfileID, RefreshToken: enrolled.RefreshToken, Facts: enrolled.Facts})
			if changed != "" {
				if err == nil || refreshed.AccessToken != "" || strings.Contains(err.Error(), "operator-secret") {
					t.Fatalf("changed verified identity was retained, error=%v", err)
				}
				return
			}
			if err != nil || refreshed.Principal != enrolled.Principal || refreshed.AccessToken != "renewed-access" {
				t.Fatalf("stable refresh lost verified identity, error=%v", err)
			}
			continued, err := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: ProfileID, RefreshToken: refreshed.RefreshToken, Facts: refreshed.Facts})
			if err != nil || continued.Principal != enrolled.Principal || continued.AccessToken != "renewed-access" {
				t.Fatalf("nonrotating refresh lost authorization or identity, error=%v", err)
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

func TestCredentialImportVerifiesOriginalAccessBeforeRefresh(t *testing.T) {
	clientID := "fixture.apps.googleusercontent.com"
	for _, changed := range []string{"", "subject", "project"} {
		t.Run(changed, func(t *testing.T) {
			credentials, err := ParseCredentialImport(`{"type":"antigravity","access_token":"import-access","refresh_token":"import-refresh","client_id":"fixture.apps.googleusercontent.com","client_secret":"operator-secret","project_id":"verified-project"}`, clientID, time.Now())
			if err != nil {
				t.Fatal("supported migration credentials were rejected")
			}
			fetch := func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
				original := http.Header(request.Header).Get("Authorization") == "Bearer import-access"
				switch request.URL {
				case TokenEndpoint:
					form, _ := url.ParseQuery(string(request.Body))
					if form.Get("grant_type") != "refresh_token" || form.Get("client_id") != clientID || form.Get("client_secret") != "operator-secret" || form.Get("refresh_token") != "import-refresh" {
						t.Fatal("import did not retain original client authorization")
					}
					return response(`{"access_token":"refreshed-access","token_type":"Bearer","expires_in":3600}`), nil
				case UserInfoEndpoint:
					if !original && http.Header(request.Header).Get("Authorization") != "Bearer refreshed-access" {
						t.Fatal("identity verification lacked authenticated access")
					}
					if !original && changed == "subject" {
						return response(`{"sub":"changed-subject"}`), nil
					}
					return response(`{"sub":"verified-subject"}`), nil
				case ProjectEndpoint:
					if !original && changed == "project" {
						return response(`{"cloudaicompanionProject":"changed-project"}`), nil
					}
					return response(`{"cloudaicompanionProject":"verified-project"}`), nil
				default:
					t.Fatal("import reached an unapproved destination")
					return abi.HTTPResponse{}, nil
				}
			}
			client := &http.Client{Transport: importRoundTripper(func(request *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(request.Body)
				response, err := fetch(request.Context(), abi.HTTPRequest{Method: request.Method, URL: request.URL.String(), Header: request.Header, Body: body})
				return &http.Response{StatusCode: response.Status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(response.Body)))}, err
			})}
			refresh, err := credentials.Verify(t.Context(), client)
			if err != nil {
				t.Fatal("original access could not establish verified identity")
			}
			adapter := Adapter{ClientID: func(context.Context) string { return clientID }, Fetch: fetch}
			grant, err := adapter.RefreshGrant(t.Context(), refresh)
			if changed != "" {
				if err == nil || grant.AccessToken != "" || strings.Contains(err.Error(), "operator-secret") {
					t.Fatal("refresh substituted a different verified identity")
				}
				return
			}
			if err != nil || grant.Principal != Principal("verified-subject", "verified-project") || grant.AccessToken != "refreshed-access" {
				t.Fatal("import lost verified identity or fresh access")
			}
			continued, err := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: ProfileID, RefreshToken: grant.RefreshToken, Facts: grant.Facts})
			if err != nil || continued.Principal != grant.Principal {
				t.Fatal("import did not retain OLP refresh ownership without token rotation")
			}
		})
	}
}

type importRoundTripper func(*http.Request) (*http.Response, error)

func (f importRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestCredentialImportRejectsUnsupportedCredentialsAndUnverifiedIdentity(t *testing.T) {
	const supported = `{"type":"antigravity","access_token":"import-access","refresh_token":"import-refresh","client_id":"fixture.apps.googleusercontent.com","client_secret":"operator-secret"}`
	for name, document := range map[string]string{
		"missing original client authorization": strings.ReplaceAll(supported, `,"client_secret":"operator-secret"`, ""),
		"another OAuth client":                  strings.ReplaceAll(supported, "fixture.apps", "another.apps"),
		"duplicate token":                       strings.Replace(supported, `"access_token":"import-access"`, `"access_token":"import-access","access_token":"another-access"`, 1),
		"unsupported store":                     `{"credentials":{"access_token":"import-access"}}`,
		"expired constraint":                    strings.TrimSuffix(supported, "}") + `,"expired":"` + time.Now().Add(-time.Hour).Format(time.RFC3339) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCredentialImport(document, "fixture.apps.googleusercontent.com", time.Now()); err == nil || strings.Contains(err.Error(), "operator-secret") || strings.Contains(err.Error(), "import-access") {
				t.Fatal("unsupported credentials were accepted or exposed")
			}
		})
	}
	for _, evidence := range []string{"denied", "missing subject", "missing project", "conflicting project", "redirect"} {
		t.Run(evidence, func(t *testing.T) {
			document := strings.TrimSuffix(supported, "}") + `,"email":"unverified@example.invalid","project_id":"verified-project"}`
			credentials, err := ParseCredentialImport(document, "fixture.apps.googleusercontent.com", time.Now())
			if err != nil {
				t.Fatal("valid constraint envelope was rejected")
			}
			client := &http.Client{Transport: importRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.URL.String() != UserInfoEndpoint && request.URL.String() != ProjectEndpoint {
					t.Fatal("verification followed an untrusted redirect")
				}
				if request.Header.Get("Authorization") != "Bearer import-access" {
					t.Fatal("verification omitted original access")
				}
				status, body := func() (int, string) {
					if evidence == "denied" {
						return http.StatusUnauthorized, `{"sub":"untrusted"}`
					}
					if evidence == "redirect" {
						return http.StatusTemporaryRedirect, ""
					}
					if evidence == "missing subject" && request.URL.String() == UserInfoEndpoint || evidence == "missing project" && request.URL.String() == ProjectEndpoint {
						return http.StatusOK, `{}`
					}
					if request.URL.String() == UserInfoEndpoint {
						return http.StatusOK, `{"sub":"verified-subject"}`
					}
					return http.StatusOK, `{"cloudaicompanionProject":"another-project"}`
				}()
				return &http.Response{StatusCode: status, Header: http.Header{"Location": {"https://untrusted.example.invalid/credentials"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			if _, err := credentials.Verify(t.Context(), client); err == nil {
				t.Fatal("local claims replaced authenticated subject or project evidence")
			}
		})
	}
}
