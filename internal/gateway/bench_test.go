package gateway

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/usage"
)

// allowing is a limiter client that grants every reservation and keeps no state,
// so that what a request costs the gateway can be measured without a Valkey: the
// commands a limiter builds, the replies it parses and the leases it keeps are
// the production ones, and the answers are fixed, so nothing grows with the number
// of operations and a request limit never runs out. It tells the commands apart
// by their size: the rate reservation takes twelve arguments, the cost
// reservation thirteen, its settlement eight, a refund nine, a reconciliation
// seven and a release five. calls counts the commands it was sent.
type allowing struct {
	calls atomic.Int64
	rate  []any
}

const (
	allowingWindow = int64(29_823_060)
	allowingReset  = int64(37_500)
)

// newAllowing answers rate reservations as the script answers a request that is
// the first of its minute and takes tokens of a limit of requests and tokens per
// minute (zero for no limit), with a concurrency lease when concurrent says so.
func newAllowing(requests, tokens, taken int64, concurrent bool) *allowing {
	var expiry int64
	if concurrent {
		expiry = 1_789_383_605_000
	}
	reply := []any{int64(2), int64(1), "ok", int64(0), allowingWindow, expiry}
	if requests > 0 || tokens > 0 {
		var requestsLeft, tokensLeft int64
		if requests > 0 {
			requestsLeft = requests - 1
		}
		if tokens > 0 {
			tokensLeft = tokens - taken
		}
		reply = append(reply, requests, requestsLeft, tokens, tokensLeft, allowingReset)
	}
	return &allowing{rate: reply}
}

var (
	costAllowed = []any{int64(1), int64(1), "ok", int64(0), int64(20_000), int64(24_000)}
	costSettled = []any{int64(1), int64(1), "ok", int64(1), int64(0)}
	released    = []any{int64(1)}
)

func (c *allowing) Do(_ context.Context, args ...string) (any, error) {
	c.calls.Add(1)
	switch len(args) {
	case 12:
		return c.rate, nil
	case 13:
		return costAllowed, nil
	case 8:
		return costSettled, nil
	}
	return released, nil
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func newAdmission(tb testing.TB, client limits.Commander) *Admission {
	tb.Helper()
	limiter, err := limits.New(client, "olp:bench")
	if err != nil {
		tb.Fatal(err)
	}
	return NewAdmission(limiter, nil, quiet)
}

const benchOwner = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"

// benchEstimate is the tokens a request reserves: a prompt of a few thousand.
const benchEstimate = int64(4_200)

func int64ptr(v int64) *int64    { return &v }
func stringptr(v string) *string { return &v }

// BenchmarkAdmission is what every request of a limited key pays at the
// reservation: admission before the first attempt and settlement after the last,
// against a limiter whose answers are fixed. A key that limits nothing is the
// floor, which touches no client and allocates nothing.
//
//	go test ./internal/gateway -run '^$' -bench BenchmarkAdmission -benchmem
func BenchmarkAdmission(b *testing.B) {
	group := uuid.NewString()
	actual := benchEstimate - 200
	for _, tc := range []struct {
		name      string
		authority access.Authority
		hold      costReservation
		// requests and tokens are the limits the limiter answers for, and
		// concurrent whether it answers with a concurrency lease.
		requests, tokens int64
		concurrent       bool
		calls            int64 // commands one operation sends
	}{
		{
			name:      "unlimited",
			authority: access.Authority{ID: benchOwner, LookupID: "lookup_bench"},
			calls:     0,
		},
		{
			name:      "requests-and-tokens",
			authority: access.Authority{ID: benchOwner, LookupID: "lookup_bench", Policy: access.KeyPolicy{RequestsPerMinute: int64ptr(600), TokensPerMinute: int64ptr(1_000_000)}},
			requests:  600, tokens: 1_000_000, calls: 2, // reserve, reconcile
		},
		{
			name:      "requests-tokens-and-concurrency",
			authority: access.Authority{ID: benchOwner, LookupID: "lookup_bench", Policy: access.KeyPolicy{RequestsPerMinute: int64ptr(600), TokensPerMinute: int64ptr(1_000_000), MaxConcurrency: int64ptr(64)}},
			requests:  600, tokens: 1_000_000, concurrent: true, calls: 3, // reserve, reconcile, release
		},
		{
			name:      "cost-budget",
			authority: access.Authority{ID: benchOwner, LookupID: "lookup_bench", Policy: access.KeyPolicy{DailyCostLimit: stringptr("25")}},
			hold:      costReservation{amount: "0.0136", requestID: uuid.NewString()},
			calls:     2, // reserve cost, settle cost
		},
		{
			name: "budget-group",
			authority: access.Authority{ID: benchOwner, LookupID: "lookup_bench", BudgetGroupID: &group, BudgetGroupMonthlyCostLimit: stringptr("500"),
				Policy: access.KeyPolicy{RequestsPerMinute: int64ptr(600)}},
			hold:     costReservation{amount: "0.0136", requestID: uuid.NewString()},
			requests: 600, calls: 3, // reserve the group's cost, reserve the key, settle the group's cost
		},
	} {
		b.Run(tc.name, func(b *testing.B) {
			client := newAllowing(tc.requests, tc.tokens, benchEstimate, tc.concurrent)
			admission := newAdmission(b, client)
			ctx := context.Background()
			var operations int64
			b.ReportAllocs()
			for b.Loop() {
				lease, e := admission.reserveKeyCosted(ctx, tc.authority, "openai", benchEstimate, 30*time.Second, tc.hold)
				if e != nil {
					b.Fatal(e)
				}
				if tc.hold.amount != "" {
					lease.SetActualCost("0.0129")
				}
				settleKey(ctx, lease, true, &actual, quiet)
				operations++
			}
			b.StopTimer()
			if got := client.calls.Load(); got != operations*tc.calls {
				b.Fatalf("%d operations sent %d commands, want %d each: the case no longer measures what it names", operations, got, tc.calls)
			}
		})
	}
	b.Run("provider-quota", func(b *testing.B) {
		client := newAllowing(0, 0, 0, true)
		admission := newAdmission(b, client)
		provider := &runtime.Provider{ID: benchOwner, Limits: &runtime.Limits{MaxConcurrency: int64ptr(32)}}
		slot := &runtime.Slot{ID: uuid.NewString(), MaxConcurrency: int64ptr(8)}
		ctx := context.Background()
		var operations int64
		b.ReportAllocs()
		for b.Loop() {
			reservation, rejection, skip := admission.reserveTarget(ctx, provider, slot, benchEstimate, 30*time.Second, "")
			if reservation == nil || rejection != nil || skip {
				b.Fatalf("the target was not admitted: %v %v %v", reservation, rejection, skip)
			}
			reservation.settle(ctx, true, nil)
			operations++
		}
		b.StopTimer()
		// A reservation for the provider and one for the slot, and a release of each.
		if got := client.calls.Load(); got != operations*4 {
			b.Fatalf("%d operations sent %d commands, want 4 each", operations, got)
		}
	})
}

// TestUnconfiguredFeaturesAddNoAllocations proves the budget of a feature that is
// off: a request whose key, budget group and target limit nothing reaches the
// limiter with no command and no allocation, whether the gateway has one or not,
// and a request that does limit something does both, so that the zero is not an
// admission that never runs.
//
// It counts allocations, which is global to the process, so it does not run in
// parallel with the tests that allocate.
func TestUnconfiguredFeaturesAddNoAllocations(t *testing.T) {
	client := newAllowing(600, 1_000_000, benchEstimate, true)
	admission := newAdmission(t, client)
	free := access.Authority{ID: benchOwner, LookupID: "lookup_bench", Policy: access.KeyPolicy{Scopes: []string{"inference"}}}
	provider := &runtime.Provider{ID: benchOwner}
	slot := &runtime.Slot{ID: uuid.NewString()}
	ctx := context.Background()
	server := &Server{Admission: admission, log: quiet}
	// A request that has a price list to be priced by, so that the guard is the only
	// reason a key without a cost budget has nothing estimated for it: an execution
	// with no attempts would spare the estimate whether the guard held or not.
	priced := costExecution(t, `{"model":"team-chat","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, openai.FamilyChat, 1,
		runtime.Attempt{Price: costPrice()})
	priced.request.id, priced.request.minted = uuid.NewString(), true

	headers := http.Header{"Content-Type": []string{"application/json"}}
	runs := map[string]func(){
		"absent caller credential": func() {
			if readCallerCredential(headers) != nil {
				t.Fatal("invented caller credentials")
			}
		},
		"route without a body limit": func() {
			if failure := priced.checkBody(priced.route); failure != nil {
				t.Fatal(failure)
			}
		},
		"key without network restrictions": func() {
			if failure := server.checkKeyAddress(nil, free); failure != nil {
				t.Fatal(failure)
			}
		},
		"key admission": func() {
			lease, e := admission.reserveKey(ctx, free, "openai", benchEstimate, time.Minute)
			if lease != nil || e != nil {
				t.Fatalf("a key that limits nothing was given %v, %v", lease, e)
			}
			settleKey(ctx, lease, true, nil, quiet)
		},
		"key admission with no limiter": func() {
			var none *Admission
			if lease, e := none.reserveKey(ctx, free, "openai", benchEstimate, time.Minute); lease != nil || e != nil {
				t.Fatalf("a key that limits nothing was given %v, %v", lease, e)
			}
		},
		"cost reservation of a key with no cost budget": func() {
			if hold := server.costReservation(priced, free); hold != (costReservation{}) {
				t.Fatalf("a key with no cost budget reserved %+v", hold)
			}
		},
		"settlement of a request that reserved nothing": func() {
			server.settleAdmission(ctx, &execution{dispatched: true})
		},
		"target admission": func() {
			reservation, rejection, skip := admission.reserveTarget(ctx, provider, slot, benchEstimate, time.Minute, "")
			if reservation != nil || rejection != nil || skip {
				t.Fatalf("a target that limits nothing was given %v, %v, %v", reservation, rejection, skip)
			}
			reservation.settle(ctx, true, nil)
		},
	}
	// An Anthropic request that sent no Anthropic-Beta has nothing to check or to
	// forward, whichever provider it goes to: the lookup of what a provider
	// declares is for a request that has a header to place.
	for _, target := range []struct {
		name          string
		kind, profile string
	}{
		{"an automatic Anthropic provider", "anthropic", ""},
		{"an automatic OpenAI provider", "openai", ""},
		{"the Anthropic Messages profile", "anthropic", "anthropic-messages"},
		{"Claude on Vertex AI", "vertex_ai", "vertex-anthropic"},
	} {
		provider := runtime.Provider{ID: uuid.NewString(), Kind: target.kind, ProfileID: target.profile}
		if target.profile != "" {
			provider.ProfileRevision = connectors.ProfileRevision
		}
		cfg := provider.Connector()
		sending := func(header http.Header) *execution {
			header.Set("Anthropic-Version", "2023-06-01")
			return &execution{
				parsed: &openai.Request{Family: openai.FamilyAnthropic}, semanticHeaders: semanticHeaders(header),
				route:              &runtime.Route{Fidelity: runtime.RouteFidelity{Mode: runtime.FidelityTransformed}, Targets: []runtime.Target{{ProviderID: provider.ID}}},
				historicalSnapshot: &runtime.Snapshot{Providers: map[string]runtime.Provider{provider.ID: provider}},
			}
		}
		x, header := sending(http.Header{}), http.Header{}
		runs["anthropic-beta check, "+target.name] = func() {
			if e := x.checkAnthropicBeta(); e != nil {
				t.Fatalf("a request with no Anthropic-Beta was refused: %v", e)
			}
		}
		runs["anthropic-beta forwarding, "+target.name] = func() {
			if forwardAnthropicBeta(header, x, openai.FamilyAnthropic, cfg).SemanticHeaders != nil || len(header) != 0 {
				t.Fatalf("a request with no Anthropic-Beta was sent one: %v", header)
			}
		}
		// A request that did send one looks up what every target of its route
		// declares, and a lookup that copied the profile of each would allocate for
		// every request that sends a beta, whichever header it sent.
		sent := sending(http.Header{"Anthropic-Beta": {"context-management-2025-06-27"}})
		runs["anthropic-beta check of a header, "+target.name] = func() {
			if e := sent.checkAnthropicBeta(); e != nil {
				t.Fatalf("a request with a valid Anthropic-Beta was refused: %v", e)
			}
		}
		// The zero is the check's, and not that of one that never runs: a header it
		// cannot forward is refused wherever the provider takes it.
		tooLong := sending(http.Header{"Anthropic-Beta": {strings.Repeat("a", maxAnthropicBeta+1)}})
		if refused := tooLong.checkAnthropicBeta() != nil; refused != takesAnthropicBeta(cfg) {
			t.Errorf("%s: a header that is too long was refused: %v, the provider takes it: %v", target.name, refused, takesAnthropicBeta(cfg))
		}
	}

	for name, run := range runs {
		before := client.calls.Load()
		if got := testing.AllocsPerRun(200, run); got != 0 {
			t.Errorf("%s: %v allocations, want none", name, got)
		}
		if got := client.calls.Load() - before; got != 0 {
			t.Errorf("%s: sent %d limiter commands, want none", name, got)
		}
	}

	limited := free
	limited.Policy.RequestsPerMinute = int64ptr(600)
	client = newAllowing(600, 0, 0, false)
	admission = newAdmission(t, client)
	before := client.calls.Load()
	reserve := func() {
		lease, e := admission.reserveKey(ctx, limited, "openai", benchEstimate, time.Minute)
		if lease == nil || e != nil {
			t.Fatalf("a key limited to 600 requests a minute was given %v, %v", lease, e)
		}
	}
	if got := testing.AllocsPerRun(10, reserve); got == 0 {
		t.Error("a limited key allocates nothing; the zeros above prove nothing")
	}
	if got := client.calls.Load() - before; got != 11 {
		t.Errorf("a limited key sent %d commands for 11 reservations, want one each", got)
	}

	// The request that was not estimated for a key without a cost budget is priced
	// the moment one applies, and the estimate allocates, so the zero of the guard is
	// the guard's and not that of a request nothing could price.
	budgetedKey := free
	budgetedKey.Policy.DailyCostLimit = stringptr("25")
	if hold := server.costReservation(priced, budgetedKey); hold.amount != "0.000162" {
		t.Errorf("a key with a daily cost limit reserves %q, want 0.000162", hold.amount)
	}
	if got := testing.AllocsPerRun(10, func() { server.costReservation(priced, budgetedKey) }); got == 0 {
		t.Error("a key with a cost budget estimates nothing; the zero above proves nothing")
	}

	// A target with a quota is named for it, which the limiter refuses to do
	// without: one that was left unnamed would be skipped as unenforceable.
	capped := &runtime.Provider{ID: benchOwner, Limits: &runtime.Limits{MaxConcurrency: int64ptr(32)}}
	client = newAllowing(0, 0, 0, true)
	admission = newAdmission(t, client)
	reservation, rejection, skip := admission.reserveTarget(ctx, capped, slot, benchEstimate, time.Minute, "")
	if reservation == nil || rejection != nil || skip || client.calls.Load() != 1 {
		t.Errorf("a target limited to 32 concurrent requests was given %v, %v, skip %v after %d commands, want one reservation", reservation, rejection, skip, client.calls.Load())
	}
}

// benchTransport is the upstream of the gateway benchmarks: every request is
// answered with the same bytes from memory, as a provider that responds at once
// would, with no socket to wait on and nothing held between requests.
type benchTransport struct {
	status int
	header http.Header
	body   []byte
	reader memoryBody
}

// memoryBody is a response body that is read from memory and closed for nothing.
type memoryBody struct{ bytes.Reader }

func (*memoryBody) Close() error { return nil }

func (t *benchTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Body != nil {
		io.Copy(io.Discard, request.Body)
		request.Body.Close()
	}
	t.reader.Reset(t.body)
	return &http.Response{StatusCode: t.status, Header: t.header, Body: &t.reader, ContentLength: int64(len(t.body)), Request: request}, nil
}

// benchWriter is the client of the gateway benchmarks: a response writer that
// discards what it is given, so that nothing grows with the number of operations,
// and takes the deadlines and flushes a connection does.
type benchWriter struct {
	header  http.Header
	status  int
	written int
	flushed int
	// kept, when set, also receives what is written.
	kept *bytes.Buffer
}

func (w *benchWriter) Header() http.Header { return w.header }
func (w *benchWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *benchWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	w.written += len(p)
	if w.kept != nil {
		w.kept.Write(p)
	}
	return len(p), nil
}
func (w *benchWriter) Flush()                           { w.flushed++ }
func (w *benchWriter) SetReadDeadline(time.Time) error  { return nil }
func (w *benchWriter) SetWriteDeadline(time.Time) error { return nil }
func (w *benchWriter) reset() {
	clear(w.header)
	w.status, w.written, w.flushed = 0, 0, 0
}

// discardSink drops the terminal event, which a gateway hands to accounting.
type discardSink struct{}

func (discardSink) Terminal(Envelope) {}

// benchLimits is what a benchmark gateway enforces beyond the defaults, which are
// a body of 64 KiB and a key that limits nothing. A budget gives the key a daily
// cost limit that the limiter always has room for, and prices the model, so that
// admission estimates what the request could cost and reserves it, as it does for
// the key of the high-throughput scenario.
type benchLimits struct {
	maxBody int64
	budget  bool
}

// benchGateway is a gateway that serves one route through one provider, over
// the transport, to a key that limits what the limits say.
func benchGateway(tb testing.TB, upstream *benchTransport, model string, limit benchLimits) http.Handler {
	tb.Helper()
	credential, version, slot := uuid.NewString(), 1, uuid.NewString()
	provider := runtime.Provider{
		ID: uuid.NewString(), Name: "bench", Kind: "openai", Enabled: true, ActiveCredential: &credential, RevisionID: uuid.NewString(),
		Endpoint: "http://127.0.0.1/bench/v1", AuthMode: "api_key",
		Slots: []runtime.Slot{{ID: slot, Name: "default", Enabled: true, Weight: 1, CredentialID: &credential, CredentialVersion: &version}},
	}
	for _, surface := range []string{"openai", "anthropic"} {
		for _, mode := range []string{"unary", "streaming"} {
			provider.Capabilities = append(provider.Capabilities, runtime.Capability{Model: model, Operation: "generation", Surface: surface, Mode: mode})
		}
	}
	routeID := uuid.NewString()
	snapshot := &runtime.Snapshot{
		Generation: runtime.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()},
		Providers:  map[string]runtime.Provider{provider.ID: provider},
		Routes: map[string]runtime.Route{routeSlug: {
			ID: routeID, Slug: routeSlug, Operations: []string{"generation"}, OverallTimeout: 5000, MaxAttempts: 3, RoutingID: routeID, RevisionID: uuid.NewString(), Revision: 1, PublishedAt: time.Now(),
			Fidelity: runtime.RouteFidelity{Mode: runtime.FidelityTransformed},
			Targets:  []runtime.Target{{ID: uuid.NewString(), ProviderID: provider.ID, ProviderModel: model, Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()}},
		}},
	}
	release, err := runtime.NewRelease(uuid.NewString(), 7, snapshot, map[string][]byte{credential: []byte(secretA)})
	if err != nil {
		tb.Fatal(err)
	}
	key := access.Authority{ID: uuid.NewString(), LookupID: "lookup_bench", Policy: access.KeyPolicy{Scopes: []string{"inference"}}}
	rt := &fakeRuntime{release: release, revoked: map[string]bool{}, keys: map[string]access.Authority{fullKey: key}}
	maxBody := cmp.Or(limit.maxBody, 64*1024)
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	gw := New(rt, &policy, Config{MaxInFlight: 8, MaxBodyBytes: maxBody, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096}, quiet)
	if limit.budget {
		key.Policy.DailyCostLimit = stringptr("1000000")
		rt.keys[fullKey] = key
		input, output := "2.5", "10"
		rt.inputs = &usage.RoutingInputs{RefreshedAt: time.Now(), Prices: []usage.RoutingPrice{{
			Price:       usage.Price{ProviderKind: "openai", ProviderID: &provider.ID, Model: model, Operation: "generation", InputPerMillion: &input, OutputPerMillion: &output, Currency: "USD"},
			EffectiveAt: time.Now().Add(-time.Hour),
		}}}
		gw.Admission = newAdmission(tb, newAllowing(0, 0, 0, false))
	}
	gw.Sink = discardSink{}
	gw.client = &http.Client{Transport: upstream}
	mux := http.NewServeMux()
	gw.Register(mux)
	return mux
}

// BenchmarkStreamWriter is what the gateway adds to each frame of a stream on its
// way to the client once the response is committed: the write deadline renewed,
// and the frame written and flushed. The first frame, which commits the response,
// is sent before the measurement and only proves the writer is the one that
// streams. That commit happens once a stream, BenchmarkGateway pays it in each of
// its stream cases, and the allocations of the response headers it adds are
// bounded by response_headers_test.go.
func BenchmarkStreamWriter(b *testing.B) {
	frame := []byte("data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1800000000,\"model\":\"team-chat\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"word 17 \"},\"finish_reason\":null}]}\n\n")
	writer := &benchWriter{header: make(http.Header, 4)}
	sw := &streamWriter{w: writer, family: openai.FamilyChat}
	if err := sw.emit(frame); err != nil || writer.status != http.StatusOK || writer.flushed != 1 {
		b.Fatalf("emit: %v, status %d, flushed %d", err, writer.status, writer.flushed)
	}
	b.SetBytes(int64(len(frame)))
	b.ReportAllocs()
	for b.Loop() {
		if err := sw.emit(frame); err != nil {
			b.Fatal(err)
		}
	}
}

// benchChat is a chat completion of the size a short answer is, as the upstream
// states it.
const benchChat = `{"id":"chatcmpl-bench","object":"chat.completion","created":1800000000,"model":"gpt-bench","choices":[{"index":0,"message":{"role":"assistant","content":"The capital of France is Paris, a city on the Seine known for its museums, its cafes and the tower that carries the name of its engineer."},"finish_reason":"stop"}],"usage":{"prompt_tokens":24,"completion_tokens":31,"total_tokens":55}}`

// benchChatStream is the same answer streamed in 64 chunks, the shape a model
// that writes a paragraph produces, with the usage the stream ends on.
func benchChatStream() []byte {
	var stream bytes.Buffer
	for i := range 64 {
		delta := `{"content":"word `
		if i == 0 {
			delta = `{"role":"assistant","content":"word `
		}
		fmt.Fprintf(&stream, "data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1800000000,\"model\":\"gpt-bench\",\"choices\":[{\"index\":0,\"delta\":%s%d \"},\"finish_reason\":null}]}\n\n", delta, i)
	}
	stream.WriteString("data: {\"id\":\"chatcmpl-bench\",\"object\":\"chat.completion.chunk\",\"created\":1800000000,\"model\":\"gpt-bench\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":24,\"completion_tokens\":64,\"total_tokens\":88}}\n\n")
	stream.WriteString("data: [DONE]\n\n")
	return stream.Bytes()
}

// BenchmarkGateway is the whole request path of a gateway that has nothing to
// wait for: the key looked up, the request parsed, planned and estimated, its
// admission, the attempt, the translation of what the upstream sent and the
// writing of the response, with the provider and the client held in memory. The
// key is looked up in a fake runtime, which BenchmarkAuthenticate of the runtime
// package measures as the real one does it. The streaming cases are the relay of
// 64 chunk frames, which is what a stream costs the gateway beside what the
// codecs spend on each.
//
//	go test ./internal/gateway -run '^$' -bench BenchmarkGateway -benchmem
func BenchmarkGateway(b *testing.B) {
	chat := []byte(`{"model":"` + routeSlug + `","messages":[{"role":"system","content":"You are a concise assistant."},{"role":"user","content":"What is the capital of France?"}],"max_tokens":128}`)
	chatStream := []byte(`{"model":"` + routeSlug + `","messages":[{"role":"system","content":"You are a concise assistant."},{"role":"user","content":"What is the capital of France?"}],"max_tokens":128,"stream":true,"stream_options":{"include_usage":true}}`)
	messages := []byte(`{"model":"` + routeSlug + `","system":"You are a concise assistant.","messages":[{"role":"user","content":"What is the capital of France?"}],"max_tokens":128,"stream":true}`)
	for _, tc := range []struct {
		name      string
		path      string
		request   []byte
		upstream  benchTransport
		anthropic bool
		want      string
	}{
		{"openai/unary", "/v1/chat/completions", chat, benchTransport{body: []byte(benchChat), header: http.Header{"Content-Type": {"application/json"}}}, false, `"object":"chat.completion"`},
		{"openai/stream", "/v1/chat/completions", chatStream, benchTransport{body: benchChatStream(), header: http.Header{"Content-Type": {"text/event-stream"}}}, false, "[DONE]"},
		{"anthropic-to-openai/stream", "/anthropic/v1/messages", messages, benchTransport{body: benchChatStream(), header: http.Header{"Content-Type": {"text/event-stream"}}}, true, "message_stop"},
	} {
		b.Run(tc.name, func(b *testing.B) {
			tc.upstream.status = http.StatusOK
			handler := benchGateway(b, &tc.upstream, "gpt-bench", benchLimits{})
			body := bytes.NewReader(tc.request)
			request := httptest.NewRequestWithContext(b.Context(), http.MethodPost, tc.path, body)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+fullKey)
			if tc.anthropic {
				request.Header.Set("Anthropic-Version", "2023-06-01")
			}
			request.ContentLength = int64(len(tc.request))
			writer := &benchWriter{header: make(http.Header, 16)}
			serve := func() {
				body.Reset(tc.request)
				request.Body = io.NopCloser(body)
				writer.reset()
				handler.ServeHTTP(writer, request)
			}
			// One request outside the measurement proves the case serves what it names.
			writer.kept = &bytes.Buffer{}
			serve()
			if writer.status != http.StatusOK || !strings.Contains(writer.kept.String(), tc.want) {
				b.Fatalf("status %d, wrote %q, want it to contain %q", writer.status, writer.kept, tc.want)
			}
			writer.kept = nil
			b.ReportAllocs()
			b.SetBytes(int64(writer.written))
			for b.Loop() {
				serve()
			}
			b.ReportMetric(float64(writer.flushed), "flushes/op")
		})
	}
}

// benchProse is prompt text of about tokens tokens, four characters to each, the
// way a document that a caller pastes in is written: lines, quotation marks and a
// few characters that a JSON string has to escape.
func benchProse(tokens int) string {
	const paragraph = "The quarterly report covers revenue, churn and support load across every region.\n" +
		"Customers wrote: \"the export is slow\" and asked for a refund, a call back, or both.\n"
	return strings.Repeat(paragraph, tokens*4/len(paragraph)+1)[:tokens*4]
}

// benchLargeChat is a chat request whose one user message holds prompt, with the
// system message and reply bound the high-throughput scenario sends.
func benchLargeChat(b *testing.B, prompt string, stream bool) []byte {
	b.Helper()
	text, err := json.Marshal(prompt)
	if err != nil {
		b.Fatal(err)
	}
	options := ""
	if stream {
		options = `,"stream":true,"stream_options":{"include_usage":true}`
	}
	return []byte(`{"model":"` + routeSlug + `","max_tokens":16,"messages":[{"role":"system","content":"You are a concise assistant."},{"role":"user","content":` + string(text) + `}]` + options + `}`)
}

// benchLargeMessages is the same request as an Anthropic client sends it.
func benchLargeMessages(b *testing.B, prompt string, stream bool) []byte {
	b.Helper()
	text, err := json.Marshal(prompt)
	if err != nil {
		b.Fatal(err)
	}
	options := ""
	if stream {
		options = `,"stream":true`
	}
	return []byte(`{"model":"` + routeSlug + `","max_tokens":16,"system":"You are a concise assistant.","messages":[{"role":"user","content":` + string(text) + `}]` + options + `}`)
}

// BenchmarkGatewayLargePrompt is the request path of the high-throughput
// scenario: a prompt of 100,000 tokens, which is 400 KB, sent by a key with a
// cost budget to a model that has a tokenizer, so that the walk of the prompt,
// its count, the price it could cost and the body sent upstream are all paid, and
// the upstream answers at once. "prose" is text as a caller writes it, with lines
// and quotation marks to escape. "plain" is words of one token each with nothing
// to escape, as the benchmark harness's prompt is, though it repeats one word
// where the harness draws its words from a vocabulary. "anthropic" is the prose
// sent as an Anthropic client sends it, to the same OpenAI upstream, which is
// translated on its way.
//
//	go test ./internal/gateway -run '^$' -bench BenchmarkGatewayLargePrompt -benchmem
func BenchmarkGatewayLargePrompt(b *testing.B) {
	const tokens = 100_000
	prose := benchProse(tokens)
	unary := benchTransport{body: []byte(benchChat), header: http.Header{"Content-Type": {"application/json"}}}
	stream := benchTransport{body: benchChatStream(), header: http.Header{"Content-Type": {"text/event-stream"}}}
	for _, tc := range []struct {
		name      string
		prompt    string
		stream    bool
		anthropic bool
		upstream  benchTransport
		want      string
	}{
		{"prose/unary", prose, false, false, unary, `"object":"chat.completion"`},
		{"prose/stream", prose, true, false, stream, "[DONE]"},
		{"plain/unary", strings.Repeat("the ", tokens) + "ping", false, false, unary, `"object":"chat.completion"`},
		{"plain/stream", strings.Repeat("the ", tokens) + "ping", true, false, stream, "[DONE]"},
		{"anthropic/unary", prose, false, true, unary, `"type":"message"`},
	} {
		b.Run(tc.name, func(b *testing.B) {
			tc.upstream.status = http.StatusOK
			handler := benchGateway(b, &tc.upstream, "gpt-4o-bench", benchLimits{maxBody: 2 << 20, budget: true})
			path, request := "/v1/chat/completions", benchLargeChat(b, tc.prompt, tc.stream)
			if tc.anthropic {
				path, request = "/anthropic/v1/messages", benchLargeMessages(b, tc.prompt, tc.stream)
			}
			body := bytes.NewReader(request)
			r := httptest.NewRequestWithContext(b.Context(), http.MethodPost, path, body)
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+fullKey)
			if tc.anthropic {
				r.Header.Set("Anthropic-Version", "2023-06-01")
			}
			r.ContentLength = int64(len(request))
			writer := &benchWriter{header: make(http.Header, 16)}
			serve := func() {
				body.Reset(request)
				r.Body = io.NopCloser(body)
				writer.reset()
				handler.ServeHTTP(writer, r)
			}
			writer.kept = &bytes.Buffer{}
			serve()
			if writer.status != http.StatusOK || !strings.Contains(writer.kept.String(), tc.want) {
				b.Fatalf("status %d, wrote %q, want it to contain %q", writer.status, writer.kept, tc.want)
			}
			writer.kept = nil
			b.ReportAllocs()
			b.SetBytes(int64(len(request)))
			start := processCPU()
			for b.Loop() {
				serve()
			}
			// The wall time of a request leaves out the collector, which runs on
			// other cores, and a request that allocates less spends less of it.
			b.ReportMetric(float64(processCPU()-start)/float64(b.N), "cpu-ns/op")
		})
	}
}

// processCPU is the CPU time this process has used, user and system, in
// nanoseconds.
func processCPU() int64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return usage.Utime.Nano() + usage.Stime.Nano()
}
