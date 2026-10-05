package catalog

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/tyk-swe/olp/internal/signing"
)

func testKey(t *testing.T) (signing.Key, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return signing.Key{ID: "test-key", Public: public}, private
}
