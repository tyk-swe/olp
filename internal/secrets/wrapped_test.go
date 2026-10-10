package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/secretstore"
)

type unwrapFunc func(context.Context, secretstore.WrappedKey) ([]byte, error)

func (f unwrapFunc) Unwrap(ctx context.Context, key secretstore.WrappedKey) ([]byte, error) {
	return f(ctx, key)
}

func TestWrappedRingRetainsOldVersionsAndSealsWithTheNewActiveKey(t *testing.T) {
	old, err := ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := old.Seal("installation", ProviderCredential, "credential", []byte("private-upstream-key"))
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{"active_version": 2, "keys": []any{
		map[string]any{"version": 1, "wrapped": secretstore.WrappedKey{Store: "vault", KeyID: "https://vault.example/v1/transit/keys/old", Ciphertext: "vault:v1:old"}},
		map[string]any{"version": 2, "wrapped": secretstore.WrappedKey{Store: "vault", KeyID: "https://vault.example/v1/transit/keys/new", Ciphertext: "vault:v1:new"}},
	}}
	data, _ := json.Marshal(doc)
	calls := 0
	ring, err := ParseRingWithUnwrapper(t.Context(), data, unwrapFunc(func(_ context.Context, key secretstore.WrappedKey) ([]byte, error) {
		calls++
		if strings.HasSuffix(key.KeyID, "/old") {
			return DecodeKey(strings.Repeat("ab", 32))
		}
		return DecodeKey(strings.Repeat("ef", 32))
	}))
	if err != nil || calls != 2 || ring.Active != 2 {
		t.Fatalf("ring: %v, calls %d", err, calls)
	}
	plaintext, err := ring.Open("installation", ProviderCredential, "credential", 1, encrypted)
	if err != nil || string(plaintext) != "private-upstream-key" {
		t.Fatal("old version was not retained")
	}
	resealed, err := ring.Seal("installation", ProviderCredential, "credential", plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ring.Open("installation", ProviderCredential, "credential", 2, resealed); err != nil {
		t.Fatal(err)
	}
	if _, err = old.Open("installation", ProviderCredential, "credential", 1, resealed); err == nil {
		t.Fatal("new records still used the old key")
	}
}

func TestWrappedRingRefusesAmbiguousMissingAndFailedUnwraps(t *testing.T) {
	wrapped := `{"store":"vault","key_id":"https://vault.example/v1/transit/keys/key","ciphertext":"vault:v1:key"}`
	for _, entry := range []string{`{"version":1}`, `{"version":1,"key":"` + strings.Repeat("ab", 32) + `","wrapped":` + wrapped + `}`, `{"version":1,"wrapped":` + wrapped + `}`} {
		data := []byte(`{"active_version":1,"keys":[` + entry + `]}`)
		if ring, err := ParseRing(data); err == nil || ring != nil {
			t.Fatal("ring accepted unresolved or ambiguous key")
		}
	}
	data := []byte(`{"active_version":1,"keys":[{"version":1,"wrapped":` + wrapped + `}]}`)
	for _, unwrapper := range []unwrapFunc{
		func(context.Context, secretstore.WrappedKey) ([]byte, error) {
			return nil, errors.New("private-upstream-response")
		},
		func(context.Context, secretstore.WrappedKey) ([]byte, error) { return []byte("private-short-key"), nil },
	} {
		if ring, err := ParseRingWithUnwrapper(t.Context(), data, unwrapper); err == nil || ring != nil || strings.Contains(err.Error(), "private") {
			t.Fatal("invalid key was installed or disclosed")
		}
	}
}
