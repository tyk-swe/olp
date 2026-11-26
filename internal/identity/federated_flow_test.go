package identity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"golang.org/x/oauth2"
)

func jsonPost(target, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

func strPtr(v string) *string { return &v }
func boolPtr(v bool) *bool    { return &v }

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
	cfg := oidcConfiguration{ClientID: "cid", EmailClaim: "email", GroupsClaim: "groups"}
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

func TestNormalizeOIDCConfigInput(t *testing.T) {
	got, err := normalizeOIDCConfigInput(oidcConfigInput{
		DiscoveryURL: "https://idp.example/.well-known/openid-configuration",
		Issuer:       "https://idp.example",
		ClientID:     "client-1",
	})
	if err != nil {
		t.Fatalf("minimal valid input rejected: %v", err)
	}
	if !got.Enabled || got.EmailClaim != "email" || got.GroupsClaim != "groups" {
		t.Fatalf("unexpected defaults: %+v", got)
	}
	if !slices.Equal(got.Scopes, []string{"openid", "email", "profile"}) {
		t.Fatalf("scopes = %v, want [openid email profile]", got.Scopes)
	}
	if got.EmailMappings == nil || got.GroupMappings == nil {
		t.Fatalf("expected non-nil empty mapping slices, got email=%v group=%v", got.EmailMappings, got.GroupMappings)
	}

	disabled, err := normalizeOIDCConfigInput(oidcConfigInput{
		ClientID:     "client-1",
		Enabled:      boolPtr(false),
		ClientSecret: strPtr(strings.Repeat("s", 4096)),
	})
	if err != nil || disabled.Enabled {
		t.Fatalf("explicit disabled + max secret rejected: enabled=%v err=%v", disabled.Enabled, err)
	}

	tooManyScopes := make([]string, 21)
	tooManyScopes[0] = "openid"
	for i := 1; i < len(tooManyScopes); i++ {
		tooManyScopes[i] = "s"
	}

	invalidCases := map[string]oidcConfigInput{
		"missing openid scope": {
			ClientID: "client-1",
			Scopes:   []string{"email", "profile"},
		},
		"too many scopes": {
			ClientID: "client-1",
			Scopes:   tooManyScopes,
		},
		"whitespace in scope": {
			ClientID: "client-1",
			Scopes:   []string{"openid", "email profile"},
		},
		"oversized scope": {
			ClientID: "client-1",
			Scopes:   []string{"openid", strings.Repeat("a", 101)},
		},
		"missing client_id": {
			ClientID: "",
		},
		"invalid default_role": {
			ClientID:    "client-1",
			DefaultRole: strPtr("superadmin"),
		},
		"duplicate email mapping ignoring case": {
			ClientID: "client-1",
			EmailMappings: []roleMapping{
				{ClaimValue: "Ada@Example.com", Role: "owner"},
				{ClaimValue: "ada@example.com", Role: "viewer"},
			},
		},
		"client secret over 4096 bytes": {
			ClientID:     "client-1",
			ClientSecret: strPtr(strings.Repeat("s", 4097)),
		},
	}
	for name, tc := range invalidCases {
		if _, err := normalizeOIDCConfigInput(tc); err == nil {
			t.Errorf("%s: accepted invalid config input", name)
		}
	}
}

func TestValidateAndResolveRoleMappings(t *testing.T) {
	if err := validateRoleMappings(
		[]roleMapping{{ClaimValue: "ada@example.com", Role: "owner"}},
		[]roleMapping{{ClaimValue: "Admins", Role: "operator"}, {ClaimValue: "admins", Role: "viewer"}},
		"email_role_mappings",
		"group_role_mappings",
		"invalid",
	); err != nil {
		t.Fatalf("distinct-case groups and valid email rejected: %v", err)
	}

	var problem *httpkit.Problem
	err := validateRoleMappings(
		[]roleMapping{
			{ClaimValue: "Ada@Example.com", Role: "owner"},
			{ClaimValue: "ada@example.com", Role: "viewer"},
		},
		nil,
		"email_role_mappings",
		"group_role_mappings",
		"invalid",
	)
	if !errors.As(err, &problem) || problem.Field != "email_role_mappings" {
		t.Fatalf("case-insensitive email dup: got err=%v, want field=email_role_mappings", err)
	}

	err = validateRoleMappings(
		nil,
		[]roleMapping{
			{ClaimValue: "devs", Role: "developer"},
			{ClaimValue: "devs", Role: "operator"},
		},
		"email_role_mappings",
		"group_role_mappings",
		"invalid",
	)
	if !errors.As(err, &problem) || problem.Field != "group_role_mappings" {
		t.Fatalf("exact group dup: got err=%v, want field=group_role_mappings", err)
	}

	overLimit := make([]roleMapping, 501)
	for i := range overLimit {
		overLimit[i] = roleMapping{ClaimValue: strings.Repeat("a", 1+i%20) + string(rune('a'+i/20)), Role: "viewer"}
	}
	if err := validateRoleMappings(nil, overLimit, "email_role_mappings", "group_role_mappings", "invalid"); err == nil {
		t.Fatal("accepted more than 500 group mappings")
	}

	if err := validateRoleMappings([]roleMapping{{ClaimValue: "ada@example.com", Role: "unknown"}}, nil, "email_role_mappings", "group_role_mappings", "invalid"); err == nil {
		t.Fatal("accepted invalid role")
	}

	policy := roleMappingPolicy{
		DefaultRole: strPtr("viewer"),
		EmailMappings: []roleMapping{
			{ClaimValue: "Ada@Example.com", Role: "developer"},
		},
		GroupMappings: []roleMapping{
			{ClaimValue: "staff", Role: "viewer"},
			{ClaimValue: "owners", Role: "owner"},
			{ClaimValue: "ops", Role: "operator"},
		},
	}

	if got := resolveMappedRole(policy, "ada@example.com", []string{"owners", "ops"}); got != "developer" {
		t.Errorf("email precedence: got %q, want developer", got)
	}
	if got := resolveMappedRole(policy, "bob@example.com", []string{"staff", "ops", "owners"}); got != "owner" {
		t.Errorf("highest group rank: got %q, want owner", got)
	}
	if got := resolveMappedRole(policy, "bob@example.com", []string{"staff", "ops"}); got != "operator" {
		t.Errorf("operator over viewer: got %q, want operator", got)
	}
	if got := resolveMappedRole(policy, "bob@example.com", []string{"unmapped"}); got != "viewer" {
		t.Errorf("default role fallback: got %q, want viewer", got)
	}
	policy.DefaultRole = nil
	if got := resolveMappedRole(policy, "bob@example.com", []string{"unmapped"}); got != "" {
		t.Errorf("no default fallback: got %q, want empty", got)
	}
}

func TestOIDCSignInRoleValidation(t *testing.T) {
	cfg := oidcConfiguration{
		ClientID:        "client-1",
		Scopes:          []string{"openid", "email", "groups"},
		EmailClaim:      "email",
		GroupsClaim:     "groups",
		HasClientSecret: true,
		GroupMappings:   []roleMapping{{ClaimValue: "admins", Role: "owner"}},
	}
	marshalClaims := func(claims oidcRoleClaims) []byte {
		t.Helper()
		b, err := json.Marshal(claims)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	validClaims := marshalClaims(oidcRoleClaims{
		ClientID:    "client-1",
		Scopes:      []string{"openid", "email"},
		EmailClaim:  "email",
		GroupsClaim: "groups",
		Email:       "ada@example.com",
		Groups:      []string{"admins"},
	})

	if role, err := oidcSignInRole(cfg, false, "viewer", validClaims); err != nil || role != "owner" {
		t.Fatalf("OIDC-managed mapped role = %q, err = %v; want owner", role, err)
	}
	if role, err := oidcSignInRole(cfg, true, "operator", validClaims); err != nil || role != "operator" {
		t.Fatalf("locally-managed role = %q, err = %v; want operator", role, err)
	}

	noSecret := cfg
	noSecret.HasClientSecret = false
	if role, err := oidcSignInRole(noSecret, true, "owner", validClaims); err != nil || role != "" {
		t.Errorf("missing client secret: got %q, %v; want empty", role, err)
	}
	if role, err := oidcSignInRole(cfg, true, "owner", nil); err != nil || role != "" {
		t.Errorf("nil role_claims: got %q, %v; want empty", role, err)
	}
	if _, err := oidcSignInRole(cfg, true, "owner", []byte("{invalid")); err == nil {
		t.Error("malformed JSON accepted")
	}

	invalidatingCases := map[string][]byte{
		"client_id changed": marshalClaims(oidcRoleClaims{
			ClientID: "other-client", Scopes: []string{"openid", "email"}, EmailClaim: "email", GroupsClaim: "groups", Email: "ada@example.com", Groups: []string{"admins"},
		}),
		"email_claim changed": marshalClaims(oidcRoleClaims{
			ClientID: "client-1", Scopes: []string{"openid", "email"}, EmailClaim: "mail", GroupsClaim: "groups", Email: "ada@example.com", Groups: []string{"admins"},
		}),
		"groups_claim changed": marshalClaims(oidcRoleClaims{
			ClientID: "client-1", Scopes: []string{"openid", "email"}, EmailClaim: "email", GroupsClaim: "roles", Email: "ada@example.com", Groups: []string{"admins"},
		}),
		"empty recorded scopes": marshalClaims(oidcRoleClaims{
			ClientID: "client-1", Scopes: nil, EmailClaim: "email", GroupsClaim: "groups", Email: "ada@example.com", Groups: []string{"admins"},
		}),
		"scope removed from config": marshalClaims(oidcRoleClaims{
			ClientID: "client-1", Scopes: []string{"openid", "email", "profile"}, EmailClaim: "email", GroupsClaim: "groups", Email: "ada@example.com", Groups: []string{"admins"},
		}),
	}
	for name, raw := range invalidatingCases {
		if role, err := oidcSignInRole(cfg, false, "owner", raw); err != nil || role != "" {
			t.Errorf("%s: got role=%q err=%v, want empty role", name, role, err)
		}
	}
}

func TestSAMLSignInRoleValidation(t *testing.T) {
	cfg := samlConfiguration{
		SAMLDefinition: SAMLDefinition{
			Enabled:         true,
			EmailAttribute:  "mail",
			GroupsAttribute: "memberOf",
			GroupMappings:   []roleMapping{{ClaimValue: "owners", Role: "owner"}},
		},
	}
	marshalFacts := func(f samlFacts) []byte {
		t.Helper()
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	validFacts := marshalFacts(samlFacts{
		Email:           "ada@example.com",
		Groups:          []string{"owners"},
		EmailAttribute:  "mail",
		GroupsAttribute: "memberOf",
	})

	if role := samlSignInRole(cfg, "saml", "viewer", validFacts); role != "owner" {
		t.Fatalf("SAML-managed role = %q, want owner", role)
	}
	if role := samlSignInRole(cfg, "local", "operator", validFacts); role != "operator" {
		t.Fatalf("locally-managed role = %q, want operator", role)
	}

	disabled := cfg
	disabled.Enabled = false
	if role := samlSignInRole(disabled, "local", "owner", validFacts); role != "" {
		t.Errorf("disabled SAML: got %q, want empty", role)
	}

	for name, raw := range map[string][]byte{
		"empty facts":     nil,
		"malformed JSON":  []byte("not-json"),
		"email attr diff": marshalFacts(samlFacts{Email: "ada@example.com", Groups: []string{"owners"}, EmailAttribute: "email", GroupsAttribute: "memberOf"}),
		"group attr diff": marshalFacts(samlFacts{Email: "ada@example.com", Groups: []string{"owners"}, EmailAttribute: "mail", GroupsAttribute: "groups"}),
		"no mapped group": marshalFacts(samlFacts{Email: "ada@example.com", Groups: []string{"other"}, EmailAttribute: "mail", GroupsAttribute: "memberOf"}),
	} {
		if role := samlSignInRole(cfg, "saml", "owner", raw); role != "" {
			t.Errorf("%s: got %q, want empty", name, role)
		}
	}
}

func TestOIDCFailureRedirect(t *testing.T) {
	cases := []struct {
		code   string
		reason string
	}{
		{"oidc_authorization_denied", "cancelled"},
		{"oidc_flow_invalid", "expired"},
		{"oidc_session_changed", "expired"},
		{"oidc_recent_auth_required", "expired"},
		{"oidc_provisioning_denied", "denied"},
		{"account_disabled", "denied"},
		{"oidc_identity_linked", "link"},
		{"oidc_link_required", "link"},
		{"oidc_identity_mismatch", "link"},
		{"oidc_token_invalid", "provider"},
	}
	for _, tc := range cases {
		reply := oidcFailureRedirect(oidcFlow{ID: "flow-1", Kind: "login", ReturnTo: "/projects"}, httpkit.Fail(403, tc.code, "msg"))
		if reply.Status != http.StatusSeeOther {
			t.Fatalf("%s: status = %d, want 303", tc.code, reply.Status)
		}
		parsed, err := url.Parse(reply.Location)
		if err != nil {
			t.Fatalf("%s: invalid redirect location %q: %v", tc.code, reply.Location, err)
		}
		if parsed.Path != "/login" || parsed.Query().Get("oidc_error") != tc.reason || parsed.Query().Get("return_to") != "/projects" {
			t.Errorf("%s: location = %q, want /login with oidc_error=%s return_to=/projects", tc.code, reply.Location, tc.reason)
		}
		if len(reply.Cookies) != 1 || reply.Cookies[0].Name != "__Host-olp_oidc_login_flow-1" || reply.Cookies[0].MaxAge != -1 {
			t.Errorf("%s: cookies = %+v, want cleared flow cookie", tc.code, reply.Cookies)
		}
	}

	unsafeReturn := oidcFailureRedirect(oidcFlow{Kind: "login", ReturnTo: "//evil.example"}, errors.New("boom"))
	parsed, err := url.Parse(unsafeReturn.Location)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "/login" || parsed.Query().Get("oidc_error") != "provider" || parsed.Query().Has("return_to") {
		t.Errorf("unsafe return_to: location = %q, want /login?oidc_error=provider without return_to", unsafeReturn.Location)
	}
	if len(unsafeReturn.Cookies) != 0 {
		t.Errorf("empty flow ID should emit no cookies, got %d", len(unsafeReturn.Cookies))
	}

	for _, kind := range []string{"link", "reauthenticate"} {
		reply := oidcFailureRedirect(oidcFlow{Kind: kind, ReturnTo: "/projects"}, httpkit.Fail(403, "oidc_identity_mismatch", "msg"))
		loc, err := url.Parse(reply.Location)
		if err != nil {
			t.Fatal(err)
		}
		if loc.Path != "/settings/profile" || loc.Query().Get("oidc_error") != "link" || loc.Query().Has("return_to") {
			t.Errorf("%s: location = %q, want /settings/profile?oidc_error=link", kind, reply.Location)
		}
	}
}

func TestOIDCDiscoveryBindingCanonicalHash(t *testing.T) {
	issuer := "https://idp.example"
	authURL := "https://idp.example/oauth2/authorize?a=1&b=<tag>"
	tokenURL := "https://idp.example/oauth2/token"
	jwksURL := "https://idp.example/.well-known/jwks.json"

	for _, style := range []oauth2.AuthStyle{oauth2.AuthStyleInHeader, oauth2.AuthStyleInParams} {
		got, err := oidcDiscoveryBinding(issuer, authURL, tokenURL, jwksURL, style)
		if err != nil {
			t.Fatalf("style %d: unexpected error: %v", style, err)
		}
		legacyBytes, err := json.Marshal([]any{issuer, authURL, tokenURL, jwksURL, style})
		if err != nil {
			t.Fatal(err)
		}
		wantSum := sha256.Sum256(legacyBytes)
		want := hex.EncodeToString(wantSum[:])
		if got != want {
			t.Fatalf("style %d: binding = %s, want canonical array digest %s", style, got, want)
		}
	}

	inHeader, err := oidcDiscoveryBinding(issuer, "https://idp.example/authorize", tokenURL, jwksURL, oauth2.AuthStyleInHeader)
	if err != nil {
		t.Fatal(err)
	}
	inParams, err := oidcDiscoveryBinding(issuer, "https://idp.example/authorize", tokenURL, jwksURL, oauth2.AuthStyleInParams)
	if err != nil {
		t.Fatal(err)
	}
	if inHeader == inParams {
		t.Fatal("binding did not change when authStyle changed")
	}
	otherToken, err := oidcDiscoveryBinding(issuer, "https://idp.example/authorize", "https://idp.example/other-token", jwksURL, oauth2.AuthStyleInHeader)
	if err != nil {
		t.Fatal(err)
	}
	if inHeader == otherToken {
		t.Fatal("binding did not change when tokenURL changed")
	}
}
