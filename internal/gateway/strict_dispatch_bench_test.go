package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/runtime"
)

// This fixture uses only APIs available at the initial audit baseline, so the
// identical benchmark can qualify strict dispatch on both revisions.
func strictDispatchGateway(tb testing.TB, upstream *benchTransport) http.Handler {
	tb.Helper()
	credential, version, slot := uuid.NewString(), 1, uuid.NewString()
	provider := routing.Provider{
		ID: uuid.NewString(), Name: "bench", Kind: "openai_compatible", Enabled: true,
		ActiveCredential: &credential, RevisionID: uuid.NewString(),
		Endpoint: "http://127.0.0.1/bench/v1", AuthMode: "api_key",
		ProfileID: "compatible-chat", ProfileRevision: connectors.ProfileRevision,
		Slots: []routing.Slot{{ID: slot, Name: "default", Enabled: true, Weight: 1, CredentialID: &credential, CredentialVersion: &version}},
	}
	for _, mode := range []string{"unary", "streaming"} {
		provider.Capabilities = append(provider.Capabilities, routing.Capability{Model: "gpt-bench", Operation: "generation", Surface: "openai", Mode: mode})
	}
	routeID, targetID := uuid.NewString(), uuid.NewString()
	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()},
		Providers:  map[string]routing.Provider{provider.ID: provider},
		Routes: map[string]routing.Route{routeSlug: {
			ID: routeID, Slug: routeSlug, Operations: []string{"generation"}, OverallTimeout: 5000, MaxAttempts: 1,
			RoutingID: routeID, RevisionID: uuid.NewString(), Revision: 1, PublishedAt: time.Now(),
			Fidelity: routing.RouteFidelity{Mode: routing.FidelityStrict},
			Targets:  []routing.Target{{ID: targetID, ProviderID: provider.ID, ProviderModel: "gpt-bench", Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()}},
		}},
	}
	release, err := runtime.NewRelease(uuid.NewString(), 7, snapshot, map[string][]byte{credential: []byte(secretA)})
	if err != nil {
		tb.Fatal(err)
	}
	if _, ok := release.Snapshot.InteractionTemplate(routeSlug, targetID); !ok {
		tb.Fatal("strict benchmark has no compiled interaction template")
	}
	key := access.Authority{ID: uuid.NewString(), LookupID: "lookup_bench", Policy: access.KeyPolicy{Scopes: []string{"inference"}}}
	rt := &fakeRuntime{release: release, revoked: map[string]bool{}, keys: map[string]access.Authority{fullKey: key}}
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	gw := New(rt, &policy, Config{MaxInFlight: 8, MaxBodyBytes: 2 << 20, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096}, quiet)
	gw.Sink = discardSink{}
	// Explicit profiles use the scoped connection cache, so choose the same
	// client dispatch will use before replacing this fixture's transport.
	client, err := gw.providerClient(tb.Context(), release, &provider, provider.Slots[0])
	if err != nil {
		tb.Fatal(err)
	}
	transport := client.Transport
	client.Transport = upstream
	tb.Cleanup(func() {
		// Cache retirement owns its original concrete transport.
		client.Transport = transport
		gw.connections.CloseIdleConnections()
		gw.client.CloseIdleConnections()
	})
	mux := http.NewServeMux()
	gw.Register(mux)
	return mux
}

// BenchmarkStrictDispatch includes real preparation and attempt dispatch, with
// native completion validation and delivery, while avoiding network latency.
func BenchmarkStrictDispatch(b *testing.B) {
	for _, size := range []struct {
		name  string
		bytes int
	}{{"small", 64}, {"1MiB", 1 << 20}} {
		for _, streamed := range []bool{false, true} {
			mode := "unary"
			if streamed {
				mode = "stream"
			}
			b.Run(size.name+"/"+mode, func(b *testing.B) {
				payload := map[string]any{
					"model": routeSlug, "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("a", size.bytes)}},
					"max_tokens": 128,
				}
				upstream := benchTransport{status: http.StatusOK, body: []byte(benchChat), header: http.Header{"Content-Type": {"application/json"}}}
				want := `"object":"chat.completion"`
				if streamed {
					payload["stream"], payload["stream_options"] = true, map[string]any{"include_usage": true}
					upstream.body, upstream.header = benchChatStream(), http.Header{"Content-Type": {"text/event-stream"}}
					want = "[DONE]"
				}
				requestBytes, err := json.Marshal(payload)
				if err != nil {
					b.Fatal(err)
				}
				handler := strictDispatchGateway(b, &upstream)
				body := bytes.NewReader(requestBytes)
				request := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "/v1/chat/completions", body)
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Authorization", "Bearer "+fullKey)
				request.ContentLength = int64(len(requestBytes))
				writer := &benchWriter{header: make(http.Header, 16)}
				serve := func() {
					body.Reset(requestBytes)
					request.Body = io.NopCloser(body)
					writer.reset()
					handler.ServeHTTP(writer, request)
				}
				writer.kept = &bytes.Buffer{}
				serve()
				if writer.status != http.StatusOK || !strings.Contains(writer.kept.String(), want) {
					b.Fatalf("status %d, wrote %q, want it to contain %q", writer.status, writer.kept, want)
				}
				writer.kept = nil
				b.SetBytes(int64(len(requestBytes)))
				b.ReportAllocs()
				for b.Loop() {
					serve()
				}
				b.ReportMetric(float64(writer.flushed), "flushes/op")
			})
		}
	}
}
