package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

func strictHarness(t *testing.T, configure func(*runtime.Snapshot)) *harness {
	t.Helper()
	h := newHarness(t, Config{})
	release := h.rt.release
	snapshot := release.Snapshot
	credentials := map[string][]byte{}
	for id, provider := range snapshot.Providers {
		provider.ProfileID, provider.ProfileRevision = "compatible-chat", connectors.ProfileRevision
		snapshot.Providers[id] = provider
		for _, slot := range provider.Slots {
			credentials[*slot.CredentialID], _ = release.Credential(*slot.CredentialID)
		}
	}
	route := snapshot.Routes[routeSlug]
	route.Fidelity = runtime.RouteFidelity{Mode: runtime.FidelityStrict}
	snapshot.Routes[routeSlug] = route
	if configure != nil {
		configure(snapshot)
	}
	var err error
	h.rt.release, err = runtime.NewRelease(release.ID, release.Sequence, snapshot, credentials)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestStrictUnknownUpstreamOutcomeNeverFailsOver(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(fmt.Sprintf("response_headers_%t", accepted), func(t *testing.T) {
			h := strictHarness(t, nil)
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if accepted {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, `{"id":`)
					_ = http.NewResponseController(w).Flush()
				}
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
			})
			resp, body := h.chat(fullKey, nil)
			if resp.StatusCode != 502 || errorCode(t, body) != "ambiguous_upstream_result" || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
				t.Fatalf("status=%d body=%v dispatches=%d/%d", resp.StatusCode, body, h.mock.count("a"), h.mock.count("b"))
			}
			envelope := h.sink.last(t)
			if len(envelope.Attempts) != 1 {
				t.Fatalf("attempts %+v", envelope.Attempts)
			}
			fact := envelope.Attempts[0]
			want := usage.UpstreamUnknown
			if accepted {
				want = usage.UpstreamAccepted
			}
			if fact.Interaction == nil || fact.Interaction.UpstreamState != want || fact.Interaction.ClientState != usage.ClientUnobserved || fact.Committed || !fact.BillingUncertain {
				t.Fatalf("incorrect acceptance/observation evidence %+v / %+v", fact, fact.Interaction)
			}
		})
	}
}

func TestStrictServerFailureLeavesUpstreamOutcomeUnknown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		harness func(*testing.T) *harness
		path    string
		body    string
	}{
		{"generation", func(t *testing.T) *harness { return strictHarness(t, nil) }, "/v1/chat/completions", `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}]}`},
		{"image generation", func(t *testing.T) *harness { return strictMediaHarness(t, media.OpImageGeneration, nil, nil) }, "/v1/images/generations", `{"model":"` + routeSlug + `","prompt":"photo"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.harness(t)
			h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"busy","type":"server_error"}}`))
			resp := h.do(t.Context(), http.MethodPost, tc.path, fullKey, []byte(tc.body), nil)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadGateway || !strings.Contains(string(raw), "ambiguous_upstream_result") || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
				t.Fatalf("status=%d body=%s dispatches=%d/%d", resp.StatusCode, raw, h.mock.count("a"), h.mock.count("b"))
			}
			fact := h.sink.last(t).Attempts[0]
			if fact.Class != classAmbiguous || fact.Status != http.StatusServiceUnavailable || fact.Interaction == nil || fact.Interaction.UpstreamState != usage.UpstreamUnknown || !fact.BillingUncertain {
				t.Fatalf("server failure evidence %+v / %+v", fact, fact.Interaction)
			}
		})
	}
}

func TestStrictSemanticEligibilityPrecedesPriorityAndPreservesNativeSource(t *testing.T) {
	h := strictHarness(t, func(snapshot *runtime.Snapshot) {
		for id, provider := range snapshot.Providers {
			if provider.Name == "a" {
				provider.Kind, provider.ProfileID = "anthropic", "anthropic-messages"
				snapshot.Providers[id] = provider
			}
		}
	})
	var captured map[string]json.RawMessage
	h.mock.set("b", func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Error(err)
		}
		completion(modelB, answerText)(w, r)
	})
	resp, body := h.chat(fullKey, nil, `,"messages":[{"role":"system","content":"system scope"},{"role":"developer","content":"developer scope"},{"role":"user","content":"question"}],"native_extension":{"large":9007199254740993,"nil":null,"empty":""}`)
	if resp.StatusCode != 200 || h.mock.count("a") != 0 || h.mock.count("b") != 1 {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if string(captured["native_extension"]) != `{"large":9007199254740993,"nil":null,"empty":""}` {
		t.Fatalf("native extension changed: %s", captured["native_extension"])
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(captured["messages"], &messages); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 3 || string(messages[0]["role"]) != `"system"` || string(messages[1]["role"]) != `"developer"` {
		t.Fatalf("instruction scopes changed: %s", captured["messages"])
	}
	fact := h.sink.last(t).Attempts[0]
	if fact.Interaction == nil || fact.Interaction.PlanClass != "native_identity" || fact.Interaction.UpstreamState != usage.UpstreamTerminal || fact.Interaction.ClientState != usage.ClientTerminal {
		t.Fatalf("interaction %+v", fact.Interaction)
	}
}

func TestStrictKnownRejectionCannotSubstituteServingEnvironment(t *testing.T) {
	h := strictHarness(t, nil)
	h.mock.set("a", status(http.StatusTooManyRequests, `{"error":{"message":"busy"}}`))
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != 429 || errorCode(t, body) != "upstream_rate_limit" || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
		t.Fatalf("serving substitution: %d %v", resp.StatusCode, body)
	}
	fact := h.sink.last(t).Attempts[0]
	if fact.Interaction == nil || fact.Interaction.UpstreamState != usage.UpstreamTerminal || fact.Interaction.ClientState != usage.ClientUnobserved || fact.BillingUncertain {
		t.Fatalf("rejection evidence %+v", fact)
	}
}

// Every slot of a provider revision observes the principal grant enrollment
// observed, so a strict request fails over to another slot of the provider
// and stays with that account; without a known principal the first admitted
// slot pins the request.
func TestStrictFailoverAcrossSlotsKeepsTheObservedPrincipal(t *testing.T) {
	for _, principal := range []string{"", "operator@example.com"} {
		t.Run("principal="+principal, func(t *testing.T) {
			h := strictHarness(t, func(snapshot *runtime.Snapshot) {
				for id, provider := range snapshot.Providers {
					if provider.Name == "a" {
						backup := provider.Slots[0]
						backup.ID, backup.Name, backup.Priority = uuid.NewString(), "backup", 1
						provider.Slots = append(provider.Slots, backup)
						provider.ObservedPrincipal = principal
						snapshot.Providers[id] = provider
					}
				}
			})
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				if h.mock.count("a") == 1 {
					status(http.StatusTooManyRequests, `{"error":{"message":"busy"}}`)(w, r)
					return
				}
				completion(modelA, answerText)(w, r)
			})
			resp, body := h.chat(fullKey, nil)
			attempts := h.sink.last(t).Attempts
			if principal == "" {
				if resp.StatusCode != http.StatusTooManyRequests || h.mock.count("a") != 1 || h.mock.count("b") != 0 || len(attempts) != 1 {
					t.Fatalf("an unknown principal did not pin the slot: %d %v attempts %+v", resp.StatusCode, body, attempts)
				}
				return
			}
			if resp.StatusCode != http.StatusOK || h.mock.count("a") != 2 || h.mock.count("b") != 0 || len(attempts) != 2 ||
				attempts[0].ProviderID != attempts[1].ProviderID || attempts[0].SlotID == attempts[1].SlotID {
				t.Fatalf("failover within the observed principal: %d %v attempts %+v", resp.StatusCode, body, attempts)
			}
		})
	}
}

func TestStrictRevokedCandidateDoesNotEstablishServingBaselineOrSpendBudget(t *testing.T) {
	credentialID := uuid.NewString()
	h := strictHarness(t, func(snapshot *runtime.Snapshot) {
		for id, provider := range snapshot.Providers {
			if provider.Name == "a" {
				provider.Slots[0].CredentialID = &credentialID
				snapshot.Providers[id] = provider
			}
		}
	})
	h.rt.revoked[credentialID] = true
	resp, body := h.chat(fullKey, map[string]string{routingHeader: `{"max_attempts":1}`})
	if resp.StatusCode != 200 || h.mock.count("a") != 0 || h.mock.count("b") != 1 {
		t.Fatalf("revocation spent budget: %d %v", resp.StatusCode, body)
	}
	if got := h.sink.last(t).Attempts; len(got) != 1 || got[0].Ordinal != 1 {
		t.Fatalf("attempts %+v", got)
	}
}

func TestStrictFailoverCannotChangeDefaultsThroughModelAlias(t *testing.T) {
	h := strictHarness(t, func(snapshot *runtime.Snapshot) {
		for id, provider := range snapshot.Providers {
			if provider.Name != "a" {
				continue
			}
			provider.Bindings = map[string]connectors.Binding{
				modelA:          {Model: "same-upstream", Defaults: map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage("1024")}}}},
				"smaller-alias": {Model: "same-upstream", Defaults: map[string]connectors.DefaultSet{"generation": {Dialect: "openai-chat", Values: map[string]json.RawMessage{"max_tokens": json.RawMessage("32")}}}},
			}
			provider.Capabilities = append(provider.Capabilities, runtime.Capability{Model: "smaller-alias", Operation: "generation", Surface: "openai", Mode: "unary"})
			snapshot.Providers[id] = provider
			route := snapshot.Routes[routeSlug]
			first := route.Targets[0]
			second := first
			second.ID, second.RoutingID, second.ProviderModel, second.Priority = uuid.NewString(), uuid.NewString(), "smaller-alias", 1
			route.Targets = []runtime.Target{first, second}
			snapshot.Routes[routeSlug] = route
		}
	})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if h.mock.count("a") == 1 {
			status(400, `{"error":{"code":"context_length_exceeded","message":"capacity refusal"}}`)(w, r)
			return
		}
		completion("same-upstream", answerText)(w, r)
	})
	resp, body := h.chat(fullKey, nil)
	if resp.StatusCode != 400 || errorCode(t, body) != "upstream_rejected" || h.mock.count("a") != 1 {
		t.Fatalf("strict defaults substituted: %d %v calls=%d", resp.StatusCode, body, h.mock.count("a"))
	}
}

type strictPartialWriter struct{ *unaryResponseWriter }

func (w *strictPartialWriter) Write(data []byte) (int, error) {
	count := len(data) / 2
	_, _ = w.ResponseRecorder.Write(data[:count])
	return count, io.ErrClosedPipe
}

func TestStrictPartialToolDeliveryRemainsPotentiallyActionable(t *testing.T) {
	h := strictHarness(t, nil)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"chunk\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"model-a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n")
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"team-chat","messages":[{"role":"user","content":"tool"}],"stream":true,"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`))
	req.Header.Set("Authorization", "Bearer "+fullKey)
	req.Header.Set("Content-Type", "application/json")
	writer := &strictPartialWriter{&unaryResponseWriter{ResponseRecorder: httptest.NewRecorder(), beforeDelivery: func() {}}}
	h.gateway.inference(openai.FamilyChat)(writer, req)
	envelope := h.sink.last(t)
	if len(envelope.Attempts) != 1 || h.mock.count("b") != 0 {
		t.Fatalf("partial tool delivery replayed: %+v", envelope)
	}
	fact := envelope.Attempts[0]
	if !fact.Committed || fact.Interaction == nil || fact.Interaction.ClientState != usage.ClientActionable || fact.Interaction.UpstreamState != usage.UpstreamAccepted {
		t.Fatalf("partial actionable evidence: %+v / %+v", fact, fact.Interaction)
	}
}
