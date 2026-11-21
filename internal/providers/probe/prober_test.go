package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/providers/connection"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/platform/egress"
)

var loopback = egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}

func prober(policy egress.Policy) *Prober {
	return New(&policy, func(string) string { return "" }, nil, nil)
}

func upstreamConnection(t *testing.T, kind connectors.Kind, endpoint string) Target {
	t.Helper()
	stored, err := json.Marshal(connection.Configuration{Identity: providerconfig.Identity[*string]{Kind: kind, AuthMode: connectors.AuthAPIKey, Endpoint: new(endpoint)}})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := connection.Load(stored, nil)
	if err != nil {
		t.Fatal(err)
	}
	return Target{Connection: prepared, Credential: []byte("sk-test")}
}

func answer(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func probeCode(t *testing.T, ctx context.Context, policy egress.Policy, handler http.Handler) string {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	connection := upstreamConnection(t, connectors.KindOpenAI, upstream.URL)
	models, err := prober(policy).ListModelFacts(ctx, connection)
	if err != nil {
		return string(Classify(err).Reason)
	}
	t.Fatalf("probe succeeded with %d models", len(models))
	return ""
}

func TestModelProbeRecordsUpstreamFailuresAsProbeCodes(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		policy  egress.Policy
		handler http.HandlerFunc
		want    string
	}{
		"rejected credential":           {loopback, answer(http.StatusUnauthorized, `{"error":{"message":"bad key"}}`), "upstream_authentication_failed"},
		"denied access":                 {loopback, answer(http.StatusForbidden, `{"error":{"message":"denied"}}`), "upstream_permission_denied"},
		"missing listing":               {loopback, answer(http.StatusNotFound, ""), "upstream_not_found"},
		"rate limited":                  {loopback, answer(http.StatusTooManyRequests, ""), "upstream_rate_limit"},
		"server failure":                {loopback, answer(http.StatusInternalServerError, ""), "upstream_unavailable"},
		"unavailable":                   {loopback, answer(http.StatusServiceUnavailable, ""), "upstream_unavailable"},
		"other rejection":               {loopback, answer(http.StatusBadRequest, ""), "upstream_rejected"},
		"listing without models":        {loopback, answer(http.StatusOK, `{}`), "provider_protocol_error"},
		"oversized body":                {loopback, answer(http.StatusOK, string(bytes.Repeat([]byte(" "), bodyLimit+1))), "upstream_response_too_large"},
		"redirect":                      {loopback, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/v2/models", http.StatusFound) }, "upstream_redirect"},
		"plain http outside the policy": {egress.Policy{}, answer(http.StatusOK, `{"data":[]}`), "invalid_endpoint"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := probeCode(t, t.Context(), c.policy, c.handler); got != c.want {
				t.Fatalf("probe code %s, want %s", got, c.want)
			}
		})
	}
}

func TestModelProbeRecordsAnUnansweredUpstreamAsTimeout(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	t.Cleanup(cancel)
	unanswered := func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	if got := probeCode(t, ctx, loopback, http.HandlerFunc(unanswered)); got != "gateway_timeout" {
		t.Fatalf("probe code %s, want gateway_timeout", got)
	}
}

type listedModel struct{ Name, Display string }

func listedModels(models []DiscoveredModel) []listedModel {
	listed := make([]listedModel, len(models))
	for i, model := range models {
		listed[i] = listedModel{Name: model.Name, Display: model.Display}
	}
	return listed
}

func TestModelProbeRefusesAHostThatResolvesToABlockedAddress(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(answer(http.StatusOK, `{"data":[]}`))
	t.Cleanup(upstream.Close)
	connection := upstreamConnection(t, connectors.KindOpenAI, strings.Replace(upstream.URL, "127.0.0.1", "localhost", 1))
	_, err := prober(egress.Policy{PlainHTTPHosts: []string{"localhost"}}).ListModelFacts(t.Context(), connection)
	if err == nil || Classify(err).Reason != "unsafe_destination" {
		t.Fatalf("listing error %v, want unsafe_destination", err)
	}
}

func TestCompatibleModelDiscoveryPreservesExactIDs(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(answer(http.StatusOK, `{"data":[{"id":"chat-local"},{"id":"models/chat-local"},{"id":"models/chat-local"}]}`))
	t.Cleanup(upstream.Close)
	connection := upstreamConnection(t, connectors.KindOpenAICompatible, upstream.URL)
	models, err := prober(loopback).ListModelFacts(t.Context(), connection)
	if err != nil {
		t.Fatal(Classify(err))
	}
	want := []listedModel{{Name: "chat-local", Display: "chat-local"}, {Name: "models/chat-local", Display: "models/chat-local"}}
	if got := listedModels(models); !slices.Equal(got, want) {
		t.Fatalf("listed %+v, want %+v", got, want)
	}
}

func TestModelDiscoveryFollowsPagesToTheEnd(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		kind      connectors.Kind
		parameter string
		pages     map[string]string
		want      []listedModel
	}{
		"anthropic has_more and last_id": {connectors.KindAnthropic, "after_id", map[string]string{
			"":         `{"data":[{"id":"claude-a","display_name":"Claude A"},{"id":"claude-b"}],"has_more":true,"last_id":"claude-b"}`,
			"claude-b": `{"data":[{"id":"claude-b"},{"id":"claude-c","display_name":"Claude C"}],"has_more":false,"last_id":"claude-c"}`,
		}, []listedModel{{Name: "claude-a", Display: "Claude A"}, {Name: "claude-b", Display: "claude-b"}, {Name: "claude-c", Display: "Claude C"}}},
		"gemini nextPageToken": {connectors.KindGemini, "pageToken", map[string]string{
			"":       `{"models":[{"name":"models/gemini-a","displayName":"Gemini A"}],"nextPageToken":"page-2"}`,
			"page-2": `{"models":[{"name":"models/gemini-b","displayName":"Gemini B"}]}`,
		}, []listedModel{{Name: "gemini-a", Display: "Gemini A"}, {Name: "gemini-b", Display: "Gemini B"}}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page, ok := c.pages[r.URL.Query().Get(c.parameter)]
				if !ok || r.URL.Path != "/models" {
					http.NotFound(w, r)
					return
				}
				_, _ = w.Write([]byte(page))
			}))
			t.Cleanup(upstream.Close)
			connection := upstreamConnection(t, c.kind, upstream.URL)
			models, err := prober(loopback).ListModelFacts(t.Context(), connection)
			if err != nil {
				t.Fatal(Classify(err))
			}
			if got := listedModels(models); !slices.Equal(got, c.want) {
				t.Fatalf("listed %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestCompleteModelDiscoveryUsesOneCredentialAcrossPages(t *testing.T) {
	t.Parallel()
	var keys, versions, afterIDs []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("X-Api-Key"))
		versions = append(versions, r.Header.Get("Anthropic-Version"))
		afterID := r.URL.Query().Get("after_id")
		afterIDs = append(afterIDs, afterID)
		switch afterID {
		case "":
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-kept","display_name":"Claude Kept"}],"has_more":true,"last_id":"claude-kept"}`))
		case "claude-kept":
			_, _ = w.Write([]byte(`{"data":[{"id":"claude-new","display_name":"Claude New"}],"has_more":false,"last_id":"claude-new"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	connection := upstreamConnection(t, connectors.KindAnthropic, upstream.URL)
	listing, err := prober(loopback).ListCompleteModelFacts(t.Context(), connection)
	if err != nil {
		t.Fatal(Classify(err))
	}
	if !listing.Complete {
		t.Fatalf("listing incomplete: %s", listing.Detail)
	}
	want := []listedModel{{Name: "claude-kept", Display: "Claude Kept"}, {Name: "claude-new", Display: "Claude New"}}
	if got := listedModels(listing.Models); !slices.Equal(got, want) {
		t.Fatalf("listed %+v, want %+v", got, want)
	}
	if len(keys) != 2 || keys[0] != "sk-test" || keys[1] != keys[0] {
		t.Fatalf("page credentials %v, want the same published credential on both pages", keys)
	}
	if len(versions) != 2 || versions[0] != versions[1] {
		t.Fatalf("page API versions %v, want one listing configuration on both pages", versions)
	}
	if !slices.Equal(afterIDs, []string{"", "claude-kept"}) {
		t.Fatalf("after_id values %v, want the first page cursor followed", afterIDs)
	}
}

func TestCompleteModelDiscoveryNeverClaimsCompletenessWithoutProof(t *testing.T) {
	t.Parallel()
	truncated := func() string {
		items := make([]string, 2001)
		for i := range items {
			items[i] = fmt.Sprintf(`{"id":"model-%04d"}`, i)
		}
		return `{"data":[` + strings.Join(items, ",") + `],"has_more":false,"last_id":"model-2000"}`
	}()
	cases := map[string]struct {
		handler http.HandlerFunc
		wantErr bool
	}{
		"missing has_more":          {answer(http.StatusOK, `{"data":[{"id":"claude-a"}]}`), false},
		"malformed has_more":        {answer(http.StatusOK, `{"data":[{"id":"claude-a"}],"has_more":"false"}`), false},
		"has_more without a cursor": {answer(http.StatusOK, `{"data":[{"id":"claude-a"}],"has_more":true}`), true},
		"repeated cursor":           {answer(http.StatusOK, `{"data":[{"id":"claude-a"}],"has_more":true,"last_id":"claude-a"}`), true},
		"model without an id":       {answer(http.StatusOK, `{"data":[{"display_name":"No ID"}],"has_more":false}`), true},
		"truncated at the bound":    {answer(http.StatusOK, truncated), false},
		"withheld final page": {func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("after_id") == "" {
				_, _ = w.Write([]byte(`{"data":[{"id":"claude-a"}],"has_more":true,"last_id":"claude-a"}`))
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		}, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(c.handler)
			t.Cleanup(upstream.Close)
			connection := upstreamConnection(t, connectors.KindAnthropic, upstream.URL)
			listing, err := prober(loopback).ListCompleteModelFacts(t.Context(), connection)
			if listing.Complete {
				t.Fatalf("listing claimed completeness from %s", name)
			}
			if c.wantErr && err == nil {
				t.Fatalf("listing from %s succeeded, want a failure", name)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("listing from %s failed with %v, want an incomplete result", name, Classify(err))
			}
		})
	}
}

func TestModelDiscoveryRefusesAContinuationItCannotFollow(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"has_more without a cursor": `{"data":[{"id":"claude-a"}],"has_more":true}`,
		"repeated cursor":           `{"data":[{"id":"claude-a"}],"has_more":true,"last_id":"claude-a"}`,
	}
	for name, page := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(answer(http.StatusOK, page))
			t.Cleanup(upstream.Close)
			connection := upstreamConnection(t, connectors.KindAnthropic, upstream.URL)
			_, err := prober(loopback).ListModelFacts(t.Context(), connection)
			if err == nil || Classify(err).Reason != "provider_protocol_error" {
				t.Fatalf("listing error %v, want provider_protocol_error", err)
			}
		})
	}
}
