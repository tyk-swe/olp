package workload

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

func fixtureConfig() Config {
	claim := "/customer"
	return Config{Name: "test", Issuer: "https://issuer.example", JWKSURL: "https://issuer.example/keys", Enabled: true, DisabledKeyIDs: []string{}, Audiences: []string{"gateway"}, Algorithms: []string{"RS256", "ES256", "EdDSA"}, MaxLifetimeSeconds: 600, Mappings: []Mapping{{Name: "build", Match: map[string]string{"/repository": "example/repo"}, ProjectID: "00000000-0000-4000-8000-000000000001", LimitTemplate: "build", RouteGroups: []string{"generation"}, Scopes: []string{"inference"}, EndUserClaim: &claim}}}
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
func TestKeySetsRejectPrivateWeakAndAmbiguousKeys(t *testing.T) {
	pub, private, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	good := jose.JSONWebKey{Key: pub, KeyID: "one", Algorithm: "EdDSA"}
	for _, set := range []jose.JSONWebKeySet{{Keys: []jose.JSONWebKey{good, good}}, {Keys: []jose.JSONWebKey{{Key: private, KeyID: "one"}}}, {Keys: []jose.JSONWebKey{{Key: []byte("secret"), KeyID: "one"}}}, {Keys: []jose.JSONWebKey{{Key: pub, KeyID: "one", Use: "enc"}}}} {
		data, _ := json.Marshal(set)
		if _, e = ParseKeys(data); e == nil {
			t.Fatal("accepted unusable key set")
		}
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

func TestCachedKeysDoNotWaitForAnUnknownKeyFetch(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	var calls atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			<-release
		}
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "known", Algorithm: "EdDSA"}}})
	}))
	defer server.Close()
	defer close(release)
	cache := NewCache(server.Client())
	now := time.Now()
	ctx := context.Background()
	if _, e := cache.Keys(ctx, "issuer", server.URL, "known", now); e != nil {
		t.Fatal(e)
	}
	// An unknown key holds a fetch open, and known keys still verify meanwhile,
	// both before and after they are due for a refresh.
	go cache.Keys(ctx, "issuer", server.URL, "unknown", now.Add(16*time.Second))
	for calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	for _, at := range []time.Duration{20 * time.Second, 2 * time.Minute} {
		done := make(chan error, 1)
		go func() {
			_, e := cache.Keys(ctx, "issuer", server.URL, "known", now.Add(at))
			done <- e
		}()
		select {
		case e := <-done:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(time.Second):
			t.Fatalf("a cached key waited on another fetch at %v", at)
		}
	}
}

func TestCachedKeysRefreshWithoutWaitingOnTheIssuer(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	var calls atomic.Int64
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			<-release
		}
		json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "known", Algorithm: "EdDSA"}}})
	}))
	defer server.Close()
	// A failed check must not leave the stalled fetch blocking the server's shutdown.
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	cache := NewCache(server.Client())
	now := time.Now()
	ctx := context.Background()
	if _, e := cache.Keys(ctx, "issuer", server.URL, "known", now); e != nil {
		t.Fatal(e)
	}
	// The caller that starts a due refresh answers from the cache while the
	// issuer stalls, and the refresh still lands once the issuer answers.
	done := make(chan error, 1)
	go func() {
		_, e := cache.Keys(ctx, "issuer", server.URL, "known", now.Add(2*time.Minute))
		done <- e
	}()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("a cached key waited on its own refresh")
	}
	for calls.Load() < 2 {
		time.Sleep(time.Millisecond)
	}
	unblock()
	deadline := time.Now().Add(time.Second)
	for {
		if _, e := cache.Keys(ctx, "issuer", server.URL, "known", now.Add(6*time.Minute)); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background refresh never landed")
		}
		time.Sleep(time.Millisecond)
	}
}
