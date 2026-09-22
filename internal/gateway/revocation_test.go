package gateway

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestNetworkCredentialRevocationAfterPlanningPreservesAttemptBudget(t *testing.T) {
	for _, authMode := range []string{"api_key", "none"} {
		for _, revoke := range []bool{false, true} {
			name := authMode + "/available"
			if revoke {
				name = authMode + "/revoked_after_planning"
			}
			t.Run(name, func(t *testing.T) {
				h := newHarness(t, Config{})
				upstream := httptest.NewTLSServer(h.mock)
				t.Cleanup(upstream.Close)
				key, err := x509.MarshalPKCS8PrivateKey(upstream.TLS.Certificates[0].PrivateKey)
				if err != nil {
					t.Fatal(err)
				}
				certificate := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}))
				networkSecret, err := json.Marshal(map[string]string{
					"client_certificate_pem": certificate,
					"client_key_pem":         string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})),
				})
				if err != nil {
					t.Fatal(err)
				}
				networkID, credentialC := uuid.NewString(), uuid.NewString()
				release := h.rt.release
				snapshot := release.Snapshot
				credentials := map[string][]byte{networkID: networkSecret, credentialC: []byte("secret-c")}
				var providerC runtime.Provider
				for id, provider := range snapshot.Providers {
					for _, slot := range provider.Slots {
						credentials[*slot.CredentialID], _ = release.Credential(*slot.CredentialID)
					}
					if provider.Name == "b" {
						providerC = provider
						provider.Network = &egress.ConnectionOptions{CredentialID: networkID, TrustRootsPEM: certificate}
						provider.Endpoint = upstream.URL + "/b/v1"
						provider.AuthMode = authMode
						snapshot.Providers[id] = provider
					}
				}
				const modelC = "model-c"
				providerC.ID, providerC.Name, providerC.RevisionID = uuid.NewString(), "c", uuid.NewString()
				providerC.Endpoint = h.upstream.URL + "/c/v1"
				providerC.ActiveCredential = &credentialC
				providerC.Slots = []runtime.Slot{{ID: uuid.NewString(), Name: "default", Enabled: true, Weight: 1, CredentialID: &credentialC}}
				providerC.Capabilities = []runtime.Capability{{Model: modelC, Operation: "generation", Surface: "openai", Mode: "unary"}}
				snapshot.Providers[providerC.ID] = providerC
				route := snapshot.Routes[routeSlug]
				route.Targets = append(route.Targets, runtime.Target{ID: uuid.NewString(), ProviderID: providerC.ID, ProviderModel: modelC, Priority: 2, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()})
				snapshot.Routes[routeSlug] = route
				h.rt.release, err = runtime.NewRelease(release.ID, release.Sequence, snapshot, credentials)
				if err != nil {
					t.Fatal(err)
				}
				h.mock.set("c", completion(modelC, answerText))
				h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
					// Planning admitted every provider before this call started.
					// Revoke a later candidate while the first Attempt is pending.
					if revoke {
						h.rt.mu.Lock()
						h.rt.revoked[networkID] = true
						h.rt.mu.Unlock()
					}
					status(http.StatusServiceUnavailable, `{}`)(w, r)
				})
				resp, body := h.chat(fullKey, map[string]string{routingHeader: `{"max_attempts":2}`})
				wantModel, wantB, wantC := modelB, 1, 0
				if revoke {
					wantModel, wantB, wantC = modelC, 0, 1
				}
				if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 || h.mock.count("b") != wantB || h.mock.count("c") != wantC {
					t.Fatalf("status %d body %v calls a=%d b=%d c=%d", resp.StatusCode, body, h.mock.count("a"), h.mock.count("b"), h.mock.count("c"))
				}
				env := h.sink.last(t)
				if len(env.Attempts) != 2 || env.Attempts[0].UpstreamModel != modelA || env.Attempts[1].UpstreamModel != wantModel || env.Attempts[1].Ordinal != 2 || env.Outcome != "success" {
					t.Fatalf("envelope %+v", env)
				}
			})
		}
	}
}

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
