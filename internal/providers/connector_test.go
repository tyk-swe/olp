package providers

import (
	"bytes"
	"context"
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
	"github.com/tyk-swe/olp/internal/egress"
)

var loopback = egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}

func probeCatalogue(policy egress.Policy) *Catalogue {
	return NewCatalogue(nil, &policy, nil, func(string) string { return "" }, CatalogueOptions{})
}

func upstreamConnection(kind, endpoint string) connection.Configuration {
	cfg := connection.Configuration{Identity: providerconfig.Identity[*string]{Kind: kind, AuthMode: connectors.AuthAPIKey, Endpoint: new(endpoint)}}
	cfg.Normalize()
	return cfg
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
	connection := upstreamConnection(connectors.KindOpenAI, upstream.URL)
	models, err := probeCatalogue(policy).listModelFacts(ctx, &connection, []byte("sk-test"))
	if err != nil {
		return classify(err).Code
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
		"oversized body":                {loopback, answer(http.StatusOK, string(bytes.Repeat([]byte(" "), probeBodyLimit+1))), "upstream_response_too_large"},
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

func listedModels(models []discoveredModel) []listedModel {
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
	connection := upstreamConnection(connectors.KindOpenAI, strings.Replace(upstream.URL, "127.0.0.1", "localhost", 1))
	_, err := probeCatalogue(egress.Policy{PlainHTTPHosts: []string{"localhost"}}).listModelFacts(t.Context(), &connection, []byte("sk-test"))
	if err == nil || classify(err).Code != "unsafe_destination" {
		t.Fatalf("listing error %v, want unsafe_destination", err)
	}
}

func TestModelDiscoveryFollowsPagesToTheEnd(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		kind, parameter string
		pages           map[string]string
		want            []listedModel
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
			connection := upstreamConnection(c.kind, upstream.URL)
			models, err := probeCatalogue(loopback).listModelFacts(t.Context(), &connection, []byte("sk-test"))
			if err != nil {
				t.Fatal(classify(err))
			}
			if got := listedModels(models); !slices.Equal(got, c.want) {
				t.Fatalf("listed %+v, want %+v", got, c.want)
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
			connection := upstreamConnection(connectors.KindAnthropic, upstream.URL)
			_, err := probeCatalogue(loopback).listModelFacts(t.Context(), &connection, []byte("sk-test"))
			if err == nil || classify(err).Code != "provider_protocol_error" {
				t.Fatalf("listing error %v, want provider_protocol_error", err)
			}
		})
	}
}
