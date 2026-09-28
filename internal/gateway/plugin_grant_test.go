package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// A grant profile whose address begins with a grant fact serves each attempt
// at its credential version's base URL. A grant whose base URL lies outside
// the plugin's approved origins can't serve: its attempt fails as a
// credential failure before anything is sent, and the route fails over.
func TestPluginGrantBaseURLPlacesEachAttempt(t *testing.T) {
	for name, approved := range map[string]bool{"approved origin": true, "unapproved origin": false} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{})
			profile := abi.Profile{ID: "acme-account", Label: "Acme account", Dialect: "openai-chat",
				Grant:   &abi.GrantAuthentication{Facts: []string{"api_base"}},
				Hosting: abi.Hosting{Address: "{grant.api_base}/v1", Headers: map[string]string{"Authorization": "Bearer {credential}"}},
			}
			digest := strings.Repeat("ef", 32)
			plugin, err := connectors.NewPluginProfile(digest, abi.Manifest{Name: "acme", Version: "1.0.0", Origins: []string{h.upstream.URL}, Profiles: []abi.Profile{profile}}, profile.ID)
			if err != nil {
				t.Fatal(err)
			}
			base := h.upstream.URL + "/a"
			if !approved {
				base = strings.Replace(base, "127.0.0.1", "127.0.0.2", 1)
			}
			grant, _ := json.Marshal(connectors.GrantCredential{AccessToken: "at-123", Facts: map[string]string{"api_base": base}})
			secrets := map[string][]byte{}
			for id, provider := range h.rt.release.Snapshot.Providers {
				for _, slot := range provider.Slots {
					secrets[*slot.CredentialID], _ = h.rt.release.Credential(*slot.CredentialID)
				}
				if provider.Slots[0].ID == h.slotA {
					provider.Kind, provider.AuthMode, provider.Plugin = connectors.KindPlugin, connectors.AuthGrant, plugin
					provider.ProfileID, provider.ProfileRevision, provider.Endpoint = profile.ID, digest, plugin.Address(nil)
					h.rt.release.Snapshot.Providers[id] = provider
				}
			}
			secrets[h.credA] = grant
			if h.rt.release, err = runtime.NewRelease(uuid.NewString(), 7, h.rt.release.Snapshot, secrets); err != nil {
				t.Fatal(err)
			}
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/a/v1/chat/completions" || r.Host != strings.TrimPrefix(h.upstream.URL, "http://") || r.Header.Get("Authorization") != "Bearer at-123" {
					t.Errorf("the upstream received %s %s with %v", r.Host, r.URL, r.Header)
				}
				completion(modelA, answerText)(w, r)
			})

			resp, body := h.chat(fullKey, nil)
			if resp.StatusCode != http.StatusOK || body["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != answerText {
				t.Fatalf("served %d %v", resp.StatusCode, body)
			}
			attempts := h.sink.last(t).Attempts
			if approved {
				if len(attempts) != 1 || attempts[0].Class != classSuccess || h.mock.count("a") != 1 {
					t.Fatalf("attempts %+v", attempts)
				}
				return
			}
			refused := attempts[0]
			if len(attempts) != 2 || refused.Class != classCredential || refused.Status != 0 || refused.FirstByte != nil || refused.BillingUncertain ||
				h.mock.count("a") != 0 || attempts[1].Class != classSuccess {
				t.Fatalf("attempts %+v", attempts)
			}
		})
	}
}
