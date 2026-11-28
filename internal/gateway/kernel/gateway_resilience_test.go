package kernel

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/routing"
)

type entryReply struct {
	status int
	retry  string
	code   string
	param  string
}

func enter(t *testing.T, h *harness, headers map[string]string, body string) entryReply {
	t.Helper()
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(body), headers)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded struct {
		Error struct {
			Code  string  `json:"code"`
			Param *string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("reply %q is not JSON: %v", raw, err)
	}
	reply := entryReply{status: resp.StatusCode, retry: resp.Header.Get("X-Should-Retry"), code: decoded.Error.Code}
	if decoded.Error.Param != nil {
		reply.param = *decoded.Error.Param
	}
	return reply
}

func failingProvider(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
}

const entryChat = `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}]}`

func TestAProviderFailureFallsBackOnATransformedRouteAndIsFinalWithNoRetryHintOnAStrictOne(t *testing.T) {
	for name, tc := range map[string]struct {
		harness *harness
		want    entryReply
		second  int
	}{
		"transformed": {newHarness(t, Config{}), entryReply{status: http.StatusBadGateway, code: "upstream_unavailable"}, 1},
		"strict":      {strictHarness(t, nil), entryReply{status: http.StatusBadGateway, code: "ambiguous_upstream_result", retry: "false"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			h := tc.harness
			h.mock.set("a", failingProvider)
			h.mock.set("b", failingProvider)
			if got := enter(t, h, nil, entryChat); got != tc.want {
				t.Errorf("reply = %+v, want %+v", got, tc.want)
			}
			if first, second := h.mock.count("a"), h.mock.count("b"); first != 1 || second != tc.second {
				t.Errorf("provider calls = %d and %d, want 1 and %d", first, second, tc.second)
			}
			if env := h.sink.last(t); env.Status != http.StatusBadGateway || len(env.Attempts) != 1+tc.second {
				t.Errorf("recorded status %d with %d attempts, want 502 with %d", env.Status, len(env.Attempts), 1+tc.second)
			}
		})
	}
}

type messagesTransport func(*http.Request) (*http.Response, error)

func (f messagesTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeMessagesProviderRequestIDsStayWithTheirAttempts(t *testing.T) {
	const (
		failedID  = "req_Unavailable123"
		successID = "req_Served456"
		gatewayID = "gateway-messages-1"
	)
	official := connectors.DefaultEndpoint(connectors.KindAnthropic, "", "")
	officialURL, err := url.Parse(official)
	if err != nil {
		t.Fatal(err)
	}
	withPort := func(port string) string {
		endpoint := *officialURL
		endpoint.Host = net.JoinHostPort(endpoint.Hostname(), port)
		return endpoint.String()
	}
	for _, tc := range []struct {
		name, endpoint string
		firstHeader    []string
		refuse         bool
		wantFirst      string
		wantSecond     string
	}{
		{name: "official fallback", endpoint: official, firstHeader: []string{failedID}, wantFirst: failedID, wantSecond: successID},
		{name: "explicit HTTPS port", endpoint: withPort("443"), firstHeader: []string{failedID}, wantFirst: failedID, wantSecond: successID},
		{name: "nondefault port", endpoint: withPort("8443"), firstHeader: []string{failedID}},
		{name: "custom host", endpoint: "https://custom.example/v1", firstHeader: []string{failedID}},
		{name: "duplicate header", endpoint: official, firstHeader: []string{failedID, failedID}, wantSecond: successID},
		{name: "combined header", endpoint: official, firstHeader: []string{failedID + ", " + successID}, wantSecond: successID},
		{name: "oversized header", endpoint: official, firstHeader: []string{"req_" + strings.Repeat("A", 125)}, wantSecond: successID},
		{name: "wrong format", endpoint: official, firstHeader: []string{"sk_secret"}, wantSecond: successID},
		{name: "missing header", endpoint: official, wantSecond: successID},
		{name: "local refusal", endpoint: official, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := strictRelease(t, newHarness(t, Config{}), func(snapshot *routing.Snapshot) {
				route := snapshot.Routes[routeSlug]
				route.Fidelity.Mode = routing.FidelityTransformed
				snapshot.Routes[routeSlug] = route
				for id, provider := range snapshot.Providers {
					provider.Endpoint, provider.Kind = tc.endpoint, connectors.KindAnthropic
					provider.ProfileID, provider.ProfileRevision = "", ""
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "generation", Surface: "anthropic", Mode: "unary"}}
					snapshot.Providers[id] = provider
				}
			})
			var calls atomic.Int32
			h.gateway.client.Transport = messagesTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != tc.endpoint+"/messages" {
					return nil, fmt.Errorf("unexpected upstream URL %s", r.URL)
				}
				stub := http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"req_OtherHeader"}}
				if calls.Add(1) == 1 {
					stub["Request-Id"] = tc.firstHeader
					return &http.Response{StatusCode: 503, Header: stub, Request: r, Body: io.NopCloser(strings.NewReader(
						`{"type":"error","error":{"type":"overloaded_error","message":"unavailable"},"request_id":"req_BodyID"}`))}, nil
				}
				stub.Set("Request-Id", successID)
				return &http.Response{StatusCode: 200, Header: stub, Request: r, Body: io.NopCloser(strings.NewReader(
					`{"id":"msg_1","type":"message","role":"assistant","model":"model-b","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":2,"output_tokens":3}}`))}, nil
			})
			headers := map[string]string{"Anthropic-Version": "2023-06-01", "X-Request-Id": gatewayID, "Request-Id": "req_ClientSupplied"}
			if tc.refuse {
				headers["X-OLP-Routing"] = "{"
			}
			resp := h.do(t.Context(), "POST", "/anthropic/v1/messages", fullKey,
				[]byte(`{"model":"`+routeSlug+`","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`), headers)
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
			env := h.sink.last(t)
			if resp.Header.Get("X-Request-Id") != gatewayID || resp.Header.Get("Request-Id") != gatewayID || env.RequestID != gatewayID {
				t.Fatalf("gateway request ID changed: headers=%v request=%q", resp.Header, env.RequestID)
			}
			if tc.refuse {
				if resp.StatusCode != 400 || calls.Load() != 0 || len(env.Attempts) != 0 {
					t.Fatalf("local refusal returned %d, sent %d calls and recorded %d attempts", resp.StatusCode, calls.Load(), len(env.Attempts))
				}
				return
			}
			if resp.StatusCode != 200 || calls.Load() != 2 || bytes.Contains(body, []byte(successID)) || bytes.Contains(body, []byte(failedID)) {
				t.Fatalf("fallback returned %d with %d calls: %s", resp.StatusCode, calls.Load(), body)
			}
			if len(env.Attempts) != 2 || env.Attempts[0].Status != 503 || env.Attempts[1].Status != 200 {
				t.Fatalf("unexpected attempts: %+v", env.Attempts)
			}
			event, err := events.UsageEvent(env)
			if err != nil || event == nil {
				t.Fatalf("usage event unavailable: %v", err)
			}
			if event.Attempts[0].ProviderRequestID != tc.wantFirst || event.Attempts[1].ProviderRequestID != tc.wantSecond {
				t.Errorf("provider request IDs = %q and %q, want %q and %q", event.Attempts[0].ProviderRequestID, event.Attempts[1].ProviderRequestID, tc.wantFirst, tc.wantSecond)
			}
		})
	}
}

func TestRequestsTheGatewayCannotUnderstandAreRefusedBeforeAnyProviderAndRecorded(t *testing.T) {
	image := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAAAAAA"}}]}]}`
	approval := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}],"olp_approval":{"approved":true}}`
	for name, tc := range map[string]struct {
		headers map[string]string
		body    string
		want    entryReply
	}{
		"routing preferences that are not an object": {map[string]string{"X-OLP-Routing": "{"}, entryChat,
			entryReply{status: http.StatusBadRequest, code: "invalid_request", param: "X-OLP-Routing"}},
		"a priority above the key's ceiling": {map[string]string{"X-OLP-Routing": `{"priority":"critical"}`}, entryChat,
			entryReply{status: http.StatusBadRequest, code: "priority_increase_forbidden", param: "X-OLP-Routing"}},
		"attribution that is not an object": {map[string]string{"X-OLP-Attribution": "!!!"}, entryChat,
			entryReply{status: http.StatusBadRequest, code: "invalid_attribution"}},
		"inline media over the installation limit": {nil, image,
			entryReply{status: http.StatusBadRequest, code: "unsupported_parameter", param: "content"}},
		"a tool approval on a route that runs no tools": {nil, approval,
			entryReply{status: http.StatusForbidden, code: "tool_approval_invalid"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{InlineMedia: protocols.InlineMediaLimits{Items: 4, ItemBytes: 2, TotalBytes: 2}})
			if got := enter(t, h, tc.headers, tc.body); got != tc.want {
				t.Errorf("reply = %+v, want %+v", got, tc.want)
			}
			if calls := h.mock.count("a") + h.mock.count("b"); calls != 0 {
				t.Errorf("provider calls = %d, want none", calls)
			}
			if env := h.sink.last(t); env.Status != tc.want.status || len(env.Attempts) != 0 {
				t.Errorf("recorded status %d with %d attempts, want %d with none", env.Status, len(env.Attempts), tc.want.status)
			}
		})
	}
}

type lostCaller struct {
	header       http.Header
	write, flush error
}

func (c lostCaller) Header() http.Header            { return c.header }
func (lostCaller) WriteHeader(int)                  {}
func (c lostCaller) Write(b []byte) (int, error)    { return len(b), c.write }
func (lostCaller) SetReadDeadline(time.Time) error  { return nil }
func (lostCaller) SetWriteDeadline(time.Time) error { return nil }
func (c lostCaller) FlushError() error              { return c.flush }

func TestAWholeReplyCountsAsDeliveredOnlyWhenTheCallerReceivesIt(t *testing.T) {
	gone := errors.New("caller is gone")
	lost := func(caller lostCaller) func(*harness) {
		return func(h *harness) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(entryChat))
			req.Header.Set("Authorization", "Bearer "+fullKey)
			req.Header.Set("Content-Type", "application/json")
			h.server.Config.Handler.ServeHTTP(caller, req)
		}
	}
	for name, tc := range map[string]struct {
		send      func(*harness)
		outcome   string
		delivered bool
	}{
		"received": {func(h *harness) {
			resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(entryChat), nil)
			resp.Body.Close()
		}, "success", true},
		"reply not written": {lost(lostCaller{header: http.Header{}, write: gone}), "cancelled", false},
		"reply not flushed": {lost(lostCaller{header: http.Header{}, flush: gone}), "cancelled", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{})
			tc.send(h)
			env := h.sink.last(t)
			attempt := env.Attempts[len(env.Attempts)-1]
			if env.Outcome != tc.outcome || !env.Committed || !attempt.Committed || (env.FirstByte != nil) != tc.delivered {
				t.Fatalf("envelope outcome %q, committed %t, attempt committed %t, first byte %v, want outcome %q and delivered %t",
					env.Outcome, env.Committed, attempt.Committed, env.FirstByte, tc.outcome, tc.delivered)
			}
		})
	}
}

type attemptSeen struct {
	class     string
	status    int
	committed bool
}

const stalled = "<stalled>"

func providerStream(frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range frames {
			if frame == stalled {
				<-r.Context().Done()
				return
			}
			io.WriteString(w, "data: "+frame+"\n\n")
			w.(http.Flusher).Flush()
		}
	}
}

func providerReply(status int, headers map[string]string, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for name, value := range headers {
			w.Header().Set(name, value)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

var replyErrorCode = regexp.MustCompile(`"error":\{"code":"([a-z_]+)"`)

func TestEachKindOfProviderFailureIsClassifiedAndFallsBackOnlyWhileTheReplyIsUncommitted(t *testing.T) {
	const (
		delta     = `{"choices":[{"index":0,"delta":{"content":"partial"}}]}`
		inBand    = `{"error":{"message":"boom","type":"server_error"}}`
		oversized = 5000

		routeDeadline = 5 * time.Second
	)
	served := attemptSeen{"success", http.StatusOK, true}
	for name, tc := range map[string]struct {
		provider http.HandlerFunc
		streamed bool
		timeout  int64
		status   int
		code     string
		attempts []attemptSeen
	}{
		"rate limited": {
			provider: providerReply(http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, `{"error":{"message":"slow down","type":"rate_limit_error"}}`),
			status:   http.StatusOK, attempts: []attemptSeen{{"rate_limit", http.StatusTooManyRequests, false}, served},
		},
		"credential refused": {
			provider: providerReply(http.StatusUnauthorized, nil, `{"error":{"message":"bad key","type":"invalid_request_error"}}`),
			status:   http.StatusOK, attempts: []attemptSeen{{"credential", http.StatusUnauthorized, false}, served},
		},
		"silent provider": {
			provider: providerStream(stalled), timeout: 80,
			status: http.StatusOK, attempts: []attemptSeen{{"timeout", 0, false}, served},
		},
		"stream opening with a provider error": {
			provider: providerStream(inBand), streamed: true,
			status: http.StatusOK, attempts: []attemptSeen{{"upstream_server", http.StatusOK, false}, served},
		},
		"unreadable reply": {
			provider: providerReply(http.StatusOK, nil, `not json`),
			status:   http.StatusBadGateway, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, false}},
		},
		"whole reply to a streamed request": {
			provider: providerReply(http.StatusOK, nil, `{}`), streamed: true,
			status: http.StatusBadGateway, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, false}},
		},
		"stream frame over the event limit": {
			provider: providerStream(`{"choices":[{"index":0,"delta":{"content":"` + strings.Repeat("a", oversized) + `"}}]}`), streamed: true,
			status: http.StatusBadGateway, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, false}},
		},
		"stream stalling after its first frame": {
			provider: providerStream(delta, stalled), streamed: true, timeout: 80,
			status: http.StatusOK, code: "gateway_timeout", attempts: []attemptSeen{{"timeout", http.StatusOK, true}},
		},
		"stream ending without a final frame": {
			provider: providerStream(delta), streamed: true,
			status: http.StatusOK, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, true}},
		},
		"provider error after the first frame": {
			provider: providerStream(delta, inBand), streamed: true,
			status: http.StatusOK, code: "provider_protocol_error", attempts: []attemptSeen{{"protocol", http.StatusOK, true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{})
			if tc.timeout > 0 {
				h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
					route := s.Routes[routeSlug]
					for i := range route.Targets {
						route.Targets[i].Timeout = tc.timeout
					}
					s.Routes[routeSlug] = route
				})
			}
			h.mock.set("a", tc.provider)
			body := entryChat
			if tc.streamed {
				h.mock.set("b", streamedChat(answerText))
				body = strings.TrimSuffix(entryChat, "}") + `,"stream":true}`
			}
			started := time.Now()
			resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(body), nil)
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if elapsed := time.Since(started); tc.timeout > 0 && elapsed > routeDeadline/2 {
				t.Fatalf("a provider silent past its %dms target timeout held the caller for %v", tc.timeout, elapsed)
			}

			code := ""
			if found := replyErrorCode.FindSubmatch(raw); found != nil {
				code = string(found[1])
			}
			if resp.StatusCode != tc.status || code != tc.code {
				t.Fatalf("reply = %d %q, want %d %q: %.300s", resp.StatusCode, code, tc.status, tc.code, raw)
			}
			if strings.Contains(string(raw), strings.Repeat("a", oversized)) {
				t.Fatal("the caller received a frame over the event limit")
			}
			if tc.code == "" && !strings.Contains(string(raw), answerText) {
				t.Fatalf("the caller did not get the second provider's answer: %.300s", raw)
			}
			if last := tc.attempts[len(tc.attempts)-1]; tc.code != "" && strings.Contains(string(raw), "partial") != last.committed {
				t.Fatalf("partial output reached the caller = %t, want %t", !last.committed, last.committed)
			}

			env := h.sink.last(t)
			seen := make([]attemptSeen, len(env.Attempts))
			for i, attempt := range env.Attempts {
				seen[i] = attemptSeen{attempt.Class, attempt.Status, attempt.Committed}
			}
			if len(seen) != len(tc.attempts) {
				t.Fatalf("attempts = %+v, want %+v", seen, tc.attempts)
			}
			for i, want := range tc.attempts {
				if seen[i] != want {
					t.Fatalf("attempt %d = %+v, want %+v", i, seen[i], want)
				}
			}
			first := env.Attempts[0]
			if name == "rate limited" && (first.RetryAfter == nil || *first.RetryAfter != 7*time.Second) {
				t.Fatalf("retry after = %v, want the provider's 7s", first.RetryAfter)
			}
			if name == "silent provider" && first.FirstByte != nil {
				t.Fatal("a provider that never answered has a first byte time")
			}
			if succeeded := env.Outcome == "success"; succeeded != (tc.code == "") {
				t.Fatalf("outcome = %s for reply code %q", env.Outcome, tc.code)
			}
		})
	}
}

type providerCalls struct{ a, b atomic.Int64 }

func TestOversizedProjectedFirstFrameDoesNotCommitStream(t *testing.T) {
	t.Parallel()
	const frame = `{"model":"x","choices":[{"index":0,"delta":{"content":"first frame"}}]}`
	h := newHarness(t, Config{MaxEventBytes: int64(len("data: "+frame+"\n\n") + 1)})
	h.mock.set("a", providerStream(frame))
	body := strings.TrimSuffix(entryChat, "}") + `,"stream":true}`
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(body), nil)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusBadGateway || strings.Contains(string(raw), "first frame") {
		t.Fatalf("opening frame rejection = %d %s, read error = %v", resp.StatusCode, raw, err)
	}
	env := h.sink.last(t)
	if env.Committed || len(env.Attempts) != 1 || env.Attempts[0].Committed {
		t.Fatalf("rejected frame recorded a committed stream: request=%t attempts=%+v", env.Committed, env.Attempts)
	}
}

func refusing(calls *atomic.Int64, refusals int64, status int, retryAfter string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) > refusals {
			completion(modelA, answerText)(w, r)
			return
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"message":"refused"}}`)
	}
}

func unreachableLimits(t *testing.T) Dependencies {
	t.Helper()
	runner, err := scripts.NewRunner(unreachableValkey{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	closed := func() limitadmission.OutagePolicy { return limitadmission.FailClosed }
	return Dependencies{Admission: quota.NewAdmission(limitadmission.NewDurable(runner, nil), fleet.New(runner), closed, slog.New(slog.DiscardHandler))}
}

func TestARouteRetriesAProviderOnlyUnderItsRetryRuleThenMovesOnAndSaysWhyNoProviderServed(t *testing.T) {
	const always = 1 << 20
	brief := routing.Retry{classRateLimit: {MaxRetries: 2, BaseBackoffMS: 1, MaxBackoffMS: 1}}
	patient := routing.Retry{classRateLimit: {MaxRetries: 2, BaseBackoffMS: 1, MaxBackoffMS: 1, RespectRetryAfter: true}}
	serverRetry := routing.Retry{ClassUpstreamServer: {MaxRetries: 1, BaseBackoffMS: 1, MaxBackoffMS: 1}}
	serverRetryWithHint := routing.Retry{ClassUpstreamServer: {MaxRetries: 1, BaseBackoffMS: 1, MaxBackoffMS: 1, RespectRetryAfter: true}}
	limited := &routing.Limits{RequestsPerMinute: new(int64(5))}
	for name, tc := range map[string]struct {
		retry      routing.Retry
		refusals   [2]int64
		refusal    int
		retryAfter string
		limits     [2]*routing.Limits
		deadline   int64
		status     int
		code       string
		calls      [2]int64
		retries    []int
		budget     int
	}{
		"a rate limited provider is retried until it answers": {
			retry: brief, refusals: [2]int64{2, 0}, refusal: http.StatusTooManyRequests,
			status: http.StatusOK, calls: [2]int64{3, 0}, retries: []int{0, 1, 2},
		},
		"retries run out and the next provider serves": {
			retry: brief, refusals: [2]int64{always, 0}, refusal: http.StatusTooManyRequests,
			status: http.StatusOK, calls: [2]int64{3, 1}, retries: []int{0, 1, 2, 0}, budget: 4,
		},
		"a failure without a retry rule moves on at once": {
			retry: brief, refusals: [2]int64{always, 0}, refusal: http.StatusInternalServerError,
			status: http.StatusOK, calls: [2]int64{1, 1}, retries: []int{0, 0},
		},
		"a wait the provider asks for past the deadline is not taken": {
			retry: patient, refusals: [2]int64{always, 0}, refusal: http.StatusTooManyRequests, retryAfter: "60", deadline: 400,
			status: http.StatusOK, calls: [2]int64{1, 1}, retries: []int{0, 0},
		},
		"an unavailable provider asking for a wait past the deadline is skipped": {
			retry: serverRetryWithHint, refusals: [2]int64{1, 0}, refusal: http.StatusServiceUnavailable, retryAfter: "60", deadline: 400,
			status: http.StatusOK, calls: [2]int64{1, 1}, retries: []int{0, 0},
		},
		"an unavailable provider is retried without opting in to its wait": {
			retry: serverRetry, refusals: [2]int64{1, 0}, refusal: http.StatusServiceUnavailable, retryAfter: "60", deadline: 400,
			status: http.StatusOK, calls: [2]int64{2, 0}, retries: []int{0, 1},
		},
		"another server error does not set the retry wait": {
			retry: serverRetryWithHint, refusals: [2]int64{1, 0}, refusal: http.StatusInternalServerError, retryAfter: "60", deadline: 400,
			status: http.StatusOK, calls: [2]int64{2, 0}, retries: []int{0, 1},
		},
		"every provider failing reports the last failure": {
			refusals: [2]int64{always, always}, refusal: http.StatusInternalServerError,
			status: http.StatusBadGateway, code: "upstream_unavailable", calls: [2]int64{1, 1}, retries: []int{0, 0},
		},
		"a provider whose limit cannot be read is passed over": {
			limits: [2]*routing.Limits{limited, nil},
			status: http.StatusOK, calls: [2]int64{0, 1}, retries: []int{0},
		},
		"no provider with a readable limit reports the limits": {
			limits: [2]*routing.Limits{limited, limited},
			status: http.StatusServiceUnavailable, code: "distributed_limits_unavailable",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var deps []Dependencies
			if tc.limits != [2]*routing.Limits{} {
				deps = append(deps, unreachableLimits(t))
			}
			h := newHarness(t, Config{}, deps...)
			budget := 0
			h.republish(func(s *routing.Snapshot, a, b routing.Provider) {
				route := s.Routes[routeSlug]
				route.Retry = tc.retry
				route.MaxAttempts = cmp.Or(tc.budget, route.MaxAttempts)
				if tc.deadline > 0 {
					route.OverallTimeout = tc.deadline
				}
				budget = route.MaxAttempts
				s.Routes[routeSlug] = route
				for i, provider := range []routing.Provider{a, b} {
					provider.Limits = tc.limits[i]
					s.Providers[provider.ID] = provider
				}
			})
			var calls providerCalls
			h.mock.set("a", refusing(&calls.a, tc.refusals[0], tc.refusal, tc.retryAfter))
			h.mock.set("b", refusing(&calls.b, tc.refusals[1], tc.refusal, tc.retryAfter))

			reply := enter(t, h, nil, entryChat)

			if reply.status != tc.status || reply.code != tc.code {
				t.Fatalf("reply = %d %q, want %d %q", reply.status, reply.code, tc.status, tc.code)
			}
			if got := [2]int64{calls.a.Load(), calls.b.Load()}; got != tc.calls {
				t.Fatalf("provider calls = %v, want %v", got, tc.calls)
			}
			attempts := h.sink.last(t).Attempts
			retries := make([]int, len(attempts))
			for i, fact := range attempts {
				retries[i] = fact.Retry
			}
			if !slices.Equal(retries, tc.retries) {
				t.Fatalf("recorded retries = %v, want %v", retries, tc.retries)
			}
			if len(attempts) > budget {
				t.Fatalf("%d attempts ran on a route allowing %d", len(attempts), budget)
			}
		})
	}
}

func TestARetryStopsAtTheRoutesAttemptBudgetAndReportsTheProvidersRefusal(t *testing.T) {
	h := newHarness(t, Config{})
	budget := 0
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.Retry = routing.Retry{classRateLimit: {MaxRetries: 10, BaseBackoffMS: 1, MaxBackoffMS: 1}}
		budget = route.MaxAttempts
		s.Routes[routeSlug] = route
	})
	var calls providerCalls
	h.mock.set("a", refusing(&calls.a, 1<<20, http.StatusTooManyRequests, ""))
	h.mock.set("b", refusing(&calls.b, 0, 0, ""))

	reply := enter(t, h, nil, entryChat)

	if reply.status != http.StatusTooManyRequests || reply.code != "upstream_rate_limit" {
		t.Fatalf("reply = %d %q, want the provider's rate limit", reply.status, reply.code)
	}
	if calls.a.Load() != int64(budget) || calls.b.Load() != 0 {
		t.Fatalf("provider calls = %d and %d on a route allowing %d attempts", calls.a.Load(), calls.b.Load(), budget)
	}
}

func TestACallerLeavingDuringARetryWaitEndsTheRequestAsCancelledWithoutAnotherAttempt(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
		route := s.Routes[routeSlug]
		route.Retry = routing.Retry{classRateLimit: {MaxRetries: 2, BaseBackoffMS: 1, MaxBackoffMS: 1, RespectRetryAfter: true}}
		s.Routes[routeSlug] = route
	})
	var calls providerCalls
	h.mock.set("a", refusing(&calls.a, 1<<20, http.StatusTooManyRequests, "2"))
	h.mock.set("b", refusing(&calls.b, 0, 0, ""))
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/chat/completions", strings.NewReader(entryChat))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fullKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("the caller got status %d while the route was waiting to retry", resp.StatusCode)
	}

	env := h.sink.last(t)
	if env.Outcome != "cancelled" || len(env.Attempts) != 1 || calls.a.Load() != 1 || calls.b.Load() != 0 {
		t.Fatalf("outcome = %q with %d attempts, provider calls %d and %d; want one attempt and a cancelled request", env.Outcome, len(env.Attempts), calls.a.Load(), calls.b.Load())
	}
}

type unaryAttemptSeen struct {
	class     string
	status    int
	uncertain bool
}

func TestAWholeOperationFallsBackOnlyWhenTheProviderRefusedItAndIsNeverRepeatedOnceItMayHaveRun(t *testing.T) {
	const routeDeadline = 5 * time.Second
	const found = `{"type":"search","web":{"results":[{"title":"Found","url":"https://example.com/found","description":"result"}]}}`
	served := unaryAttemptSeen{"success", http.StatusOK, false}
	for name, tc := range map[string]struct {
		provider   http.HandlerFunc
		timeout    int64
		status     int
		code       string
		final      bool
		retryAfter time.Duration
		attempts   []unaryAttemptSeen
	}{
		"rate limited": {
			provider: providerReply(http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, `{"error":{"message":"slow down"}}`),
			status:   http.StatusOK, retryAfter: 7 * time.Second, attempts: []unaryAttemptSeen{{"rate_limit", http.StatusTooManyRequests, false}, served},
		},
		"credential refused": {
			provider: providerReply(http.StatusUnauthorized, nil, `{"error":{"message":"bad key"}}`),
			status:   http.StatusOK, attempts: []unaryAttemptSeen{{"credential", http.StatusUnauthorized, false}, served},
		},
		"request the provider rejects": {
			provider: providerReply(http.StatusBadRequest, nil, `{"error":{"message":"bad","type":"invalid_request_error"}}`),
			status:   http.StatusBadRequest, code: "upstream_rejected", attempts: []unaryAttemptSeen{{"upstream_client", http.StatusBadRequest, false}},
		},
		"provider error": {
			provider: providerReply(http.StatusInternalServerError, nil, `{"error":{"message":"boom"}}`),
			status:   http.StatusBadGateway, code: "ambiguous_upstream_result", final: true, attempts: []unaryAttemptSeen{{"ambiguous", http.StatusInternalServerError, true}},
		},
		"silent provider": {
			provider: providerStream(stalled), timeout: 80,
			status: http.StatusBadGateway, code: "ambiguous_upstream_result", final: true, attempts: []unaryAttemptSeen{{"ambiguous", 0, true}},
		},
		"unreadable reply": {
			provider: providerReply(http.StatusOK, nil, `not json`),
			status:   http.StatusBadGateway, code: "fidelity_protocol_violation", final: true, attempts: []unaryAttemptSeen{{"protocol", http.StatusOK, true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, Config{})
			h.republish(func(s *routing.Snapshot, _, _ routing.Provider) {
				for id, provider := range s.Providers {
					provider.VendorID = "brave"
					provider.ProfileID, provider.ProfileRevision = "brave-search", connectors.ProfileRevision
					provider.Capabilities = []routing.Capability{{Model: provider.Capabilities[0].Model, Operation: "search", Surface: "openai", Mode: "unary"}}
					s.Providers[id] = provider
				}
				route := s.Routes[routeSlug]
				route.Operations = []string{"search"}
				for i := range route.Targets {
					route.Targets[i].Timeout = cmp.Or(tc.timeout, route.Targets[i].Timeout)
				}
				s.Routes[routeSlug] = route
			})
			var second atomic.Int64
			h.mock.set("a", tc.provider)
			h.mock.set("b", func(w http.ResponseWriter, r *http.Request) {
				second.Add(1)
				providerReply(http.StatusOK, nil, found)(w, r)
			})
			started := time.Now()
			resp := h.do(t.Context(), http.MethodPost, "/v1/search", fullKey, []byte(`{"query":"test","max_results":1}`), map[string]string{"X-OLP-Route": routeSlug})
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if elapsed := time.Since(started); tc.timeout > 0 && elapsed > routeDeadline/2 {
				t.Fatalf("a provider silent past its %dms target timeout held the caller for %v", tc.timeout, elapsed)
			}

			code := ""
			if found := replyErrorCode.FindSubmatch(raw); found != nil {
				code = string(found[1])
			}
			if resp.StatusCode != tc.status || code != tc.code {
				t.Fatalf("reply = %d %q, want %d %q: %.300s", resp.StatusCode, code, tc.status, tc.code, raw)
			}
			if tc.code == "" && !strings.Contains(string(raw), "https://example.com/found") {
				t.Fatalf("the caller did not get the result: %.300s", raw)
			}
			if final := resp.Header.Get("X-Should-Retry") == "false"; final != tc.final {
				t.Fatalf("the reply told the caller not to retry = %t, want %t", final, tc.final)
			}
			if asked := second.Load() == 1; asked != (len(tc.attempts) == 2) {
				t.Fatalf("the second provider was asked %d times after %s", second.Load(), name)
			}

			env := h.sink.last(t)
			seen := make([]unaryAttemptSeen, len(env.Attempts))
			for i, attempt := range env.Attempts {
				seen[i] = unaryAttemptSeen{attempt.Class, attempt.Status, attempt.BillingUncertain}
			}
			if !slices.Equal(seen, tc.attempts) {
				t.Fatalf("attempts = %+v, want %+v", seen, tc.attempts)
			}
			if got := cmp.Or(env.Attempts[0].RetryAfter, new(time.Duration)); *got != tc.retryAfter {
				t.Fatalf("the first attempt recorded a provider wait of %v, want %v", *got, tc.retryAfter)
			}
		})
	}
}

func TestStagedBodyFailureBlamesTheProviderOnlyWhenItsBodyBreaks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		body   io.Reader
		closed bool
		local  bool
	}{
		{"provider body breaks", iotest.ErrReader(errors.New("connection reset")), false, false},
		{"spool is unavailable", strings.NewReader("content"), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, Config{})
			spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = spool.Close() })
			if tc.closed {
				if err := spool.Close(); err != nil {
					t.Fatal(err)
				}
			}
			x := &Call{request: request{id: uuid.NewString(), startedAt: time.Now(), release: h.rt.release}, family: wire.File, callIdentity: callIdentity{actor: "api_key_resource", keyID: h.keyID}}
			response := &PinnedResponse{
				Response: &http.Response{Body: io.NopCloser(tc.body)},
				Exchange: h.gateway.beginPinned(t.Context(), x, testPinnedTarget(h, 0), http.MethodGet),
			}
			if _, err := response.StageBody(t.Context(), spool, "file-content", "application/octet-stream", 64); err == nil {
				t.Fatal("staging succeeded")
			}
			if fact := x.facts[0]; fact.Class != ClassProtocol || fact.LocalRefusal != tc.local {
				t.Fatalf("got class %q local %v, want local %v", fact.Class, fact.LocalRefusal, tc.local)
			}
		})
	}
}
