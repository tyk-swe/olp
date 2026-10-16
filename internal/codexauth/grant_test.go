package codexauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func testToken(account, user string, expiry time.Time) string {
	payload := fmt.Sprintf(`{"exp":%d,"https://api.openai.com/auth":{"chatgpt_account_id":%q,"chatgpt_user_id":%q}}`, expiry.Unix(), account, user)
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".fixture-signature"
}

func TestDeviceEnrollmentOnlyCallsOfficialAuthenticationEndpoints(t *testing.T) {
	token := testToken("account-a", "user-a", time.Now().Add(time.Hour))
	step := 0
	a := Adapter{Fetch: func(_ context.Context, r abi.HTTPRequest) (abi.HTTPResponse, error) {
		step++
		if r.Method != "POST" || len(r.Header) != 1 {
			t.Fatalf("unexpected auth request method or headers")
		}
		switch step {
		case 1:
			if r.URL != "https://auth.openai.com/api/accounts/deviceauth/usercode" || string(r.Body) != `{"client_id":"app_EMoamEEZ73f0CkXaXp7hrann"}` {
				t.Fatal("device start differs from official client contract")
			}
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"device_auth_id":"device-1","user_code":"ABCD-EFGH","interval":" 5 "}`)}, nil
		case 2, 3:
			if r.URL != "https://auth.openai.com/api/accounts/deviceauth/token" || string(r.Body) != `{"device_auth_id":"device-1","user_code":"ABCD-EFGH"}` {
				t.Fatal("device poll differs from official client contract")
			}
			if step == 2 {
				return abi.HTTPResponse{Status: 403}, nil
			}
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"authorization_code":"issued-code","code_verifier":"issued-verifier","code_challenge":"issued-challenge"}`)}, nil
		case 4:
			form, err := url.ParseQuery(string(r.Body))
			if err != nil || r.URL != "https://auth.openai.com/oauth/token" || r.Header["Content-Type"][0] != "application/x-www-form-urlencoded" || len(form) != 5 ||
				form.Get("grant_type") != "authorization_code" || form.Get("code") != "issued-code" || form.Get("code_verifier") != "issued-verifier" || form.Get("client_id") != "app_EMoamEEZ73f0CkXaXp7hrann" || form.Get("redirect_uri") != "https://auth.openai.com/deviceauth/callback" {
				t.Fatal("device exchange differs from official client contract")
			}
			body, _ := json.Marshal(map[string]string{"access_token": token, "id_token": token, "refresh_token": "refresh-1"})
			return abi.HTTPResponse{Status: 200, Body: body}, nil
		default:
			t.Fatal("unexpected extra upstream dispatch")
			return abi.HTTPResponse{}, nil
		}
	}}
	start, err := a.StartGrant(t.Context(), abi.GrantStart{Profile: ProfileID})
	if err != nil || start.URL != "" || start.Device.VerificationURL != "https://auth.openai.com/codex/device" || start.Device.ExpiresIn != 900 || start.Device.Interval != 5 {
		t.Fatalf("device authorization: %v", err)
	}
	poll := abi.GrantPoll{Profile: ProfileID, Session: start.Session}
	if _, err := a.PollGrant(t.Context(), poll); errorCode(err) != abi.CodeAuthorizationPending {
		t.Fatalf("pending poll: %v", err)
	}
	grant, err := a.PollGrant(t.Context(), poll)
	if err != nil || grant.AccessToken != token || grant.RefreshToken != "refresh-1" || grant.Facts["account_id"] != "account-a" || grant.Facts["user_id"] != "user-a" || grant.ExpiresIn < 3590 || grant.ExpiresIn > 3600 {
		t.Fatalf("enrollment did not retain the issued identity and token: %v", err)
	}
	if _, err := a.Sign(t.Context(), abi.SignRequest{}); errorCode(err) != "code_mode_required" || step != 4 {
		t.Fatal("ordinary inference must refuse without any upstream request")
	}
	if _, err := connectors.NewPluginProfile("digest", Manifest(), ProfileID); err != nil {
		t.Fatalf("invalid declared provider profile: %v", err)
	}
}

func TestAuthenticationFailuresAreMetadataOnlyAndPermanentWhenRequired(t *testing.T) {
	for _, body := range []string{`{"error":"invalid_grant","error_description":"secret"}`, `{"error":{"code":"refresh_token_expired","message":"secret"}}`, `{"error":{"code":"refresh_token_reused"}}`, `{"error":{"code":"refresh_token_invalidated"}}`} {
		a := Adapter{Fetch: func(context.Context, abi.HTTPRequest) (abi.HTTPResponse, error) {
			return abi.HTTPResponse{Status: 400, Body: []byte(body)}, nil
		}}
		_, err := a.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: ProfileID, RefreshToken: "secret"})
		if errorCode(err) != abi.CodeInvalidGrant || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe refresh failure: %v", err)
		}
	}
	a := Adapter{Fetch: func(context.Context, abi.HTTPRequest) (abi.HTTPResponse, error) {
		return abi.HTTPResponse{}, errors.New("private transport detail")
	}}
	if _, err := a.StartGrant(t.Context(), abi.GrantStart{Profile: ProfileID}); strings.Contains(err.Error(), "private") {
		t.Fatal("transport detail exposed")
	}
}

func errorCode(err error) string {
	if err == nil {
		return ""
	}
	if failure, ok := errors.AsType[*abi.Error](err); ok {
		return failure.Code
	}
	return err.Error()
}
