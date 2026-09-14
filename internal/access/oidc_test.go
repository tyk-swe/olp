package access

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
)

type oidcDiscoveryTransport struct{ metadata map[string]string }

func (transport oidcDiscoveryTransport) RoundTrip(*http.Request) (*http.Response, error) {
	body, err := json.Marshal(transport.metadata)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
}

func TestOIDCDiscoveryValidatesAdvertisedAddresses(t *testing.T) {
	for _, field := range []string{"issuer", "authorization_endpoint", "token_endpoint", "jwks_uri"} {
		for _, tc := range []struct {
			endpoint string
			allowed  bool
		}{
			{"https://8.8.8.8/endpoint", true},
			{"https://[2606:4700:4700::1111]/endpoint", true},
			{"https://10.0.0.1/endpoint", false},
			{"https://172.16.0.1/endpoint", false},
			{"https://192.168.1.1/endpoint", false},
			{"https://169.254.169.254/endpoint", false},
			{"https://100.64.0.1/endpoint", false},
			{"https://192.0.2.1/endpoint", false},
			{"https://[fd00::1]/endpoint", false},
			{"https://[fe80::1]/endpoint", false},
			{"https://[::ffff:10.0.0.1]/endpoint", false},
			{"https://127.0.0.1/endpoint", oidcTestBuild},
			{"https://[::1]/endpoint", oidcTestBuild},
			{"https://localhost/endpoint", oidcTestBuild},
			{"http://127.0.0.1/endpoint", oidcTestBuild},
			{"http://8.8.8.8/endpoint", false},
			{"https://user:password@8.8.8.8/endpoint", false},
			{"https://8.8.8.8/endpoint#fragment", false},
			{"", false},
		} {
			t.Run(field+"/"+tc.endpoint, func(t *testing.T) {
				metadata := map[string]string{
					"issuer":                 "https://8.8.8.8",
					"authorization_endpoint": "https://8.8.8.8/authorize",
					"token_endpoint":         "https://8.8.8.8/token",
					"jwks_uri":               "https://8.8.8.8/jwks",
				}
				metadata[field] = tc.endpoint
				s := &Server{Origin: "https://console.test", OIDCClient: &http.Client{Transport: oidcDiscoveryTransport{metadata}}}
				provider, oauth, err := s.discover(t.Context(), oidcConfiguration{
					DiscoveryURL: "https://8.8.8.8/.well-known/openid-configuration",
					Issuer:       metadata["issuer"], ClientID: "test-client", Scopes: []string{"openid"},
				})
				if tc.allowed {
					if err != nil || provider == nil || oauth == nil {
						t.Fatalf("rejected allowed endpoint: %v", err)
					}
					if oauth.Endpoint.AuthURL != metadata["authorization_endpoint"] || oauth.Endpoint.TokenURL != metadata["token_endpoint"] {
						t.Fatal("discovered endpoints changed")
					}
					return
				}
				var p *Problem
				if !errors.As(err, &p) || p.Status != 422 || p.Field != "discovery_url" || provider != nil || oauth != nil {
					t.Fatalf("got %v, want unsafe discovery endpoint rejection", err)
				}
			})
		}
	}
}

func TestOIDCSignInRoleRequiresVerifiedScopes(t *testing.T) {
	verifiedScopes := []string{"openid", "email", "profile", "groups"}
	for _, tc := range []struct {
		name     string
		scopes   []string
		verified []string
		want     string
	}{
		{"unchanged", verifiedScopes, verifiedScopes, "owner"},
		{"reordered", []string{"groups", "profile", "email", "openid"}, verifiedScopes, "owner"},
		{"expanded", []string{"openid", "email", "profile", "groups", "offline_access"}, verifiedScopes, "owner"},
		{"email_removed", []string{"openid", "profile", "groups"}, verifiedScopes, ""},
		{"groups_removed", []string{"openid", "email", "profile"}, verifiedScopes, ""},
		{"scope_replaced", []string{"openid", "email", "profile", "roles"}, verifiedScopes, ""},
		{"scopes_missing", verifiedScopes, nil, ""},
		{"scopes_empty", verifiedScopes, []string{}, ""},
		{"reverified_reduction", []string{"openid"}, []string{"openid"}, "owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := "owner"
			configuration := oidcConfiguration{
				ClientID: "test-client", HasClientSecret: true, Scopes: tc.scopes,
				EmailClaim: "email", GroupsClaim: "groups", DefaultRole: &role,
			}
			data, err := json.Marshal(oidcRoleClaims{
				ClientID: "test-client", Scopes: tc.verified,
				EmailClaim: "email", GroupsClaim: "groups", Email: "owner@example.com",
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, local := range []bool{false, true} {
				got, err := oidcSignInRole(configuration, local, role, data)
				if err != nil || got != tc.want {
					t.Fatalf("local=%t: got role %q, error %v; want %q", local, got, err, tc.want)
				}
			}
		})
	}
}
