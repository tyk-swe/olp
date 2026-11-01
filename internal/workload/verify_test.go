package workload

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

func fixtureConfig() Config {
	claim := "/customer"
	return Config{Name: "test", Issuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys", Enabled: true, DisabledKeyIDs: []string{}, Audiences: []string{"gateway"}, Algorithms: []string{"RS256", "ES256", "EdDSA"}, MaxLifetimeSeconds: 600, Mappings: []Mapping{{Name: "build", Match: map[string]string{"/repository": "example/repo"}, ProjectID: "00000000-0000-4000-8000-000000000001", LimitTemplate: "build", RouteGroups: []string{"generation"}, Scopes: []string{"inference"}, EndUserClaim: &claim}}}
}

func TestMappingConsumerScopes(t *testing.T) {
	c := fixtureConfig()
	c.Mappings[0].Scopes = []string{"inference", "models_read", "guardrails", "tools", "agents", "traces_write", "delegate"}
	if err := c.Validate(); err != nil {
		t.Fatalf("advertised consumer scopes refused: %v", err)
	}
	c.Mappings[0].Scopes = []string{"unknown"}
	if c.Validate() == nil {
		t.Fatal("unknown consumer scope accepted")
	}
}

func TestMappingRouteScopesRequireExplicitGroups(t *testing.T) {
	for _, scope := range []string{"inference", "models_read"} {
		c := fixtureConfig()
		c.Mappings[0].RouteGroups = []string{}
		c.Mappings[0].Scopes = []string{scope}
		if c.Validate() == nil {
			t.Fatalf("%s accepted unrestricted empty route groups", scope)
		}
	}
	c := fixtureConfig()
	c.Mappings[0].RouteGroups = []string{}
	c.Mappings[0].Scopes = []string{"tools", "agents", "guardrails", "traces_write", "delegate"}
	if err := c.Validate(); err != nil {
		t.Fatalf("resource scopes required unrelated route grants: %v", err)
	}
}

func agentIssuerConfig(match map[string]string) Config {
	return Config{
		Name: "agents", Issuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys", Enabled: true,
		DisabledKeyIDs: []string{}, Audiences: []string{"gateway"}, Algorithms: []string{"EdDSA"}, MaxLifetimeSeconds: 600,
		Mappings: []Mapping{{
			Name: "worker", AgentPrincipal: true, Match: match,
			ProjectID: "00000000-0000-4000-8000-000000000001", LimitTemplate: "agent",
			RouteGroups: []string{"generation"}, Scopes: []string{"inference"},
		}},
	}
}

func TestAgentMappingRequiresSubjectAndClient(t *testing.T) {
	for _, test := range []struct {
		name    string
		match   map[string]string
		allowed bool
	}{
		{name: "unbound", match: map[string]string{}},
		{name: "subject only", match: map[string]string{"/sub": "worker"}},
		{name: "client only", match: map[string]string{"/client_id": "client"}},
		{name: "subject and client", match: map[string]string{"/sub": "worker", "/client_id": "client"}, allowed: true},
		{name: "subject and authorized party", match: map[string]string{"/sub": "worker", "/azp": "client"}, allowed: true},
		{name: "subject and application", match: map[string]string{"/sub": "worker", "/appid": "client"}, allowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := agentIssuerConfig(test.match).Validate()
			if (err == nil) != test.allowed {
				t.Fatalf("mapping allowed = %t, want %t: %v", err == nil, test.allowed, err)
			}
		})
	}
}

func TestAgentMappingVerifiesBothSignedClaims(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	config := agentIssuerConfig(map[string]string{"/sub": "worker", "/client_id": "client"})
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	keys := map[string]jose.JSONWebKey{"agent": {Key: public, KeyID: "agent", Algorithm: string(jose.EdDSA)}}
	now := time.Now()
	for _, test := range []struct {
		name    string
		subject string
		client  string
		allowed bool
	}{
		{name: "bound identity", subject: "worker", client: "client", allowed: true},
		{name: "different subject", subject: "other", client: "client"},
		{name: "different client", subject: "worker", client: "other"},
		{name: "missing client", subject: "worker"},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := struct {
				Issuer   string   `json:"iss"`
				Subject  string   `json:"sub"`
				ClientID string   `json:"client_id,omitempty"`
				Audience []string `json:"aud"`
				Issued   int64    `json:"iat"`
				Expires  int64    `json:"exp"`
			}{config.Issuer, test.subject, test.client, config.Audiences, now.Unix() - 1, now.Unix() + 120}
			token, err := Parse(sign(t, jose.EdDSA, private, "agent", claims))
			if err != nil {
				t.Fatal(err)
			}
			identity, err := token.Verify(config, keys, now)
			if (err == nil) != test.allowed {
				t.Fatalf("identity allowed = %t, want %t: %v", err == nil, test.allowed, err)
			}
			if test.allowed && (identity.Subject != test.subject || !identity.Mapping.AgentPrincipal) {
				t.Fatalf("verified identity lost the agent binding: %+v", identity)
			}
		})
	}
}

func fixtureClaims(now time.Time) map[string]any {
	return map[string]any{"iss": "https://issuer.example", "sub": "private-service-account", "aud": []string{"gateway"}, "iat": now.Unix() - 1, "exp": now.Unix() + 120, "nbf": now.Unix() - 1, "repository": "example/repo", "customer": "customer-1"}
}
func sign(t *testing.T, alg jose.SignatureAlgorithm, key any, kid string, claims any) string {
	t.Helper()
	s, e := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", kid))
	if e != nil {
		t.Fatal(e)
	}
	body, e := json.Marshal(claims)
	if e != nil {
		t.Fatal(e)
	}
	j, e := s.Sign(body)
	if e != nil {
		t.Fatal(e)
	}
	token, e := j.CompactSerialize()
	if e != nil {
		t.Fatal(e)
	}
	return token
}
func TestSupportedSignaturesAndEverySecurityClaim(t *testing.T) {
	rsaKey, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	ecKey, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	pub, edKey, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	for _, key := range []struct {
		alg             jose.SignatureAlgorithm
		private, public any
	}{{jose.RS256, rsaKey, &rsaKey.PublicKey}, {jose.ES256, ecKey, &ecKey.PublicKey}, {jose.EdDSA, edKey, pub}} {
		t.Run(string(key.alg), func(t *testing.T) {
			now := time.Now()
			keys := map[string]jose.JSONWebKey{"one": {Key: key.public, KeyID: "one", Algorithm: string(key.alg), Use: "sig"}}
			c := fixtureConfig()
			if e = c.Validate(); e != nil {
				t.Fatal(e)
			}
			raw := sign(t, key.alg, key.private, "one", fixtureClaims(now))
			token, e := Parse(raw)
			if e != nil {
				t.Fatal(e)
			}
			if !token.Allowed(c, now) || token.Allowed(c, now.Add(10*time.Minute)) {
				t.Fatal("expiry preflight must refuse without fetching keys")
			}
			identity, e := token.Verify(c, keys, now)
			if e != nil || identity.Subject != "private-service-account" || identity.Mapping.Name != "build" || identity.EndUser != "customer-1" {
				t.Fatalf("valid mapped identity: %v", e)
			}
			mutations := map[string]any{"iss": "https://other.example", "aud": []string{"another"}, "exp": now.Unix(), "iat": now.Unix() + 1, "nbf": now.Unix() + 1, "sub": "", "repository": "other/repo", "customer": nil}
			for claim, value := range mutations {
				t.Run(claim, func(t *testing.T) {
					claims := fixtureClaims(now)
					claims[claim] = value
					token, e := Parse(sign(t, key.alg, key.private, "one", claims))
					if e == nil {
						_, e = token.Verify(c, keys, now)
					}
					if e == nil {
						t.Fatal("accepted invalid claim")
					}
				})
			}
			claims := fixtureClaims(now)
			claims["exp"] = now.Unix() + 601
			token, _ = Parse(sign(t, key.alg, key.private, "one", claims))
			if _, e = token.Verify(c, keys, now); e == nil {
				t.Fatal("unbounded lifetime")
			}
			c.DisabledKeyIDs = []string{"one"}
			token, _ = Parse(raw)
			if _, e = token.Verify(c, keys, now); e == nil {
				t.Fatal("disabled key accepted")
			}
			c.DisabledKeyIDs = nil
			c.Enabled = false
			if _, e = token.Verify(c, keys, now); e == nil {
				t.Fatal("disabled issuer accepted")
			}
		})
	}
}

func TestCachedKeyRotationAndBoundedOutage(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	var generation atomic.Int64
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		kid := "old"
		if generation.Load() != 0 {
			kid = "new"
		}
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: kid, Algorithm: "EdDSA"}}})
	}))
	defer server.Close()
	cache := NewCache(server.Client())
	now := time.Now()
	ctx := context.Background()
	if _, e := cache.Keys(ctx, "issuer", server.URL, "old", now); e != nil {
		t.Fatal(e)
	}
	if _, e := cache.Keys(ctx, "issuer", server.URL, "old", now.Add(time.Second)); e != nil || calls.Load() != 1 {
		t.Fatal("cached request fetched")
	}
	generation.Store(1)
	keys, e := cache.Keys(ctx, "issuer", server.URL, "new", now.Add(16*time.Second))
	if e != nil || keys["new"].Key == nil || keys["old"].Key != nil {
		t.Fatal("rotation did not replace keys")
	}
	server.Close()
	if _, e = cache.Keys(ctx, "issuer", server.URL, "new", now.Add(2*time.Minute)); e != nil {
		t.Fatal("bounded last-good keys unavailable")
	}
	if _, e = cache.Keys(ctx, "issuer", server.URL, "new", now.Add(6*time.Minute)); e == nil {
		t.Fatal("stale keys survived indefinitely")
	}
}

// An unknown key holds a fetch open, and known keys still verify meanwhile,
// both before and after they are due for a refresh.

// A failed check must not leave the stalled fetch blocking the server's shutdown.

// The caller that starts a due refresh answers from the cache while the
// issuer stalls, and the refresh still lands once the issuer answers.

type entraUserClaims struct {
	Issuer       string          `json:"iss"`
	Subject      string          `json:"sub"`
	Audience     string          `json:"aud"`
	IssuedAt     int64           `json:"iat"`
	Expires      int64           `json:"exp"`
	Repository   string          `json:"repository"`
	EndUser      string          `json:"customer"`
	Scopes       json.RawMessage `json:"scp,omitempty"`
	IdentityType json.RawMessage `json:"idtyp,omitempty"`
}

func TestEntraUserRequiresDelegatedClaimsOnVerifiedToken(t *testing.T) {
	public, private, keyErr := ed25519.GenerateKey(rand.Reader)
	if keyErr != nil {
		t.Fatal(keyErr)
	}
	config := fixtureConfig()
	keys := map[string]jose.JSONWebKey{"entra": {Key: public, KeyID: "entra", Algorithm: string(jose.EdDSA), Use: "sig"}}
	now := time.Now()
	for _, test := range []struct {
		name         string
		issuer       string
		audience     string
		scopes       json.RawMessage
		identityType json.RawMessage
		expires      time.Duration
		accepted     bool
	}{
		{"delegated-user", config.Issuer, "gateway", json.RawMessage(`"tools.read tools.write"`), json.RawMessage(`"user"`), time.Minute, true},
		{"delegated-without-type", config.Issuer, "gateway", json.RawMessage(`"tools.read"`), nil, time.Minute, true},
		{"wrong-issuer", config.Issuer + "/other", "gateway", json.RawMessage(`"tools.read"`), nil, time.Minute, false},
		{"wrong-audience", config.Issuer, "other", json.RawMessage(`"tools.read"`), nil, time.Minute, false},
		{"missing-scopes", config.Issuer, "gateway", nil, nil, time.Minute, false},
		{"blank-scopes", config.Issuer, "gateway", json.RawMessage(`"   "`), nil, time.Minute, false},
		{"wrong-scope-type", config.Issuer, "gateway", json.RawMessage(`["tools.read"]`), nil, time.Minute, false},
		{"bounded-scopes", config.Issuer, "gateway", json.RawMessage(`"` + strings.Repeat("x", 4097) + `"`), nil, time.Minute, false},
		{"application", config.Issuer, "gateway", json.RawMessage(`"tools.read"`), json.RawMessage(`"app"`), time.Minute, false},
		{"wrong-identity-type", config.Issuer, "gateway", json.RawMessage(`"tools.read"`), json.RawMessage(`true`), time.Minute, false},
		{"expired", config.Issuer, "gateway", json.RawMessage(`"tools.read"`), nil, -time.Second, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			claims := entraUserClaims{Issuer: config.Issuer, Subject: "private-subject", Audience: "gateway", IssuedAt: now.Add(-time.Minute).Unix(), Expires: now.Add(test.expires).Unix(), Repository: "example/repo", EndUser: "private-end-user", Scopes: test.scopes, IdentityType: test.identityType}
			raw := sign(t, jose.EdDSA, private, "entra", claims)
			token, parseErr := Parse(raw)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			if _, verifyErr := token.Verify(config, keys, now.Add(-2*time.Second)); verifyErr != nil {
				t.Fatal(verifyErr)
			}
			assertion, err := token.EntraUser(test.issuer, test.audience)
			if !test.accepted {
				if err == nil {
					t.Fatal("nondelegated or mismatched identity became a user assertion")
				}
				return
			}
			if err != nil || assertion.Token() != raw || !assertion.ExpiresAt().Equal(time.Unix(claims.Expires, 0)) {
				t.Fatal("verified delegated assertion lost its token or expiry")
			}
			if strings.Contains(fmt.Sprint(assertion), raw) || strings.Contains(fmt.Sprint(assertion), claims.Subject) {
				t.Fatal("formatted assertion leaked private identity")
			}
		})
	}
}

func TestRequestAssertionStaysInItsRequestContext(t *testing.T) {
	base := context.Background()
	first := WithAssertion(base, "first-private-assertion")
	second := WithAssertion(base, "second-private-assertion")
	if RequestAssertion(base) != "" || RequestAssertion(first) != "first-private-assertion" || RequestAssertion(second) != "second-private-assertion" {
		t.Fatal("request assertion crossed context boundaries")
	}
	if RequestAssertion(context.WithValue(first, struct{}{}, true)) != "first-private-assertion" {
		t.Fatal("nested request context lost its assertion")
	}
}
