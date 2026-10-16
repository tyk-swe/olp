package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"
)

func callerHarness(t *testing.T) *harness {
	t.Helper()
	h := strictHarness(t, nil)
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		for id, p := range s.Providers {
			p.CredentialSource = "caller"
			s.Providers[id] = p
		}
	})
	return h
}

func TestCallerCredentialRefusalDoesNotPoisonProbeOrCrossTargets(t *testing.T) {
	h := callerHarness(t)
	const secret = "rejected-caller-secret"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "rejected " + secret, "type": "authentication_error"}})
	})
	response, body := h.chat(fullKey, map[string]string{callerCredentialHeader: secret})
	encoded, _ := json.Marshal(body)
	if response.StatusCode < 400 || bytes.Contains(encoded, []byte(secret)) || h.mock.count("b") != 0 {
		t.Fatalf("refusal leaked, succeeded, or crossed targets: %d %s", response.StatusCode, encoded)
	}
	h.mock.set("a", completion(modelA, answerText))
	response, _ = h.chat(fullKey, map[string]string{callerCredentialHeader: "valid-new-caller"})
	if response.StatusCode != 200 {
		t.Fatalf("caller failure poisoned operator probe or pool: %d", response.StatusCode)
	}
}
