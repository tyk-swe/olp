package workload

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

type fakeTransport struct {
	bodies  map[string][]byte
	counts  map[string]int
	failing map[string]bool
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	url := req.URL.String()
	f.counts[url]++
	if f.failing[url] {
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(bytes.NewReader(nil)), Header: http.Header{}, Request: req}, nil
	}
	body, ok := f.bodies[url]
	if !ok {
		return nil, errors.New("unknown url")
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body)), Header: http.Header{}, Request: req}, nil
}

func jwksBody(t *testing.T, kid string) []byte {
	t.Helper()
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: public, KeyID: kid, Algorithm: string(jose.EdDSA), Use: "sig"}}})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestKeysSweepsStaleIssuers(t *testing.T) {
	const (
		urlA = "https://a.example/jwks"
		urlB = "https://b.example/jwks"
	)
	transport := &fakeTransport{
		bodies:  map[string][]byte{urlA: jwksBody(t, "kid-a"), urlB: jwksBody(t, "kid-b")},
		counts:  map[string]int{},
		failing: map[string]bool{},
	}
	cache := NewCache(&http.Client{Transport: transport})
	t0 := time.Date(2026, time.November, 16, 10, 0, 0, 0, time.UTC)
	keys, err := cache.Keys(t.Context(), "issuer-a", urlA, "kid-a", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["kid-a"]; !ok || transport.counts[urlA] != 1 {
		t.Fatalf("got keys %v after %d fetches at t0, want kid-a after one fetch", keys, transport.counts[urlA])
	}
	transport.failing[urlA] = true
	if _, err := cache.Keys(t.Context(), "issuer-a", urlA, "kid-a", t0.Add(9*time.Minute+55*time.Second)); !errors.Is(err, ErrKeys) {
		t.Fatalf("got %v for a stale issuer whose refresh failed, want %v", err, ErrKeys)
	}
	if transport.counts[urlA] != 2 {
		t.Fatalf("got %d fetches after the failed refresh, want 2", transport.counts[urlA])
	}
	transport.failing[urlA] = false
	keys, err = cache.Keys(t.Context(), "issuer-b", urlB, "kid-b", t0.Add(10*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := keys["kid-b"]; !ok {
		t.Fatalf("got keys %v for issuer B, want kid-b", keys)
	}
	keys, err = cache.Keys(t.Context(), "issuer-a", urlA, "kid-a", t0.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("got %v for issuer A after issuer B was served, want a refetch", err)
	}
	if _, ok := keys["kid-a"]; !ok || transport.counts[urlA] != 3 {
		t.Fatalf("got keys %v after %d fetches, want kid-a after a third fetch", keys, transport.counts[urlA])
	}
}
