package gateway

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/runtime"
)

func TestRevocationDuringAnAttemptSkipsSiblingWithoutConsumingBudget(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			h := newHarness(t, Config{})
			release := h.rt.release
			snapshot := release.Snapshot
			siblingID, credentialID := uuid.NewString(), uuid.NewString()
			credentials := map[string][]byte{credentialID: []byte("sibling-secret")}
			for id, provider := range snapshot.Providers {
				for _, slot := range provider.Slots {
					credentials[*slot.CredentialID], _ = release.Credential(*slot.CredentialID)
				}
				if provider.Name == "a" {
					provider.Slots = append(provider.Slots, runtime.Slot{ID: siblingID, Name: "sibling", Enabled: true, Priority: 1, Weight: 1, CredentialID: &credentialID})
					snapshot.Providers[id] = provider
				}
			}
			var err error
			h.rt.release, err = runtime.NewRelease(release.ID, release.Sequence, snapshot, credentials)
			if err != nil {
				t.Fatal(err)
			}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+secretA {
					t.Error("dispatched the revoked sibling credential")
					completion(modelA, answerText)(w, r)
					return
				}
				// The first call is pending when authority revokes its sibling.
				h.rt.mu.Lock()
				h.rt.revoked[credentialID] = true
				h.rt.mu.Unlock()
				status(code, `{}`)(w, r)
			})
			resp, body := h.chat(fullKey, map[string]string{routingHeader: `{"max_attempts":2}`})
			if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 || h.mock.count("b") != 1 {
				t.Fatalf("status %d body %v calls a=%d b=%d", resp.StatusCode, body, h.mock.count("a"), h.mock.count("b"))
			}
			env := h.sink.last(t)
			if len(env.Attempts) != 2 || env.Attempts[0].CredentialID != h.credA || env.Attempts[1].UpstreamModel != modelB || env.Attempts[1].Ordinal != 2 || env.Outcome != "success" {
				t.Fatalf("envelope %+v", env)
			}
		})
	}
}
