package codeforward

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode/codeclient"
	"github.com/tyk-swe/olp/internal/codemode/codewire"
	"github.com/tyk-swe/olp/internal/codemode/codexauth"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/connectors/providerconfig"
	"github.com/tyk-swe/olp/internal/domain/operations/tokenization/estimate"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/codeforward/codexwire"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type recordedCooldown struct {
	account        string
	model          string
	classification codeadapter.Classification
}

func TestCodeModelHintRefusalExplainsRetry(t *testing.T) {
	deadline := time.Now().Add(time.Minute)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	codeWriteError(recorder, request, &codemode.Refusal{Status: http.StatusBadRequest, Code: "code_connection_model_required", RetryAt: &deadline})
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Retry-After") == "" || body.Error.Code != "code_connection_model_required" || !strings.Contains(body.Error.Message, "X-OLP-Code-Model") || !strings.Contains(body.Error.Message, "Retry-After") {
		t.Fatalf("model hint refusal = %d, %v, %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}
}

type busyCodeLedger struct{ fakeCodeLedger }

func (*busyCodeLedger) Admit(context.Context, codesessions.Admission) (codesessions.Permit, error) {
	return codesessions.Permit{}, &codemode.Refusal{Status: http.StatusTooManyRequests, Code: "code_account_busy", RetryAt: new(time.Now().Add(time.Second))}
}

func (*busyCodeLedger) ResolvePaidFallback(_ context.Context, request codesessions.Admission, _ keyauthority.Authority) (*codemode.Operation, error) {
	if request.InitialRecovery {
		return nil, errors.New("local saturation must not consider paid fallback")
	}
	return nil, nil
}

type codeKeyFixture struct{ authority keyauthority.Authority }

func (k codeKeyFixture) Authenticate(string) (*keyauthority.Authority, error) {
	return &k.authority, nil
}

func TestCodeAccountBusyReturnsRetryWithoutPaidFallback(t *testing.T) {
	route := codemode.Route{
		ProjectID: "project", Slug: "code", Models: []string{"gpt-model"}, InitialRecoveryAttempts: 1,
		PaidParticipation: true, PaidFallbackTargets: map[string]codemode.PaidTarget{"gpt-model": {ProviderID: "paid", SlotID: "slot"}},
	}
	key := codeKeyFixture{authority: keyauthority.Authority{ID: "key", ProjectID: &route.ProjectID, Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}}}}
	ledger := &busyCodeLedger{}
	f := &Forwarder{gate: ingress.NewGate(key, nil, nil, 0, time.Now), ledger: ledger, now: time.Now}
	snapshot, err := release.NewRelease("", 0, &routing.Snapshot{Generation: routing.Generation{ID: uuid.NewString()}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/code/code/responses", nil)
	r.Header.Set("Authorization", "Bearer fixture")
	w := httptest.NewRecorder()
	inbound, _ := codeIngressFor("responses")
	observation := codemode.Request{Operation: codemode.Operation{Name: "responses", Model: "gpt-model"}, Recoverable: true}
	f.forwardCode(w, codeRequest{
		r: r, release: snapshot, route: route, observation: observation, inbound: inbound, transport: codeadapter.HTTP,
	}, nil, func(err error) {
		f.codeRefuse(w, r, route, key.authority.ID, "", err)
	})
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" ||
		!strings.Contains(w.Body.String(), `"code":"code_account_busy"`) || !strings.Contains(w.Body.String(), "Retry the same conversation") {
		t.Fatalf("busy response = %d, %v, %s", w.Code, w.Header(), w.Body)
	}
	if len(ledger.health) != 0 || len(ledger.cooldowns) != 0 || ledger.confirmations != 0 {
		t.Fatal("local saturation recorded an upstream failure")
	}
}

type notifyingResponseWriter struct {
	*httptest.ResponseRecorder
	written chan struct{}
	once    sync.Once
}

func (w *notifyingResponseWriter) Write(body []byte) (int, error) {
	n, err := w.ResponseRecorder.Write(body)
	w.once.Do(func() { close(w.written) })
	return n, err
}

type afterResponseReader struct {
	ready     <-chan struct{}
	premature chan<- struct{}
	reader    io.Reader
}

type stalledRejectionBody struct {
	prefix  io.Reader
	waiting chan struct{}
	closed  chan struct{}
	started sync.Once
	stopped sync.Once
}

func (r *stalledRejectionBody) Read(body []byte) (int, error) {
	if n, _ := r.prefix.Read(body); n > 0 {
		return n, nil
	}
	r.started.Do(func() { close(r.waiting) })
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *stalledRejectionBody) Close() error {
	r.stopped.Do(func() { close(r.closed) })
	return nil
}

func (r afterResponseReader) Read(body []byte) (int, error) {
	select {
	case <-r.ready:
		return r.reader.Read(body)
	default:
		select {
		case r.premature <- struct{}{}:
		default:
		}
		return 0, errors.New("response body read before client response")
	}
}

func TestTranslatedRejectionRespondsBeforeReadingUpstream(t *testing.T) {
	for _, status := range []int{429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ledger := &fakeCodeLedger{}
			attempt := codexAttempt(t, ledger, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
			attempt.dispatched = true
			writer := &notifyingResponseWriter{ResponseRecorder: httptest.NewRecorder(), written: make(chan struct{})}
			premature := make(chan struct{}, 1)
			body := afterResponseReader{ready: writer.written, premature: premature, reader: strings.NewReader(`{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`)}
			response := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(body)}
			attempt.response(t.Context(), status, response.Header)
			attempt.translateResponse(t.Context(), writer, httptest.NewRequest(http.MethodPost, "/", nil), response, "public-model", false, wire.Chat, false)
			attempt.finish(t.Context())
			select {
			case <-premature:
				t.Fatal("upstream body was read before the client received the rejection")
			default:
			}
			if writer.Code != status || !strings.Contains(writer.Body.String(), "code_upstream_rejected") {
				t.Fatalf("response = %d %s", writer.Code, writer.Body.String())
			}
			if status == 429 && (len(ledger.cooldowns) != 1 || ledger.cooldowns[0].classification.Class != codeadapter.CooldownCapacity) {
				t.Fatalf("cooldowns = %+v", ledger.cooldowns)
			}
			if status == 503 && len(ledger.health) != 1 {
				t.Fatalf("health = %+v", ledger.health)
			}
		})
	}
}

func TestTranslatedIncompleteRateLimitFinishesClientResponse(t *testing.T) {
	ledger := &fakeCodeLedger{}
	attempt := codexAttempt(t, ledger, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	attempt.dispatched = true
	body := &stalledRejectionBody{prefix: strings.NewReader(`{"error":{"type":"rate_limit_error"`), waiting: make(chan struct{}), closed: make(chan struct{})}
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: body}
		attempt.response(r.Context(), response.StatusCode, response.Header)
		attempt.translateResponse(r.Context(), w, r, response, "public-model", false, wire.Chat, false)
		attempt.finish(context.WithoutCancel(r.Context()))
		close(finished)
	}))
	defer server.Close()
	defer body.Close()
	type clientResult struct {
		status int
		length int64
		body   []byte
		err    error
	}
	result := make(chan clientResult, 1)
	go func() {
		response, err := server.Client().Get(server.URL)
		if err != nil {
			result <- clientResult{err: err}
			return
		}
		defer response.Body.Close()
		content, err := io.ReadAll(response.Body)
		result <- clientResult{status: response.StatusCode, length: response.ContentLength, body: content, err: err}
	}()
	<-body.waiting
	select {
	case got := <-result:
		if got.err != nil || got.status != 429 || got.length != int64(len(got.body)) || !json.Valid(got.body) || !bytes.Contains(got.body, []byte("code_upstream_rejected")) {
			t.Fatalf("client response = %+v", got)
		}
	case <-body.closed:
		t.Fatal("client response did not finish while the upstream body was stalled")
	}
	<-finished
	select {
	case <-body.closed:
	default:
		t.Fatal("stalled upstream body was not closed")
	}
	if len(ledger.cooldowns) != 0 || len(ledger.health) != 1 || ledger.health[0].Status != "quota_limited" || ledger.confirmations != 0 {
		t.Fatalf("cooldowns = %+v, health = %+v, confirmed = %d", ledger.cooldowns, ledger.health, ledger.confirmations)
	}
}

func TestWebSocketHandshakeRejectionStreamsBeforeReadingMore(t *testing.T) {
	ledger := &fakeCodeLedger{}
	attempt := codexAttempt(t, ledger, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	attempt.permit.Attempt.ID = ""
	writer := &notifyingResponseWriter{ResponseRecorder: httptest.NewRecorder(), written: make(chan struct{})}
	premature := make(chan struct{}, 1)
	body := io.MultiReader(strings.NewReader("first"), afterResponseReader{ready: writer.written, premature: premature, reader: strings.NewReader("second")})
	forwardCodeHandshakeRejection(t.Context(), writer, attempt, &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(body)})
	select {
	case <-premature:
		t.Fatal("upstream body was read further before forwarding the first bytes")
	default:
	}
	if writer.Code != 503 || writer.Body.String() != "firstsecond" {
		t.Fatalf("response = %d %q", writer.Code, writer.Body.String())
	}
}

func TestWebSocketHandshakeRejectionPreservesBodyAndQuota(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
			body := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":600}}`)
			header := http.Header{"Retry-After": {"60"}}
			if compressed {
				var encoded bytes.Buffer
				compressor := gzip.NewWriter(&encoded)
				if _, err := compressor.Write(body); err != nil {
					t.Fatal(err)
				}
				if err := compressor.Close(); err != nil {
					t.Fatal(err)
				}
				body = encoded.Bytes()
				header.Set("Content-Encoding", "gzip")
			}
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for name, values := range header {
					w.Header()[name] = values
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write(body)
			}))
			defer upstream.Close()
			ledger := &fakeCodeLedger{}
			attempt := codexAttempt(t, ledger, now)
			attempt.permit.Attempt.ID = ""
			writer := httptest.NewRecorder()
			base := http.DefaultTransport.(*http.Transport).Clone()
			base.DisableCompression = true
			defer base.CloseIdleConnections()
			transport := &codeHandshakeTransport{base: base, reject: func(response *http.Response) { forwardCodeHandshakeRejection(t.Context(), writer, attempt, response) }}
			_, response, err := websocket.Dial(t.Context(), upstream.URL, &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
			if err == nil || response == nil {
				t.Fatalf("dial response = %+v, error = %v", response, err)
			}
			attempt.flushPendingHealth(t.Context())
			if writer.Code != http.StatusTooManyRequests || !bytes.Equal(writer.Body.Bytes(), body) || writer.Header().Get("Content-Encoding") != header.Get("Content-Encoding") {
				t.Fatalf("forwarded response = %d %v, body differs: %t", writer.Code, writer.Header(), !bytes.Equal(writer.Body.Bytes(), body))
			}
			if len(ledger.cooldowns) != 1 || ledger.cooldowns[0].classification.Class != codeadapter.CooldownQuota || ledger.cooldowns[0].classification.Scope != codeadapter.ScopeCredential || !ledger.cooldowns[0].classification.RetryAt.Equal(now.Add(10*time.Minute)) {
				t.Fatalf("cooldowns = %+v, health = %+v", ledger.cooldowns, ledger.health)
			}
			if len(ledger.settlements) != 0 || ledger.confirmations != 0 {
				t.Fatalf("settlements = %+v, confirmations = %d", ledger.settlements, ledger.confirmations)
			}
		})
	}
}

func TestWebSocketHandshakeRejectionForwardsOversizedBody(t *testing.T) {
	body := bytes.Repeat([]byte("x"), codewire.MaxBody+1)
	ledger := &fakeCodeLedger{}
	attempt := codexAttempt(t, ledger, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	attempt.permit.Attempt.ID = ""
	writer := httptest.NewRecorder()
	forwardCodeHandshakeRejection(t.Context(), writer, attempt, &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))})
	if writer.Code != 429 || !bytes.Equal(writer.Body.Bytes(), body) || len(ledger.cooldowns) != 0 {
		t.Fatalf("forwarded status = %d, body length = %d, cooldowns = %+v", writer.Code, writer.Body.Len(), ledger.cooldowns)
	}
}

func TestWebSocketHandshakeCapacityCooldownUsesSelectedModel(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	body := `{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()
	route := codemode.Route{
		RevisionID: "revision", Models: []string{"gpt-model", "alternate-model"},
		ModelRewriting: true, Aliases: map[string]string{"public-model": "gpt-model"},
	}
	snapshot := &routing.Snapshot{Generation: routing.Generation{ID: uuid.NewString()}, CodeConnections: map[string]routing.Configuration{
		"revision:provider": {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID, Endpoint: upstream.URL}},
	}}
	inbound, ok := codeIngressFor("responses")
	if !ok {
		t.Fatal("responses ingress missing")
	}
	for _, tc := range []struct {
		name  string
		hint  string
		pin   codemode.Pin
		model string
	}{
		{name: "native", hint: "gpt-model", model: "gpt-model"},
		{name: "alias", hint: "public-model", model: "gpt-model"},
		{name: "pinned alias", hint: "public-model", pin: codemode.Pin{Model: "public-model", TargetModel: "alternate-model"}, model: "alternate-model"},
		{name: "no hint"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &fakeCodeLedger{connectionPermit: codesessions.Permit{
				Account: codemode.Account{ID: "account", ProviderID: "provider", Principal: "principal"},
				Pin:     codemode.Pin{Model: tc.pin.Model, TargetModel: tc.pin.TargetModel, Principal: "principal"},
			}}
			server := &Forwarder{
				gate: ingress.NewGate(nil, nil, nil, 0, func() time.Time { return now }),
				now:  func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				egress: &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}, ledger: ledger, authorizer: fakeCodeAuthorizer{},
			}
			request := httptest.NewRequest(http.MethodGet, "/responses", nil)
			request.Header.Set("X-OLP-Code-Affinity", "conversation")
			if tc.hint != "" {
				request.Header.Set("X-OLP-Code-Model", tc.hint)
			}
			writer := httptest.NewRecorder()
			snapshotRelease, err := release.NewRelease("", 0, snapshot, nil)
			if err != nil {
				t.Fatal(err)
			}
			server.codeWebSocket(writer, request, snapshotRelease, route, keyauthority.Authority{}, inbound)
			if writer.Code != http.StatusTooManyRequests || writer.Body.String() != body {
				t.Fatalf("response = %d %s", writer.Code, writer.Body.String())
			}
			if tc.model == "" {
				if len(ledger.cooldowns) != 0 || len(ledger.health) != 1 || ledger.health[0].Status != "quota_limited" {
					t.Fatalf("cooldowns = %+v, health = %+v", ledger.cooldowns, ledger.health)
				}
				return
			}
			if len(ledger.cooldowns) != 1 || len(ledger.health) != 0 {
				t.Fatalf("cooldowns = %+v, health = %+v", ledger.cooldowns, ledger.health)
			}
			cooldown := ledger.cooldowns[0]
			if cooldown.account != ledger.connectionPermit.Account.ID || cooldown.model != tc.model || cooldown.classification.Class != codeadapter.CooldownCapacity || cooldown.classification.Scope != codeadapter.ScopeModel || !cooldown.classification.RetryAt.Equal(now.Add(2*time.Minute)) {
				t.Fatalf("cooldown = %+v", cooldown)
			}
		})
	}
}

type fakeCodeAuthorizer struct{}

func (fakeCodeAuthorizer) AuthorizeCode(_ context.Context, _ routing.Configuration, account codemode.Account, _ codemode.Dispatch) (codemode.Authorization, error) {
	return codemode.Authorization{Principal: account.Principal, Headers: http.Header{"Authorization": {"Bearer fixture"}}}, nil
}

func TestTranslatedRejectionRecordsModelCooldown(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, family := range []wire.Format{wire.Chat, wire.Anthropic} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/compressed=%t", family, compressed), func(t *testing.T) {
				ledger := &fakeCodeLedger{}
				attempt := codexAttempt(t, ledger, now)
				attempt.dispatched = true
				body := []byte(`{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`)
				header := http.Header{"Retry-After": {"120"}}
				if compressed {
					var encoded bytes.Buffer
					compressor := gzip.NewWriter(&encoded)
					if _, err := compressor.Write(body); err != nil {
						t.Fatal(err)
					}
					if err := compressor.Close(); err != nil {
						t.Fatal(err)
					}
					body = encoded.Bytes()
					header.Set("Content-Encoding", "gzip")
				}
				response := &http.Response{StatusCode: 429, Header: header, Body: io.NopCloser(bytes.NewReader(body))}
				attempt.response(t.Context(), response.StatusCode, response.Header)
				writer := httptest.NewRecorder()
				attempt.translateResponse(t.Context(), writer, httptest.NewRequest(http.MethodPost, "/", nil), response, "public-model", false, family, false)
				attempt.finish(t.Context())
				if writer.Code != 429 || !strings.Contains(writer.Body.String(), "code_upstream_rejected") {
					t.Fatalf("response = %d %s", writer.Code, writer.Body.String())
				}
				if len(ledger.cooldowns) != 1 || ledger.cooldowns[0].model != "gpt-model" || ledger.cooldowns[0].classification.Class != codeadapter.CooldownCapacity || !ledger.cooldowns[0].classification.RetryAt.Equal(now.Add(2*time.Minute)) || len(ledger.health) != 0 {
					t.Fatalf("cooldowns = %+v, health = %+v", ledger.cooldowns, ledger.health)
				}
				if len(ledger.outcomes) != 1 || ledger.outcomes[0].Kind != "rejected" || len(ledger.settlements) != 1 || ledger.settlements[0].Total != nil || ledger.confirmations != 0 {
					t.Fatalf("outcomes = %+v, settlements = %+v, confirmations = %d", ledger.outcomes, ledger.settlements, ledger.confirmations)
				}
			})
		}
	}
}

type fakeCodeLedger struct {
	connectionPermit codesessions.Permit
	health           []codemode.HealthObservation
	cooldowns        []recordedCooldown
	outcomes         []codemode.Outcome
	settlements      []codemode.Usage
	confirmations    int
	cooldownErr      error
	threadStatus     codemode.ThreadStatus
	threadErr        error
	threadIdentity   codemode.Identity
	threadKey        string
	threadCalls      int
}

func TestCodeAttemptChatTracksEveryChoice(t *testing.T) {
	for _, tc := range []struct {
		name       string
		count      int64
		choices    []string
		stream     bool
		done       bool
		incomplete bool
		unknown    bool
	}{
		{name: "single choice at EOF", count: 1, choices: []string{`{"index":0,"finish_reason":"stop"}`}, stream: true},
		{name: "single choice without index", count: 1, choices: []string{`{"finish_reason":"stop"}`}, stream: true},
		{name: "interleaved completion at EOF", choices: []string{`{"index":0,"finish_reason":null},{"index":1,"finish_reason":null}`, `{"index":1,"finish_reason":"stop"}`, `{"index":0,"finish_reason":"tool_calls"}`}, stream: true},
		{name: "unfinished second choice at EOF", choices: []string{`{"index":1,"finish_reason":null}`, `{"index":0,"finish_reason":"stop"}`}, stream: true, incomplete: true},
		{name: "unfinished choice with DONE", choices: []string{`{"index":0,"finish_reason":"stop"},{"index":1,"finish_reason":null}`}, stream: true, done: true, incomplete: true},
		{name: "stream length before stop", choices: []string{`{"index":0,"finish_reason":"length"}`, `{"index":1,"finish_reason":"stop"}`}, stream: true, done: true, incomplete: true},
		{name: "stream length after stop", choices: []string{`{"index":1,"finish_reason":"stop"}`, `{"index":0,"finish_reason":"length"}`}, stream: true, incomplete: true},
		{name: "unary length before stop", choices: []string{`{"index":0,"finish_reason":"length"},{"index":1,"finish_reason":"stop"}`}, incomplete: true},
		{name: "unary length after stop", choices: []string{`{"index":1,"finish_reason":"stop"},{"index":0,"finish_reason":"length"}`}, incomplete: true},
		{name: "unseen second choice at EOF", choices: []string{`{"index":0,"finish_reason":"stop"}`}, stream: true, incomplete: true},
		{name: "unseen second choice with DONE", choices: []string{`{"index":0,"finish_reason":"stop"}`}, stream: true, done: true, incomplete: true},
		{name: "unseen second choice in unary", choices: []string{`{"index":0,"finish_reason":"stop"}`}, incomplete: true},
		{name: "conflicting length then stop", count: 1, choices: []string{`{"index":0,"finish_reason":"length"}`, `{"index":0,"finish_reason":"stop"}`}, stream: true, incomplete: true},
		{name: "conflicting stop then length", count: 1, choices: []string{`{"index":0,"finish_reason":"stop"}`, `{"index":0,"finish_reason":"length"}`}, stream: true, incomplete: true},
		{name: "wrong second index", choices: []string{`{"index":1,"finish_reason":"stop"},{"index":2,"finish_reason":"stop"}`}, incomplete: true},
		{name: "negative index", count: 1, choices: []string{`{"index":-1,"finish_reason":"stop"}`}, incomplete: true},
		{name: "fractional index", count: 1, choices: []string{`{"index":0.5,"finish_reason":"stop"}`}, incomplete: true},
		{name: "missing multi-choice indices", choices: []string{`{"finish_reason":"stop"},{"finish_reason":"stop"}`}, incomplete: true},
		{name: "missing multi-choice stream indices", choices: []string{`{"finish_reason":"stop"}`, `{"finish_reason":"stop"}`}, stream: true, done: true, incomplete: true},
		{name: "usage only at EOF", count: 1, stream: true, incomplete: true},
		{name: "empty stream at EOF", count: 1, stream: true, incomplete: true, unknown: true},
		{name: "no choices with DONE", count: 1, stream: true, done: true, incomplete: true},
		{name: "unary complete", choices: []string{`{"index":1,"finish_reason":"stop"},{"index":0,"finish_reason":"stop"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &fakeCodeLedger{}
			attempt := glmAttempt(t, ledger, time.Now(), "v1/chat/completions")
			choices := int64(2)
			if tc.count != 0 {
				choices = tc.count
			}
			request, err := codewire.ClassifyChat(fmt.Appendf(nil, `{"model":"glm-model","n":%d,"messages":[{"role":"user","content":"fixture"}]}`, choices), http.Header{})
			if err != nil {
				t.Fatal(err)
			}
			attempt.observer = codeObserver(attempt.endpoint, request.ChatChoices)
			attempt.dispatched = true
			body := func() string {
				if !tc.stream {
					return `{"choices":[` + tc.choices[0] + `],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`
				}
				var stream strings.Builder
				for _, choices := range tc.choices {
					fmt.Fprintf(&stream, "data: {\"choices\":[%s]}\n\n", choices)
				}
				if !tc.unknown {
					stream.WriteString("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n")
				}
				if tc.done {
					stream.WriteString("data: [DONE]\n\n")
				}
				return stream.String()
			}()
			header := http.Header{}
			if tc.stream {
				header.Set("Content-Type", "text/event-stream")
			}
			attempt.response(t.Context(), http.StatusOK, header)
			writer := httptest.NewRecorder()
			attempt.copyResponse(t.Context(), writer, &http.Response{Header: header, Body: io.NopCloser(strings.NewReader(body))})
			attempt.finish(t.Context())
			if writer.Body.String() != body {
				t.Fatalf("forwarded response = %q, want %q", writer.Body.String(), body)
			}
			want := "completed"
			if tc.incomplete {
				want = "incomplete"
			}
			if len(ledger.outcomes) != 2 || ledger.outcomes[1].Origin != "upstream" || ledger.outcomes[1].Kind != want {
				t.Fatalf("outcomes = %+v, want upstream %s", ledger.outcomes, want)
			}
			if tc.incomplete && len(ledger.health) != 0 || !tc.incomplete && (len(ledger.health) != 1 || ledger.health[0].Status != "healthy") {
				t.Fatalf("health = %+v, incomplete = %t", ledger.health, tc.incomplete)
			}
			if len(ledger.settlements) != 1 {
				t.Fatalf("settlements = %+v", ledger.settlements)
			}
			if total := ledger.settlements[0].Total; tc.unknown && total != nil || !tc.unknown && (total == nil || *total != 5) {
				t.Fatalf("reported usage lost, settlements = %+v", ledger.settlements)
			}
		})
	}
}

func (f *fakeCodeLedger) Models(context.Context, codemode.Route, string) (codeclient.Offers, error) {
	return codeclient.Offers{}, nil
}
func (f *fakeCodeLedger) ThreadStatus(_ context.Context, _ codemode.Route, key string, identity codemode.Identity, _ time.Time) (codemode.ThreadStatus, error) {
	f.threadCalls++
	f.threadKey = key
	f.threadIdentity = identity
	return f.threadStatus, f.threadErr
}
func (f *fakeCodeLedger) Admit(context.Context, codesessions.Admission) (codesessions.Permit, error) {
	return codesessions.Permit{}, nil
}
func (f *fakeCodeLedger) BindConnection(context.Context, codemode.Route, string, codemode.Identity, string, []string) (codesessions.Permit, error) {
	return f.connectionPermit, nil
}
func (f *fakeCodeLedger) MarkDispatched(context.Context, string) error { return nil }
func (f *fakeCodeLedger) ConfirmNonexecution(context.Context, string, int) error {
	f.confirmations++
	return nil
}
func (f *fakeCodeLedger) ObserveCooldown(context.Context, string, time.Time) error { return nil }
func (f *fakeCodeLedger) ObserveCooldownClass(_ context.Context, account, model string, classification codeadapter.Classification, _ time.Time) error {
	if classification.Scope == codeadapter.ScopeModel && model == "" {
		return codemode.Refuse(400, "code_cooldown_invalid")
	}
	if f.cooldownErr != nil {
		return f.cooldownErr
	}
	f.cooldowns = append(f.cooldowns, recordedCooldown{account: account, model: model, classification: classification})
	return nil
}
func (f *fakeCodeLedger) Settle(_ context.Context, _ string, usage codemode.Usage) error {
	f.settlements = append(f.settlements, usage)
	return nil
}
func (f *fakeCodeLedger) Abort(context.Context, string) error { return nil }
func (f *fakeCodeLedger) ObserveHealth(_ context.Context, _ string, observation codemode.HealthObservation) error {
	f.health = append(f.health, observation)
	return nil
}
func (f *fakeCodeLedger) ObserveAllowance(context.Context, string, codemode.Allowance) error {
	return nil
}
func (f *fakeCodeLedger) ObserveAllowanceCharge(context.Context, string, json.RawMessage) error {
	return nil
}
func (f *fakeCodeLedger) ObserveReference(context.Context, string, string) error { return nil }
func (f *fakeCodeLedger) ObserveOutcome(_ context.Context, _ string, outcome codemode.Outcome) error {
	f.outcomes = append(f.outcomes, outcome)
	return nil
}
func (f *fakeCodeLedger) RecordRefusal(context.Context, codemode.Route, string, string, string) error {
	return nil
}
func (f *fakeCodeLedger) ResolvePaidFallback(context.Context, codesessions.Admission, keyauthority.Authority) (*codemode.Operation, error) {
	return nil, nil
}
func (f *fakeCodeLedger) ResolvePaidModel(context.Context, codesessions.Admission, keyauthority.Authority) (codemode.Operation, error) {
	return codemode.Operation{}, errors.New("unexpected paid model resolution")
}
func (f *fakeCodeLedger) AdmitPaid(context.Context, codesessions.PaidAdmission) (codesessions.PaidPermit, error) {
	return codesessions.PaidPermit{}, errors.New("unexpected paid admission")
}

func codexAttempt(t *testing.T, ledger *fakeCodeLedger, now time.Time) *codeAttempt {
	t.Helper()
	vendor, ok := codeadapter.Lookup(codemode.AdapterCodex)
	if !ok {
		t.Fatal("codex vendor missing")
	}
	endpoint, ok := vendor.Endpoint("responses")
	if !ok {
		t.Fatal("codex responses endpoint missing")
	}
	server := &Forwarder{now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ledger: ledger}
	return &codeAttempt{
		forwarder:   server,
		permit:      codesessions.Permit{Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID}},
		endpoint:    endpoint,
		observer:    codeObserver(endpoint, 1),
		model:       "gpt-model",
		publicModel: "public-model",
	}
}

func TestCodeAttemptUnaryCapacity429WritesOnlyModelCooldown(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ledger := &fakeCodeLedger{}
	attempt := codexAttempt(t, ledger, now)
	attempt.response(t.Context(), 429, http.Header{"Retry-After": {"120"}})
	if len(ledger.health) != 0 || len(ledger.cooldowns) != 0 {
		t.Fatalf("headers wrote health %v cooldowns %v before body classification", ledger.health, ledger.cooldowns)
	}
	attempt.observe(t.Context(), attempt.observer.Unary([]byte(`{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`)))
	attempt.finish(t.Context())
	if len(ledger.health) != 0 {
		t.Fatalf("capacity 429 wrote credential health %v, want none", ledger.health)
	}
	if len(ledger.cooldowns) != 1 {
		t.Fatalf("cooldowns = %+v, want one model cooldown", ledger.cooldowns)
	}
	got := ledger.cooldowns[0]
	if got.account != "account" || got.model != "gpt-model" || got.classification.Class != codeadapter.CooldownCapacity || got.classification.Scope != codeadapter.ScopeModel || !got.classification.RetryAt.Equal(now.Add(120*time.Second)) {
		t.Fatalf("cooldown = %+v", got)
	}
}

func TestCodeAttemptRecoveryRecordsQuotaCooldown(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		cooldownErr error
	}{
		{name: "records cooldown"},
		{name: "does not replay when cooldown storage fails", cooldownErr: errors.New("unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ledger := &fakeCodeLedger{cooldownErr: tc.cooldownErr}
			attempt := codexAttempt(t, ledger, now)
			attempt.permit.InitialRecovery = true
			attempt.dispatched = true
			response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached","resets_in_seconds":600}}`))}
			attempt.response(t.Context(), response.StatusCode, response.Header)
			recovery, err := attempt.recover(t.Context(), response, codemode.Route{InitialRecoveryAttempts: 1}, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if tc.cooldownErr != nil {
				if recovery != nil || len(ledger.health) != 1 {
					t.Fatalf("failed cooldown write = recovery %+v, health %+v", recovery, ledger.health)
				}
				return
			}
			if recovery == nil || len(ledger.cooldowns) != 1 || ledger.cooldowns[0].classification.Class != codeadapter.CooldownQuota || ledger.cooldowns[0].classification.Scope != codeadapter.ScopeCredential || !ledger.cooldowns[0].classification.RetryAt.Equal(now.Add(10*time.Minute)) || len(ledger.health) != 0 {
				t.Fatalf("recovery = %+v, cooldowns %+v, health %+v", recovery, ledger.cooldowns, ledger.health)
			}
		})
	}
}

type refusalRuntime struct{ Runtime }

func (refusalRuntime) CredentialRefused(string, int64) {}

func TestTranslatedRecoveryBoundsStalledRejection(t *testing.T) {
	for _, status := range []int{401, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ledger := &fakeCodeLedger{}
			attempt := codexAttempt(t, ledger, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
			attempt.permit.InitialRecovery = true
			attempt.forwarder.runtime = refusalRuntime{}
			attempt.dispatched = true
			body := &stalledRejectionBody{prefix: strings.NewReader(`{"error":`), waiting: make(chan struct{}), closed: make(chan struct{})}
			defer body.Close()
			response := &http.Response{StatusCode: status, Header: http.Header{}, Body: body}
			attempt.response(t.Context(), status, response.Header)
			result := make(chan *codesessions.Recovery, 1)
			go func() {
				recovery, _ := attempt.recover(t.Context(), response, codemode.Route{InitialRecoveryAttempts: 1}, nil, true)
				result <- recovery
			}()
			<-body.waiting
			select {
			case <-body.closed:
			case <-time.After(3 * time.Second):
				_ = body.Close()
				<-result
				t.Fatal("translated recovery waited for a stalled rejection")
			}
			if recovery := <-result; recovery != nil {
				t.Fatalf("incomplete rejection replayed: %+v", recovery)
			}
			writer := httptest.NewRecorder()
			attempt.translateResponse(t.Context(), writer, httptest.NewRequest(http.MethodPost, "/", nil), response, "public-model", false, wire.Chat, false)
			attempt.finish(t.Context())
			if writer.Code != status || !strings.Contains(writer.Body.String(), "code_upstream_rejected") || ledger.confirmations != 0 || len(ledger.cooldowns) != 0 || len(ledger.health) != 1 {
				t.Fatalf("response = %d %s, confirmations = %d, cooldowns = %+v, health = %+v", writer.Code, writer.Body.String(), ledger.confirmations, ledger.cooldowns, ledger.health)
			}
		})
	}
}

func TestTranslatedRecoveryKeepsProvenReplay(t *testing.T) {
	ledger := &fakeCodeLedger{}
	attempt := codexAttempt(t, ledger, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	attempt.permit.InitialRecovery = true
	attempt.dispatched = true
	response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"usage_limit_reached","resets_in_seconds":600}}`))}
	attempt.response(t.Context(), response.StatusCode, response.Header)
	recovery, err := attempt.recover(t.Context(), response, codemode.Route{InitialRecoveryAttempts: 1}, nil, true)
	if err != nil || recovery == nil || ledger.confirmations != 1 || len(ledger.cooldowns) != 1 || ledger.cooldowns[0].classification.Class != codeadapter.CooldownQuota {
		t.Fatalf("recovery = %+v, error = %v, confirmations = %d, cooldowns = %+v", recovery, err, ledger.confirmations, ledger.cooldowns)
	}
}

func TestRecoveryPreservesUntranslatedRejectionBody(t *testing.T) {
	ledger := &fakeCodeLedger{}
	attempt := codexAttempt(t, ledger, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	attempt.permit.InitialRecovery = true
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: reader}
	prefix := `{"error":{"type":"rate_limit_error",`
	suffix := `"code":"unrecognized"}}`
	written := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_, _ = io.WriteString(writer, prefix)
		close(written)
		<-release
		_, _ = io.WriteString(writer, suffix)
		_ = writer.Close()
	}()
	result := make(chan *codesessions.Recovery, 1)
	go func() {
		recovery, _ := attempt.recover(t.Context(), response, codemode.Route{InitialRecoveryAttempts: 1}, nil, false)
		result <- recovery
	}()
	<-written
	close(release)
	if recovery := <-result; recovery != nil {
		t.Fatalf("unproven rejection replayed: %+v", recovery)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != prefix+suffix || ledger.confirmations != 0 {
		t.Fatalf("forwarded body = %q, error = %v, confirmations = %d", body, err, ledger.confirmations)
	}
}

func TestCodeAttemptUnaryUnclassified429KeepsCredentialFallback(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ledger := &fakeCodeLedger{}
	attempt := codexAttempt(t, ledger, now)
	attempt.response(t.Context(), 429, http.Header{"Retry-After": {"120"}})
	attempt.observe(t.Context(), attempt.observer.Unary([]byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`)))
	attempt.finish(t.Context())
	if len(ledger.cooldowns) != 0 {
		t.Fatalf("unclassified 429 wrote cooldowns %+v", ledger.cooldowns)
	}
	if len(ledger.health) != 1 || ledger.health[0].Status != "quota_limited" || ledger.health[0].RetryAt == nil || !ledger.health[0].RetryAt.Equal(now.Add(120*time.Second)) {
		t.Fatalf("health = %+v, want one quota_limited at retry-after", ledger.health)
	}
}

func TestCodeAttemptFailedCooldownWriteKeepsCredentialFallback(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ledger := &fakeCodeLedger{cooldownErr: errors.New("store unavailable")}
	attempt := codexAttempt(t, ledger, now)
	attempt.response(t.Context(), 429, http.Header{"Retry-After": {"120"}})
	attempt.observe(t.Context(), attempt.observer.Unary([]byte(`{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`)))
	attempt.finish(t.Context())
	if len(ledger.health) != 1 || ledger.health[0].Status != "quota_limited" {
		t.Fatalf("health = %+v, want credential fallback after failed model cooldown write", ledger.health)
	}
}

func TestCodeAttemptUnobserved429FlushesCredentialHealthAtFinish(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ledger := &fakeCodeLedger{}
	attempt := codexAttempt(t, ledger, now)
	attempt.response(t.Context(), 429, http.Header{"Retry-After": {"120"}})
	attempt.finish(t.Context())
	if len(ledger.health) != 1 || ledger.health[0].Status != "quota_limited" || ledger.health[0].RetryAt == nil || !ledger.health[0].RetryAt.Equal(now.Add(120*time.Second)) {
		t.Fatalf("health = %+v, want finish flush", ledger.health)
	}
}

func TestCodeAttemptObserveLogsLedgerFailuresAndPropagatesContextValues(t *testing.T) {
	type traceKey struct{}
	var logs bytes.Buffer
	var seenValue any
	ledger := &contextCapturingLedger{
		err: errors.New("ledger down"),
		onCall: func(ctx context.Context) {
			seenValue = ctx.Value(traceKey{})
		},
	}
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	attempt := &codeAttempt{
		forwarder: &Forwarder{
			now:    func() time.Time { return now },
			log:    slog.New(slog.NewTextHandler(&logs, nil)),
			ledger: ledger,
		},
		permit: codesessions.Permit{
			Account: codemode.Account{ID: "acct-1"},
			Attempt: codemode.Attempt{ID: "att-1"},
		},
	}
	ctx := context.WithValue(t.Context(), traceKey{}, "req-42")
	attempt.observe(ctx, codemode.Observation{ResponseID: "resp-1", Successful: true})
	if seenValue != "req-42" {
		t.Fatalf("ledger ctx value = %v, want req-42", seenValue)
	}
	if !strings.Contains(logs.String(), "code reference unavailable") || !strings.Contains(logs.String(), "code health unavailable") {
		t.Fatalf("expected ledger warnings in logs, got %q", logs.String())
	}
}

type contextCapturingLedger struct {
	Ledger
	err    error
	onCall func(context.Context)
}

func (l *contextCapturingLedger) ObserveReference(ctx context.Context, _ string, _ string) error {
	l.onCall(ctx)
	return l.err
}

func (l *contextCapturingLedger) ObserveHealth(ctx context.Context, _ string, _ codemode.HealthObservation) error {
	l.onCall(ctx)
	return l.err
}

func TestCodeProviderDemandHoldsTheReplyBoundAsOutput(t *testing.T) {
	t.Parallel()
	const prompt = `"messages":[{"role":"user","content":"fixture"}]`
	for name, tc := range map[string]struct {
		classify func([]byte, http.Header) (codemode.Request, error)
		body     string
		want     int64
	}{
		"messages cap":     {classify: codewire.ClassifyMessages, body: `{"model":"m","max_tokens":300,` + prompt + `}`, want: 300},
		"chat cap":         {classify: codewire.ClassifyChat, body: `{"model":"m","max_completion_tokens":200,` + prompt + `}`, want: 200},
		"chat without cap": {classify: codewire.ClassifyChat, body: `{"model":"m",` + prompt + `}`, want: estimate.DefaultOutputTokens},
		"chat choices":     {classify: codewire.ClassifyChat, body: `{"model":"m","n":3,"max_completion_tokens":200,` + prompt + `}`, want: 600},
		"chat choices without cap": {
			classify: codewire.ClassifyChat, body: `{"model":"m","n":2,` + prompt + `}`, want: 2 * estimate.DefaultOutputTokens,
		},
		"responses cap": {
			classify: func(body []byte, headers http.Header) (codemode.Request, error) {
				return codexwire.Classify(body, headers, "responses", false)
			},
			body: `{"model":"m","max_output_tokens":150,"input":"fixture"}`, want: 150,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			request, err := tc.classify([]byte(tc.body), http.Header{})
			if err != nil {
				t.Fatal(err)
			}
			demand := codeProviderDemand(request)
			if demand.Output != tc.want || demand.Input < 1 || demand.Total != demand.Input+demand.Output {
				t.Fatalf("demand = %+v, want %d output tokens beside the sized input", demand, tc.want)
			}
		})
	}
}

func TestSettledCodeUsageChargesEachTokenDimension(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		usage codemode.Usage
		want  *admission.TokenUsage
	}{
		"nothing reported": {},
		"total only":       {usage: codemode.Usage{Total: new(int64(15))}, want: &admission.TokenUsage{Total: 15}},
		"input and output": {
			usage: codemode.Usage{Total: new(int64(15)), Input: new(int64(10)), Output: new(int64(5))},
			want:  &admission.TokenUsage{Input: 10, Output: 5, Total: 15, Credit: true},
		},
		"cached input": {
			usage: codemode.Usage{Total: new(int64(1025)), Input: new(int64(1000)), Output: new(int64(25)), Cached: new(int64(800))},
			want:  &admission.TokenUsage{Input: 200, Output: 25, Total: 1025, Credit: true},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := settledCodeUsage(tc.usage)
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("settledCodeUsage = %+v, want %+v", got, tc.want)
			}
		})
	}
}
