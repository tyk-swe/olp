package identity

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func jsonPost(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func TestParseFederatedBeginInputEnforcesBoundsAndPurpose(t *testing.T) {
	tooLong := "/" + strings.Repeat("a", maxFederatedReturnLen)
	if _, err := parseFederatedBeginInput(jsonPost("/api/v1/oidc/login", `{"return_to":"`+tooLong+`"}`), "login", true); err == nil {
		t.Fatal("accepted oversized return_to")
	}
	if _, err := parseFederatedBeginInput(jsonPost("/api/v1/oidc/login", `{"purpose":"mfa"}`), "login", true); err == nil {
		t.Fatal("accepted unexpected purpose on login flow")
	}
	got, err := parseFederatedBeginInput(jsonPost("/api/v1/saml/link", `{}`), "link", false)
	if err != nil || got.ReturnTo != "/settings/profile" {
		t.Fatalf("link input = %+v, err = %v", got, err)
	}
}

func TestDecodeOIDCVerifiedClaimsRejectsMalformedTypes(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	cfg := &oidcConfiguration{ClientID: "cid", EmailClaim: "email", GroupsClaim: "groups"}
	valid := map[string]json.RawMessage{
		"email":          json.RawMessage(`"ada@example.com"`),
		"email_verified": json.RawMessage(`true`),
		"auth_time":      json.RawMessage(`1699999990`),
	}
	if _, err := decodeOIDCVerifiedClaims(valid, "sub-1", "reauthenticate", cfg, now); err != nil {
		t.Fatalf("valid claims rejected: %v", err)
	}
	for name, claims := range map[string]map[string]json.RawMessage{
		"non-string email": {
			"email":          json.RawMessage(`123`),
			"email_verified": json.RawMessage(`true`),
		},
		"non-bool email_verified": {
			"email":          json.RawMessage(`"ada@example.com"`),
			"email_verified": json.RawMessage(`"true"`),
		},
		"non-int auth_time": {
			"email":          json.RawMessage(`"ada@example.com"`),
			"email_verified": json.RawMessage(`true`),
			"auth_time":      json.RawMessage(`"1699999990"`),
		},
	} {
		kind := "login"
		if name == "non-int auth_time" {
			kind = "reauthenticate"
		}
		if _, err := decodeOIDCVerifiedClaims(claims, "sub-1", kind, cfg, now); err == nil {
			t.Errorf("%s: accepted malformed claim type", name)
		}
	}
}
