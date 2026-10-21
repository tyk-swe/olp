package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// hashSigner stands in for a plugin's signing hook: it signs a request with a
// hash of the credential and body, or fails when told to.
type hashSigner struct {
	mu    sync.Mutex
	calls int
	fail  error
}

func (s *hashSigner) Sign(_ context.Context, _ string, _ abi.Provider, request abi.SignRequest, _ []string) (abi.SignResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.fail != nil {
		return abi.SignResult{}, s.fail
	}
	return abi.SignResult{Headers: map[string]string{"X-Acme-Signature": signature(request.Credential, request.Body)}}, nil
}

func (s *hashSigner) signed() int { s.mu.Lock(); defer s.mu.Unlock(); return s.calls }

// recover makes the hook sign again.
func (s *hashSigner) recover() { s.mu.Lock(); defer s.mu.Unlock(); s.fail = nil }

func signature(credential string, body []byte) string {
	sum := sha256.Sum256(append([]byte(credential), body...))
	return hex.EncodeToString(sum[:])
}
