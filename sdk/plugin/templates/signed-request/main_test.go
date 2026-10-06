package main

import (
	"context"
	"testing"
	"time"

	"github.com/tyk-swe/olp/sdk/plugin"
)

func fixedClock(t *testing.T) {
	t.Helper()
	previous := now
	now = func() time.Time { return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { now = previous })
}

func sign(t *testing.T, rawURL, body string) plugin.SignResult {
	t.Helper()
	result, err := signedRequest{}.Sign(context.Background(), plugin.SignRequest{Method: "POST", URL: rawURL, Body: []byte(body), Credential: "key-1:secret"})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// TestSignatureMatchesAKnownAnswer pins the scheme against a signature computed
// independently of this code (Python's hmac over the same canonical request),
// so a refactor that changes it fails here before the upstream refuses it.
func TestSignatureMatchesAKnownAnswer(t *testing.T) {
	fixedClock(t)
	result := sign(t, "https://api.example.com/v1/chat/completions?b=2&a=1", `{"model":"m"}`)
	if result.Headers["X-Api-Key-Id"] != "key-1" || result.Headers["X-Api-Date"] != "20261005T120000Z" {
		t.Fatalf("headers = %v", result.Headers)
	}
	const want = "ef69dd218063ddd9ce7ede436a7b952063167689371a2f65acd964fb02b35e0f"
	if got := result.Headers["X-Api-Signature"]; got != want {
		t.Fatalf("signature = %s, want %s", got, want)
	}
}

func TestSignatureCoversTheBodyButNotTheQueryOrder(t *testing.T) {
	fixedClock(t)
	a := sign(t, "https://api.example.com/v1/chat/completions?b=2&a=1", `{}`)
	b := sign(t, "https://api.example.com/v1/chat/completions?a=1&b=2", `{}`)
	c := sign(t, "https://api.example.com/v1/chat/completions?a=1&b=2", `{"tampered":true}`)
	if a.Headers["X-Api-Signature"] != b.Headers["X-Api-Signature"] || b.Headers["X-Api-Signature"] == c.Headers["X-Api-Signature"] {
		t.Fatal("the signature does not cover exactly the canonical request")
	}
}

func TestMalformedCredentialsAreRefused(t *testing.T) {
	if _, err := (signedRequest{}).Sign(context.Background(), plugin.SignRequest{URL: "https://api.example.com/v1", Credential: "no-separator"}); err == nil {
		t.Fatal("a credential without a key ID was used")
	}
}
