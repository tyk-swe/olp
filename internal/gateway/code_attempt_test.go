package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/providerconfig"
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
	ingress, ok := codeIngressFor("responses")
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
			server := &Server{

				now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				egress: &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}, deps: Dependencies{CodeLedger: ledger, CodeAuthorizer: fakeCodeAuthorizer{}},
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
			server.codeWebSocket(writer, request, snapshotRelease, route, keyauthority.Authority{}, ingress)
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
}

func (f *fakeCodeLedger) Models(context.Context, codemode.Route, string) (codeclient.Offers, error) {
	return codeclient.Offers{}, nil
}
func (f *fakeCodeLedger) Admit(context.Context, codesessions.Admission) (codesessions.Permit, error) {
	return f.connectionPermit, nil
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
	server := &Server{now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), deps: Dependencies{CodeLedger: ledger}}
	return &codeAttempt{
		server:      server,
		permit:      codesessions.Permit{Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID}},
		endpoint:    endpoint,
		observer:    codeObserver(endpoint),
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
	attempt.observe(attempt.observer.Unary([]byte(`{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`)))
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

func TestTranslatedRecoveryBoundsStalledRejection(t *testing.T) {
	for _, status := range []int{401, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ledger := &fakeCodeLedger{}
			attempt := codexAttempt(t, ledger, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
			attempt.permit.InitialRecovery = true
			attempt.server.deps.Runtime = &fakeRuntime{}
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
	attempt.observe(attempt.observer.Unary([]byte(`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded"}}`)))
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
	attempt.observe(attempt.observer.Unary([]byte(`{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`)))
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

func glmAttempt(t *testing.T, ledger *fakeCodeLedger, now time.Time, path string) *codeAttempt {
	t.Helper()
	vendor, ok := codeadapter.Lookup(codemode.AdapterZAICoding)
	if !ok {
		t.Fatal("glm vendor missing")
	}
	endpoint, ok := vendor.Endpoint(path)
	if !ok {
		t.Fatal("glm endpoint missing")
	}
	server := &Server{now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), deps: Dependencies{CodeLedger: ledger}}
	return &codeAttempt{
		server:      server,
		permit:      codesessions.Permit{InitialRecovery: true, Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.ZAIProfile}},
		endpoint:    endpoint,
		observer:    codeObserver(endpoint),
		model:       "glm-model",
		publicModel: "glm-model",
	}
}

func TestCodeAttemptGLMMessagesQuota429WritesCredentialCooldownWithVendorReset(t *testing.T) {
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), code 1316.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	ledger := &fakeCodeLedger{}
	attempt := glmAttempt(t, ledger, now, "v1/messages")
	attempt.response(t.Context(), 429, http.Header{"Retry-After": {"120"}})
	attempt.observe(attempt.observer.Unary([]byte(`{"error":{"code":"1316","message":"Usage limit reached for the past 5 hours. Insufficient balance for extra usage. Resets at ` + reset.Format(time.RFC3339) + `."}}`)))
	attempt.finish(t.Context())
	if len(ledger.cooldowns) != 1 {
		t.Fatalf("cooldowns = %+v, want one credential cooldown", ledger.cooldowns)
	}
	got := ledger.cooldowns[0]
	if got.account != "account" || got.classification.Class != codeadapter.CooldownQuota || got.classification.Scope != codeadapter.ScopeCredential || got.classification.Envelope != "glm_1316" || !got.classification.RetryAt.Equal(reset) {
		t.Fatalf("cooldown = %+v", got)
	}
}

func TestCodeAttemptGLMExpiry429WritesNoHealthOrCooldown(t *testing.T) {
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), code 1309.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ledger := &fakeCodeLedger{}
	attempt := glmAttempt(t, ledger, now, "v1/chat/completions")
	attempt.response(t.Context(), 429, http.Header{"Retry-After": {"120"}})
	attempt.observe(attempt.observer.Unary([]byte(`{"error":{"code":"1309","message":"Your GLM Coding Plan package has expired"}}`)))
	attempt.finish(t.Context())
	if len(ledger.health) != 0 {
		t.Fatalf("expiry 429 wrote health %+v, want none", ledger.health)
	}
	if len(ledger.cooldowns) != 0 {
		t.Fatalf("expiry 429 wrote cooldowns %+v, want none", ledger.cooldowns)
	}
}

func TestCodeAttemptGLMQuotaDoesNotRecoverWithoutNonexecutionProof(t *testing.T) {
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), code 1308.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	route := codemode.Route{InitialRecoveryAttempts: 1}
	proven := glmAttempt(t, &fakeCodeLedger{}, now, "v1/chat/completions")
	response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"1308","message":"Usage limit reached for 5 hours. Your limit will reset at ` + reset.Format(time.RFC3339) + `"}}`))}
	recovery, err := proven.recover(t.Context(), response, route, nil, false)
	if err != nil || recovery != nil {
		t.Fatalf("recover(quota) = %+v, %v, want no replay", recovery, err)
	}
	ambiguous := glmAttempt(t, &fakeCodeLedger{}, now, "v1/chat/completions")
	response = &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"1308","message":"Usage limit reached for 5 hours. Your limit will reset at ` + reset.Format(time.RFC3339) + `"},"extra":1}`))}
	recovery, err = ambiguous.recover(t.Context(), response, route, nil, false)
	if err != nil || recovery != nil {
		t.Fatalf("recover(ambiguous) = %+v, %v, want no replay", recovery, err)
	}
}

func qwenAttempt(t *testing.T, ledger *fakeCodeLedger, now time.Time, anchor *string) *codeAttempt {
	t.Helper()
	vendor, ok := codeadapter.Lookup(codemode.AdapterQwenTokenPlan)
	if !ok {
		t.Fatal("qwen vendor missing")
	}
	endpoint, ok := vendor.Endpoint("v1/messages")
	if !ok {
		t.Fatal("qwen messages endpoint missing")
	}
	server := &Server{now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), deps: Dependencies{CodeLedger: ledger}}
	return &codeAttempt{
		server:      server,
		permit:      codesessions.Permit{InitialRecovery: true, Account: codemode.Account{ID: "account", CycleAnchor: anchor}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.QwenTokenPlanProfile}},
		endpoint:    endpoint,
		observer:    codeObserver(endpoint),
		model:       "qwen-model",
		publicModel: "qwen-model",
	}
}

func TestCodeAttemptQwenQuotaCooldownUsesCycleAnchorReset(t *testing.T) {
	// Source: https://www.alibabacloud.com/help/en/model-studio/token-plan-team-faq (fetched 2026-11-05), 429 insufficient_quota resets next subscription month.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	anchor := "2026-01-15"
	ledger := &fakeCodeLedger{}
	attempt := qwenAttempt(t, ledger, now, &anchor)
	attempt.response(t.Context(), 429, http.Header{})
	attempt.observe(attempt.observer.Unary([]byte(`{"error":{"code":"insufficient_quota","message":"Your token-plan quota has been exhausted."}}`)))
	attempt.finish(t.Context())
	if len(ledger.cooldowns) != 1 {
		t.Fatalf("cooldowns = %+v, want one credential cooldown", ledger.cooldowns)
	}
	got := ledger.cooldowns[0]
	want := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	if got.classification.Envelope != "qwen_insufficient_quota" || got.classification.Class != codeadapter.CooldownQuota || !got.classification.RetryAt.Equal(want) {
		t.Fatalf("cooldown = %+v, want retry at %s", got, want)
	}
}

func TestCodeAttemptQwenQuotaCooldownShortVendorRetryAfterDoesNotShortenAnchorReset(t *testing.T) {
	// Source: https://www.alibabacloud.com/help/en/model-studio/token-plan-team-faq (fetched 2026-11-05), 429 insufficient_quota resets next subscription month, so a shorter vendor Retry-After never shortens the monthly reset.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	anchor := "2026-01-15"
	want := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	ledger := &fakeCodeLedger{}
	attempt := qwenAttempt(t, ledger, now, &anchor)
	attempt.response(t.Context(), 429, http.Header{"Retry-After": {"3600"}})
	attempt.observe(attempt.observer.Unary([]byte(`{"error":{"code":"insufficient_quota","message":"Your token-plan quota has been exhausted."}}`)))
	attempt.finish(t.Context())
	if len(ledger.cooldowns) != 1 || !ledger.cooldowns[0].classification.RetryAt.Equal(want) {
		t.Fatalf("cooldowns = %+v, want anchor reset %s over one hour retry-after", ledger.cooldowns, want)
	}
	longLedger := &fakeCodeLedger{}
	longAttempt := qwenAttempt(t, longLedger, now, &anchor)
	longAttempt.response(t.Context(), 429, http.Header{"Retry-After": {"2592000"}})
	longAttempt.observe(longAttempt.observer.Unary([]byte(`{"error":{"code":"insufficient_quota","message":"Your token-plan quota has been exhausted."}}`)))
	longAttempt.finish(t.Context())
	if len(longLedger.cooldowns) != 1 || !longLedger.cooldowns[0].classification.RetryAt.Equal(now.Add(30*24*time.Hour)) {
		t.Fatalf("cooldowns = %+v, want later vendor retry-after over anchor reset", longLedger.cooldowns)
	}
}

func TestCodeAttemptQwenQuotaCooldownWithoutAnchorKeepsFallback(t *testing.T) {
	// Source: https://www.alibabacloud.com/help/en/model-studio/token-plan-team-faq (fetched 2026-11-05), 429 insufficient_quota.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ledger := &fakeCodeLedger{}
	attempt := qwenAttempt(t, ledger, now, nil)
	attempt.response(t.Context(), 429, http.Header{})
	attempt.observe(attempt.observer.Unary([]byte(`{"error":{"code":"insufficient_quota","message":"Your token-plan quota has been exhausted."}}`)))
	attempt.finish(t.Context())
	if len(ledger.cooldowns) != 1 || !ledger.cooldowns[0].classification.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("cooldowns = %+v, want one minute fallback without anchor", ledger.cooldowns)
	}
}

func minimaxAttempt(t *testing.T, ledger *fakeCodeLedger, now time.Time, path string) *codeAttempt {
	t.Helper()
	vendor, ok := codeadapter.Lookup(codemode.AdapterMiniMaxPlan)
	if !ok {
		t.Fatal("minimax vendor missing")
	}
	endpoint, ok := vendor.Endpoint(path)
	if !ok {
		t.Fatal("minimax endpoint missing")
	}
	server := &Server{now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), deps: Dependencies{CodeLedger: ledger}}
	return &codeAttempt{
		server:      server,
		permit:      codesessions.Permit{InitialRecovery: true, Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.MiniMaxProfile}},
		endpoint:    endpoint,
		observer:    codeObserver(endpoint),
		model:       "MiniMax-M3",
		publicModel: "MiniMax-M3",
	}
}

func TestCodeAttemptMiniMaxMessagesUsage429WritesCredentialQuotaCooldown(t *testing.T) {
	// Sources: https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05), code 2056, and https://platform.minimax.io/docs/api-reference/speech-to-text (fetched 2026-11-05), OaiError envelope.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ledger := &fakeCodeLedger{}
	attempt := minimaxAttempt(t, ledger, now, "v1/messages")
	attempt.response(t.Context(), 429, http.Header{"Retry-After": {"120"}})
	attempt.observe(attempt.observer.Unary([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)","http_code":"429"},"request_id":"r"}`)))
	attempt.finish(t.Context())
	if len(ledger.cooldowns) != 1 {
		t.Fatalf("cooldowns = %+v, want one credential cooldown", ledger.cooldowns)
	}
	got := ledger.cooldowns[0]
	if got.account != "account" || got.classification.Class != codeadapter.CooldownQuota || got.classification.Scope != codeadapter.ScopeCredential || got.classification.Envelope != "minimax_2056" || !got.classification.RetryAt.Equal(now.Add(120*time.Second)) {
		t.Fatalf("cooldown = %+v", got)
	}
}

func TestCodeAttemptMiniMaxChatAmbiguous2062WritesShortRateCooldownOnly(t *testing.T) {
	// Source: https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05), code 2062 is ambiguous and never quota.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ledger := &fakeCodeLedger{}
	attempt := minimaxAttempt(t, ledger, now, "v1/chat/completions")
	attempt.response(t.Context(), 429, http.Header{})
	attempt.observe(attempt.observer.Unary([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"M Plan rate limit reached (2062)","http_code":"429"},"request_id":"r"}`)))
	attempt.finish(t.Context())
	if len(ledger.cooldowns) != 1 {
		t.Fatalf("cooldowns = %+v, want one rate cooldown", ledger.cooldowns)
	}
	got := ledger.cooldowns[0]
	if got.classification.Class != codeadapter.CooldownRate || got.classification.Class == codeadapter.CooldownQuota || got.classification.Scope != codeadapter.ScopeCredential || got.classification.Envelope != "minimax_2062" || got.classification.NotExecuted || !got.classification.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("cooldown = %+v", got)
	}
}

func TestCodeAttemptMiniMaxDeclaredFoldingWritesModelEntitlementCooldown(t *testing.T) {
	// Sources: MiniMax-AI/cli@38aba5f (fetched 2026-11-05), src/errors/api.ts ApiErrorBody and mapApiError folding base_resp.status_code before error.code, and https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05), codes 2061 and 2056.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ledger := &fakeCodeLedger{}
	attempt := minimaxAttempt(t, ledger, now, "v1/chat/completions")
	attempt.response(t.Context(), 429, http.Header{})
	attempt.observe(attempt.observer.Unary([]byte(`{"base_resp":{"status_code":2061,"status_msg":"your current token plan not support model"},"error":{"message":"usage limit exceeded","type":"rate_limit_error","code":2056,"http_code":"429"}}`)))
	attempt.finish(t.Context())
	if len(ledger.cooldowns) != 1 {
		t.Fatalf("cooldowns = %+v, want one model cooldown", ledger.cooldowns)
	}
	got := ledger.cooldowns[0]
	if got.account != "account" || got.classification.Class != codeadapter.CooldownEntitlement || got.classification.Scope != codeadapter.ScopeModel || got.classification.Envelope != "minimax_2061" || got.classification.NotExecuted || !got.classification.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("cooldown = %+v", got)
	}
}

func TestCodeAttemptMiniMaxStandaloneErrorCoolsOnlyTargetModel(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, path := range []string{"v1/chat/completions", "v1/messages"} {
		for _, alias := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/alias=%t", path, alias), func(t *testing.T) {
				ledger := &fakeCodeLedger{}
				attempt := minimaxAttempt(t, ledger, now, path)
				attempt.dispatched = true
				if alias {
					attempt.publicModel = "public-model"
				}
				body := `{"base_resp":{"status_code":2061,"status_msg":"your current token plan not support model"}}`
				response := &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
				attempt.response(t.Context(), response.StatusCode, response.Header)
				attempt.copyResponse(t.Context(), httptest.NewRecorder(), response)
				attempt.finish(t.Context())
				if len(ledger.cooldowns) != 1 || ledger.cooldowns[0].model != "MiniMax-M3" || ledger.cooldowns[0].classification.Class != codeadapter.CooldownEntitlement || ledger.cooldowns[0].classification.Scope != codeadapter.ScopeModel || len(ledger.health) != 0 {
					t.Fatalf("cooldowns = %+v, health = %+v", ledger.cooldowns, ledger.health)
				}
				if len(ledger.outcomes) != 1 || ledger.outcomes[0].Origin != "upstream" || ledger.outcomes[0].Kind != "rejected" || len(ledger.settlements) != 1 || ledger.settlements[0].Total != nil || ledger.confirmations != 0 {
					t.Fatalf("outcomes = %+v, settlements = %+v, confirmations = %d", ledger.outcomes, ledger.settlements, ledger.confirmations)
				}
			})
		}
	}
}

func kimiAttempt(t *testing.T, ledger *fakeCodeLedger, now time.Time, path string) *codeAttempt {
	t.Helper()
	vendor, ok := codeadapter.Lookup(codemode.AdapterKimiCode)
	if !ok {
		t.Fatal("kimi vendor missing")
	}
	endpoint, ok := vendor.Endpoint(path)
	if !ok {
		t.Fatal("kimi endpoint missing")
	}
	server := &Server{now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), deps: Dependencies{CodeLedger: ledger}}
	return &codeAttempt{
		server:      server,
		permit:      codesessions.Permit{InitialRecovery: true, Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.KimiProfile}},
		endpoint:    endpoint,
		observer:    codeObserver(endpoint),
		model:       "kimi-model",
		publicModel: "kimi-model",
	}
}

func TestCodeAttemptKimiQuotaDoesNotRecoverWithoutNonexecutionProof(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	route := codemode.Route{InitialRecoveryAttempts: 1}
	for name, tc := range map[string]struct {
		path, body, envelope string
	}{
		"structured chat":                    {"v1/chat/completions", `{"error":{"message":"quota exceeded","type":"rate_limit_error","code":"exceeded_current_quota_error"}}`, "kimi_exceeded_current_quota_error"},
		"structured messages":                {"v1/messages", `{"type":"error","error":{"type":"insufficient_quota","message":"quota exceeded"}}`, "kimi_insufficient_quota"},
		"chat envelope on messages endpoint": {"v1/messages", `{"error":{"message":"quota exceeded","type":"rate_limit_error","code":"exceeded_current_quota_error"}}`, ""},
		"messages envelope on chat endpoint": {"v1/chat/completions", `{"type":"error","error":{"type":"insufficient_quota","message":"quota exceeded"}}`, ""},
		"message-only":                       {"v1/chat/completions", `{"error":{"message":"You've reached your 5-hour usage limit. Your quota will reset when the current 5-hour window ends. To continue now, purchase extra usage or upgrade your plan: https://www.kimi.com/membership/subscription?tab=quota","type":"rate_limit_error","code":"rate_limit_exceeded"}}`, ""},
		"extra member":                       {"v1/chat/completions", `{"error":{"message":"quota exceeded","code":"exceeded_current_quota_error","param":null}}`, ""},
	} {
		attempt := kimiAttempt(t, &fakeCodeLedger{}, now, tc.path)
		response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body))}
		recovery, err := attempt.recover(t.Context(), response, route, nil, false)
		if err != nil || recovery != nil {
			t.Fatalf("recover(%s) = %+v, %v, want no replay", name, recovery, err)
		}
	}
}

func TestCodeAttemptMiniMaxUsageAndAmbiguousDoNotRecover(t *testing.T) {
	// Sources: https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05), codes 2056 and 2062, and https://platform.minimax.io/docs/api-reference/speech-to-text (fetched 2026-11-05), which documents the OaiError envelope only for speech-to-text. It is not proven for chat/messages inference, so replay waits for live envelope qualification and neither shape replays today
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	route := codemode.Route{InitialRecoveryAttempts: 1}
	proven := minimaxAttempt(t, &fakeCodeLedger{}, now, "v1/chat/completions")
	response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)","http_code":"429"},"request_id":"r"}`))}
	recovery, err := proven.recover(t.Context(), response, route, nil, false)
	if err != nil || recovery != nil {
		t.Fatalf("recover(proven) = %+v, %v, want no replay pending live envelope qualification", recovery, err)
	}
	ambiguous := minimaxAttempt(t, &fakeCodeLedger{}, now, "v1/chat/completions")
	response = &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"M Plan rate limit reached (2062)","http_code":"429"},"request_id":"r"}`))}
	recovery, err = ambiguous.recover(t.Context(), response, route, nil, false)
	if err != nil || recovery != nil {
		t.Fatalf("recover(ambiguous) = %+v, %v, want no replay", recovery, err)
	}
}

func TestTranslatedCodexPublishedOutputCap(t *testing.T) {
	for _, spec := range []struct {
		path, cap, metadata string
		safety, refuse      bool
	}{
		{path: "v1/messages", cap: "max_tokens"}, {path: "v1/chat/completions", cap: "max_tokens"}, {path: "v1/chat/completions", cap: "max_completion_tokens"}, {path: "responses", cap: "max_output_tokens"},
		{path: "v1/messages", cap: "max_tokens", metadata: `{"user_id":"` + strings.Repeat("identity", 30) + `"}`, safety: true},
		{path: "v1/messages", cap: "max_tokens", metadata: `{"user_id":"other identity"}`, safety: true},
		{path: "v1/messages", cap: "max_tokens", metadata: `{"user_id":"identity"}`, refuse: true},
		{path: "v1/messages", cap: "max_tokens", metadata: `{"user_id":"identity","other":"state"}`, safety: true, refuse: true},
		{path: "v1/messages", cap: "max_tokens", metadata: `{"user_id":42}`, safety: true, refuse: true},
	} {
		path := spec.path
		for _, callerCap := range []int{1, 15, 16, 32} {
			for _, minimum := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/minimum=%t/cap=%d", path, spec.cap, minimum, callerCap), func(t *testing.T) {
					type observation struct {
						Cap    int    `json:"max_output_tokens"`
						Safety string `json:"safety_identifier"`
					}
					observed := make(chan observation, 1)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var body observation
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						observed <- body
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"resp_cap","object":"response","status":"completed","model":"gpt-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
					}))
					defer upstream.Close()
					project := "project"
					authority := keyauthority.Authority{ID: "key", ProjectID: &project, Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}}}
					ledger := &fakeCodeLedger{connectionPermit: codesessions.Permit{Authority: authority, Account: codemode.Account{ID: "account", ProviderID: "provider", Principal: "principal"}, Pin: codemode.Pin{AccountID: "account", Principal: "principal"}, Attempt: codemode.Attempt{ID: "attempt"}}}
					h := newHarness(t, Config{}, Dependencies{Admission: NewAdmission(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))), CodeLedger: ledger, CodeAuthorizer: fakeCodeAuthorizer{}})
					h.rt.keys[fullKey] = authority
					var route codemode.Route
					raw := fmt.Sprintf(`{"id":"route","project_id":"project","slug":"coding","pool_id":"pool","models":["gpt-model"],"enabled":true,"revision_id":"revision","revision":1,"published_at":"2026-11-07T00:00:00Z","messages_to_codex":true,"chat_to_codex":true,"translated_codex_output_minimum":%t,"messages_codex_safety_identifier":%t}`, minimum, spec.safety)
					if err := json.Unmarshal([]byte(raw), &route); err != nil {
						t.Fatal(err)
					}
					snapshot := &routing.Snapshot{Generation: routing.Generation{ID: uuid.NewString()}, CodeRoutes: map[string]codemode.Route{"coding": route}, CodeConnections: map[string]routing.Configuration{"revision:provider": {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID, Endpoint: upstream.URL}}}}
					publication, err := release.NewRelease("", 0, snapshot, nil)
					if err != nil {
						t.Fatal(err)
					}
					h.rt.release = publication
					mux := http.NewServeMux()
					h.gateway.RegisterCode(mux)
					body := func() string {
						if path == "responses" {
							return fmt.Sprintf(`{"model":"gpt-model","input":"hi","max_output_tokens":%d}`, callerCap)
						}
						metadata := ""
						if spec.metadata != "" {
							metadata = `,"metadata":` + spec.metadata
						}
						return fmt.Sprintf(`{"model":"gpt-model","messages":[{"role":"user","content":"hi"}],%q:%d%s}`, spec.cap, callerCap, metadata)
					}()
					request := httptest.NewRequest(http.MethodPost, "/code/coding/"+path, strings.NewReader(body))
					request.Header.Set("Authorization", "Bearer "+fullKey)
					response := httptest.NewRecorder()
					mux.ServeHTTP(response, request)
					if spec.refuse {
						if response.Code != http.StatusUnprocessableEntity {
							t.Fatalf("unsupported metadata = %d %s", response.Code, response.Body.String())
						}
						select {
						case <-observed:
							t.Fatal("unsupported metadata dispatched")
						default:
						}
						return
					}
					if response.Code != http.StatusOK {
						t.Fatalf("status %d %s", response.Code, response.Body.String())
					}
					want := callerCap
					if minimum && path != "responses" {
						want = max(16, callerCap)
					}
					upstreamBody := <-observed
					if upstreamBody.Cap != want {
						t.Fatalf("upstream output cap %d, want %d", upstreamBody.Cap, want)
					}
					if spec.safety && spec.metadata != "" {
						var metadata struct {
							UserID string `json:"user_id"`
						}
						if err := json.Unmarshal([]byte(spec.metadata), &metadata); err != nil {
							t.Fatal(err)
						}
						expected := fmt.Sprintf("%x", sha256.Sum256([]byte(metadata.UserID)))
						if upstreamBody.Safety != expected {
							t.Fatalf("safety identifier = %q, want stable hash %q", upstreamBody.Safety, expected)
						}
					}
					if minimum && callerCap == 1 && path != "responses" {
						unsupported := httptest.NewRequest(http.MethodPost, "/code/coding/"+path, strings.NewReader(body))
						unsupported.Header.Set("Authorization", "Bearer "+fullKey)
						if path == "v1/messages" {
							unsupported.Header.Set("Anthropic-Beta", "interleaved-thinking-2025-05-14")
						} else {
							unsupported.Header.Set("X-Codex-Turn-State", "opaque")
						}
						refused := httptest.NewRecorder()
						mux.ServeHTTP(refused, unsupported)
						expected := http.StatusUnprocessableEntity
						if path == "v1/chat/completions" {
							expected = http.StatusConflict
						}
						if refused.Code != expected {
							t.Fatalf("unsupported state = %d %s", refused.Code, refused.Body.String())
						}
						select {
						case <-observed:
							t.Fatal("unsupported state dispatched upstream")
						default:
						}
					}

				})
			}
		}
	}
}

func TestAntigravitySignedToolDurability(t *testing.T) {
	for _, policy := range []struct {
		signed  bool
		saves   int
		refusal string
	}{{false, 0, "code_upstream_unsupported_state"}, {true, 1, "code_continuation_unavailable"}} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("signed=%v/stream=%v", policy.signed, stream), func(t *testing.T) {
				ledger := &signedTestLedger{fakeCodeLedger: fakeCodeLedger{}, saveErr: errors.New("storage unavailable")}
				attempt := &codeAttempt{server: &Server{deps: Dependencies{CodeLedger: ledger}}}
				body := `{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"name":"read_file","args":{"path":"sample"}},"thoughtSignature":"private-signature"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":2,"totalTokenCount":10}}}`
				contentType := "application/json"
				if stream {
					body = "data: " + body + "\n\n"
					contentType = "text/event-stream"
				}
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
				recorder := httptest.NewRecorder()
				attempt.antigravityResponse(context.Background(), recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), response, "gemini-model", codePayload{stream: stream, signedContinuation: policy.signed})
				if ledger.saves != policy.saves || strings.Contains(recorder.Body.String(), "read_file") || strings.Contains(recorder.Body.String(), "private-signature") || !strings.Contains(recorder.Body.String(), policy.refusal) || attempt.usage.Total == nil || *attempt.usage.Total != 10 {
					t.Fatalf("saves = %d, usage = %+v, response = %s", ledger.saves, attempt.usage, recorder.Body.String())
				}
			})
		}
	}
}

type signedTestLedger struct {
	fakeCodeLedger
	saveErr error
	model   string
	saves   int
	calls   map[string]codesessions.SignedCall
}

func (l *signedTestLedger) SaveCodeContinuation(_ context.Context, _ codesessions.Permit, model string, calls []codesessions.SignedCall) error {
	l.saves++
	l.model = model
	l.calls = make(map[string]codesessions.SignedCall)
	for _, call := range calls {
		l.calls[call.ID] = call
	}
	return l.saveErr
}

func (l *signedTestLedger) LoadCodeContinuation(_ context.Context, _ codesessions.Permit, model string, id string) (codesessions.SignedCall, error) {
	if call, found := l.calls[id]; found && model == l.model {
		return call, nil
	}
	return codesessions.SignedCall{}, errors.New("missing")
}

func TestAntigravitySignedToolRoundTrip(t *testing.T) {
	ledger := &signedTestLedger{}
	attempt := &codeAttempt{model: "gemini-native", server: &Server{deps: Dependencies{CodeLedger: ledger}, now: time.Now}}
	body := `{"response":{"candidates":[{"content":{"parts":[{"text":"Looking"},{"functionCall":{"id":"provider-tool","name":"read_file","args":{"path":"sample","limit":1.0,"large":9007199254740993}},"thoughtSignature":"private-signature"},{"functionCall":{"name":"list_files","args":{}},"thoughtSignature":"private-second"}]},"finishReason":"STOP"}]}}`
	recorder := httptest.NewRecorder()
	attempt.antigravityResponse(context.Background(), recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, "public-model", codePayload{signedContinuation: true})
	var response struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || len(response.Choices) != 1 {
		t.Fatalf("response = %s, %v", recorder.Body.String(), err)
	}
	var message struct {
		ToolCalls []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(response.Choices[0].Message, &message); err != nil || len(message.ToolCalls) != 2 {
		t.Fatalf("message = %s, %v", response.Choices[0].Message, err)
	}
	requestBody := []byte(fmt.Sprintf(`{"model":"gemini-native","messages":[{"role":"user","content":"Inspect"},%s,{"role":"tool","tool_call_id":%q,"content":"file result"},{"role":"tool","tool_call_id":%q,"content":"directory result"}]}`, response.Choices[0].Message, message.ToolCalls[0].ID, message.ToolCalls[1].ID))
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	payload, err := attempt.translateAntigravity(requestBody, request, "gemini-native", "project", true)
	if err != nil || !strings.Contains(string(payload.body), `"thoughtSignature":"private-signature"`) || strings.Contains(string(payload.body), message.ToolCalls[0].ID) || !strings.Contains(string(payload.body), "file result") {
		t.Fatalf("continuation = %s, %v", payload.body, err)
	}
	if strings.Count(string(payload.body), `"id":"provider-tool"`) != 2 || strings.Contains(string(payload.body), message.ToolCalls[1].ID) || !strings.Contains(string(payload.body), `"thoughtSignature":"private-second"`) {
		t.Fatalf("parallel provider identity restoration = %s", payload.body)
	}
	partial := []byte(fmt.Sprintf(`{"model":"gemini-native","messages":[{"role":"user","content":"Inspect"},%s,{"role":"tool","tool_call_id":%q,"content":"file result"}]}`, response.Choices[0].Message, message.ToolCalls[0].ID))
	if _, err := attempt.translateAntigravity(partial, request, "gemini-native", "project", true); err == nil {
		t.Fatal("partial parallel tool results were accepted")
	}
	reversed := []byte(fmt.Sprintf(`{"model":"gemini-native","messages":[{"role":"user","content":"Inspect"},%s,{"role":"tool","tool_call_id":%q,"content":"directory result"},{"role":"tool","tool_call_id":%q,"content":"file result"}]}`, response.Choices[0].Message, message.ToolCalls[1].ID, message.ToolCalls[0].ID))
	if _, err := attempt.translateAntigravity(reversed, request, "gemini-native", "project", true); err != nil {
		t.Fatalf("reordered parallel results = %v", err)
	}
	premature := []byte(fmt.Sprintf(`{"model":"gemini-native","messages":[{"role":"user","content":"Inspect"},{"role":"tool","tool_call_id":%q,"content":"file result"},%s,{"role":"tool","tool_call_id":%q,"content":"directory result"}]}`, message.ToolCalls[0].ID, response.Choices[0].Message, message.ToolCalls[1].ID))
	if _, err := attempt.translateAntigravity(premature, request, "gemini-native", "project", true); err == nil {
		t.Fatal("tool result before its call was accepted")
	}
	missingState := ledger.calls[message.ToolCalls[1].ID]
	delete(ledger.calls, message.ToolCalls[1].ID)
	if _, err := attempt.translateAntigravity(requestBody, request, "gemini-native", "project", true); err == nil {
		t.Fatal("missing parallel association was accepted")
	}
	ledger.calls[missingState.ID] = missingState
	escaped := []byte(strings.Replace(string(requestBody), "sample", `\\u0073ample`, 1))
	if _, err := attempt.translateAntigravity(escaped, request, "gemini-native", "project", true); err != nil {
		t.Fatalf("escaped string argument = %v", err)
	}
	normalized := []byte(strings.Replace(string(requestBody), "1.0", "1", 1))
	if _, err := attempt.translateAntigravity(normalized, request, "gemini-native", "project", true); err != nil {
		t.Fatalf("reserialized numeric argument = %v", err)
	}
	changedInteger := []byte(strings.Replace(string(requestBody), "9007199254740993", "9007199254740992", 1))
	if _, err := attempt.translateAntigravity(changedInteger, request, "gemini-native", "project", true); err == nil {
		t.Fatal("changed large integer was accepted")
	}
	altered := []byte(strings.Replace(string(requestBody), "sample", "another", 1))
	if _, err := attempt.translateAntigravity(altered, request, "gemini-native", "project", true); err == nil {
		t.Fatal("altered signed call was accepted")
	}
}

func TestGrokPublishedTransportPreservesTextAndRefusesMedia(t *testing.T) {
	for _, test := range []struct {
		name, body string
		messages   bool
		identity   bool
		safety     bool
		status     int
	}{
		{name: "Grok metadata requires its own opt-in", messages: true, body: `{"model":"grok-model","max_tokens":4,"metadata":{"user_id":"fixture-user"},"messages":[{"role":"user","content":"hello"}]}`, status: http.StatusUnprocessableEntity},
		{name: "Grok metadata opt-in hashes identity", messages: true, safety: true, body: `{"model":"grok-model","max_tokens":4,"metadata":{"user_id":"fixture-user"},"messages":[{"role":"user","content":"hello"}]}`, status: http.StatusOK},
		{name: "Grok metadata extra state refuses", messages: true, safety: true, body: `{"model":"grok-model","max_tokens":4,"metadata":{"user_id":"fixture-user","state":"foreign"},"messages":[{"role":"user","content":"hello"}]}`, status: http.StatusUnprocessableEntity},
		{name: "translated Messages text", messages: true, body: `{"model":"grok-model","max_tokens":4,"messages":[{"role":"user","content":"hello"}]}`, status: http.StatusOK},
		{name: "unsupported media", body: `{"model":"grok-model","input":[{"role":"user","content":[{"type":"input_image","image_url":"https://example.invalid/image.png"}]}]}`, status: http.StatusUnprocessableEntity},
		{name: "truthful gateway identity", body: `{"model":"grok-model","input":"hello","include":["reasoning.encrypted_content"],"client_metadata":{"thread_id":"fixture-thread"}}`, status: http.StatusOK},
		{name: "explicit Grok identity", body: `{"model":"grok-model","input":"hello"}`, identity: true, status: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			reached := make(chan struct{}, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached <- struct{}{}
				raw, err := io.ReadAll(r.Body)
				if err != nil || !test.messages && string(raw) != test.body {
					t.Errorf("request changed %q, %v", raw, err)
				}
				if test.safety {
					expected := fmt.Sprintf(`"safety_identifier":"%x"`, sha256.Sum256([]byte("fixture-user")))
					if !bytes.Contains(raw, []byte(expected)) || bytes.Contains(raw, []byte("fixture-user")) {
						t.Errorf("identity mapping %s", raw)
					}
				}
				if !test.identity && (r.Header.Get("User-Agent") != "OLP" || r.Header.Get("X-Grok-Client-Identifier") != "") {
					t.Errorf("implicit identity %v", r.Header)
				}
				if test.identity && (r.Header.Get("X-Grok-Client-Identifier") != "grok-shell" || r.Header.Get("X-Xai-Token-Auth") != "xai-grok-cli") {
					t.Errorf("missing identity %v", r.Header)
				}
				_, _ = io.WriteString(w, `{ "id":"resp_foreign", "status":"completed", "output":[] }`)
			}))
			defer upstream.Close()
			project := "project"
			authority := keyauthority.Authority{ID: "key", ProjectID: &project, Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}}}
			ledger := &fakeCodeLedger{connectionPermit: codesessions.Permit{Authority: authority, Account: codemode.Account{ID: "account", ProviderID: "provider", Principal: "principal"}, Pin: codemode.Pin{AccountID: "account", Principal: "principal"}, Attempt: codemode.Attempt{ID: "attempt"}}}
			h := newHarness(t, Config{}, Dependencies{Admission: NewAdmission(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))), CodeLedger: ledger, CodeAuthorizer: fakeCodeAuthorizer{}})
			h.rt.keys[fullKey] = authority
			published := time.Now()
			route := codemode.Route{PublishedAt: &published, ID: "route", ProjectID: project, Slug: "grok", PoolID: "pool", Models: []string{"grok-model"}, Enabled: true, RevisionID: "revision", Revision: 1, ResponsesToGrok: !test.messages, MessagesToGrok: test.messages, MessagesGrokSafetyIdentifier: test.safety, MessagesCodexSafetyIdentifier: true, GrokCLIIdentity: test.identity}
			publication, err := release.NewRelease("", 0, &routing.Snapshot{Generation: routing.Generation{ID: uuid.NewString()}, CodeRoutes: map[string]codemode.Route{"grok": route}, CodeConnections: map[string]routing.Configuration{"revision:provider": {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: "grok-build-subscription", Endpoint: upstream.URL}}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			h.rt.release = publication
			mux := http.NewServeMux()
			h.gateway.RegisterCode(mux)
			path := "/code/grok/responses"
			if test.messages {
				path = "/code/grok/v1/messages?beta=true"
			}
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+fullKey)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("unsupported input = %d %s", response.Code, response.Body.String())
			}
			select {
			case <-reached:
				if test.status != http.StatusOK {
					t.Fatal("unsupported media reached subscription")
				}
			default:
				if test.status == http.StatusOK {
					t.Fatal("text did not reach subscription")
				}
			}
		})
	}
}

func TestGrokMessagesToolContinuationAndUnsupportedState(t *testing.T) {
	for _, mode := range []struct{ stream, incomplete bool }{{}, {stream: true}, {stream: true, incomplete: true}} {
		stream := mode.stream
		t.Run(fmt.Sprintf("stream=%t/incomplete=%t", stream, mode.incomplete), func(t *testing.T) {
			observed := make(chan []byte, 8)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				observed <- body
				var request struct {
					Stream bool   `json:"stream"`
					Safety string `json:"safety_identifier"`
					Cap    int    `json:"max_output_tokens"`
				}
				if err := json.Unmarshal(body, &request); err != nil || request.Stream != stream || request.Cap != 4 || request.Safety != fmt.Sprintf("%x", sha256.Sum256([]byte("fixture-user"))) {
					t.Errorf("translated request %s, %v", body, err)
				}
				output := `[{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{\"path\":\"main.go\"}"}]`
				if bytes.Contains(body, []byte(`"function_call_output"`)) {
					output = `[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"file read"}]}]`
				}
				response := `{"id":"answer","status":"completed","model":"grok-model","output":` + output + `,"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					if mode.incomplete {
						_, _ = io.WriteString(w, "data: "+`{"type":"response.created","response":{"id":"answer","status":"in_progress","output":[]}}`+"\n\ndata: "+`{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"partial"}`+"\n\n")
						return
					}
					_, _ = io.WriteString(w, "data: "+`{"type":"response.created","response":{"id":"answer","status":"in_progress","output":[]}}`+"\n\ndata: "+`{"type":"response.completed","response":`+response+"}\n\n")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, response)
			}))
			defer upstream.Close()
			project := "project"
			authority := keyauthority.Authority{ID: "key", ProjectID: &project, Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}}}
			ledger := &fakeCodeLedger{connectionPermit: codesessions.Permit{Authority: authority, Account: codemode.Account{ID: "account", ProviderID: "provider", Principal: "principal"}, Pin: codemode.Pin{AccountID: "account", Principal: "principal"}, Attempt: codemode.Attempt{ID: "attempt"}}}
			h := newHarness(t, Config{}, Dependencies{Admission: NewAdmission(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil))), CodeLedger: ledger, CodeAuthorizer: fakeCodeAuthorizer{}})
			h.rt.keys[fullKey] = authority
			published := time.Now()
			route := codemode.Route{PublishedAt: &published, ID: "route", ProjectID: project, Slug: "grok", PoolID: "pool", Models: []string{"grok-model"}, Enabled: true, RevisionID: "revision", Revision: 1, MessagesToGrok: true, MessagesGrokSafetyIdentifier: true, TranslatedCodexOutputMinimum: true}
			publication, err := release.NewRelease("", 0, &routing.Snapshot{Generation: routing.Generation{ID: uuid.NewString()}, CodeRoutes: map[string]codemode.Route{"grok": route}, CodeConnections: map[string]routing.Configuration{"revision:provider": {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: "grok-build-subscription", Endpoint: upstream.URL}}}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			h.rt.release = publication
			mux := http.NewServeMux()
			h.gateway.RegisterCode(mux)
			call := func(body string) *httptest.ResponseRecorder {
				request := httptest.NewRequest(http.MethodPost, "/code/grok/v1/messages?beta=true", strings.NewReader(body))
				request.Header.Set("Authorization", "Bearer "+fullKey)
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, request)
				return response
			}
			prefix := fmt.Sprintf(`{"model":"grok-model","max_tokens":4,"stream":%t,"system":"Read carefully","metadata":{"user_id":"fixture-user"},"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],"messages":`, stream)
			first := call(prefix + `[{"role":"user","content":"read main.go"}]}`)
			if mode.incomplete {
				if !strings.Contains(first.Body.String(), "code_upstream_invalid") {
					t.Fatalf("incomplete stream %d %s", first.Code, first.Body.String())
				}
				<-observed
				select {
				case body := <-observed:
					t.Fatalf("incomplete stream replayed %s", body)
				default:
				}
				if !slices.ContainsFunc(ledger.outcomes, func(outcome codemode.Outcome) bool { return outcome.Kind == "incomplete" }) || ledger.confirmations != 0 {
					t.Fatalf("incomplete outcome %+v, confirmations %d", ledger.outcomes, ledger.confirmations)
				}
				return
			}
			if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), `"id":"call-read"`) || !strings.Contains(first.Body.String(), `"name":"read_file"`) || !strings.Contains(first.Body.String(), `"stop_reason":"tool_use"`) {
				t.Fatalf("tool response %d %s", first.Code, first.Body.String())
			}
			firstRequest := <-observed
			for _, value := range []string{"Read carefully", `"name":"read_file"`, `"required":["path"]`} {
				if !bytes.Contains(firstRequest, []byte(value)) {
					t.Fatalf("lost %s in %s", value, firstRequest)
				}
			}
			second := call(prefix + `[{"role":"user","content":"read main.go"},{"role":"assistant","content":[{"type":"tool_use","id":"call-read","name":"read_file","input":{"path":"main.go"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-read","content":"package main"}]}]}`)
			if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), "file read") {
				t.Fatalf("continuation %d %s", second.Code, second.Body.String())
			}
			secondRequest := <-observed
			for _, value := range []string{`"call_id":"call-read"`, `"type":"function_call_output"`, "package main", `\"path\":\"main.go\"`} {
				if !bytes.Contains(secondRequest, []byte(value)) {
					t.Fatalf("lost %s in %s", value, secondRequest)
				}
			}
			if len(ledger.settlements) != 2 || ledger.settlements[0].Total == nil || *ledger.settlements[0].Total != 5 || ledger.settlements[1].Total == nil || *ledger.settlements[1].Total != 5 {
				t.Fatalf("usage %+v", ledger.settlements)
			}
			for _, content := range []string{`{"type":"thinking","thinking":"opaque","signature":"signed"}`, `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}`, `{"type":"text","text":"cached","cache_control":{"type":"ephemeral"}}`} {
				response := call(prefix + `[{"role":"user","content":[` + content + `]}]}`)
				if response.Code != http.StatusUnprocessableEntity && response.Code != http.StatusConflict {
					t.Fatalf("unsupported content = %d %s", response.Code, response.Body.String())
				}
			}
			select {
			case body := <-observed:
				t.Fatalf("unsupported request dispatched %s", body)
			default:
			}
		})
	}
}
