package proxy

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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/client"
	"github.com/tyk-swe/olp/internal/code/session"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/code/wire"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/oif"
	protocolwire "github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type recordedCooldown struct {
	account        string
	model          string
	classification vendor.Classification
}

type fakeLedger struct {
	connectionPermit session.Permit
	health           []code.HealthObservation
	cooldowns        []recordedCooldown
	outcomes         []code.Outcome
	settlements      []code.Usage
	confirmations    int
	cooldownErr      error
	chargeAttempt    string
	charge           json.RawMessage
}

func (f *fakeLedger) Models(context.Context, code.Route, string) (client.Offers, error) {
	return client.Offers{}, nil
}
func (f *fakeLedger) Admit(context.Context, session.Admission) (session.Permit, error) {
	return session.Permit{}, nil
}
func (f *fakeLedger) BindConnection(context.Context, code.Route, string, code.Identity, string, []string) (session.Permit, error) {
	return f.connectionPermit, nil
}
func (f *fakeLedger) MarkDispatched(context.Context, string) error { return nil }
func (f *fakeLedger) ConfirmNonexecution(context.Context, string, int) error {
	f.confirmations++
	return nil
}
func (f *fakeLedger) ObserveCooldown(context.Context, string, time.Time) error { return nil }
func (f *fakeLedger) ObserveCooldownClass(_ context.Context, account, model string, classification vendor.Classification, _ time.Time) error {
	if classification.Scope == code.ScopeModel && model == "" {
		return code.Refuse(400, "code_cooldown_invalid")
	}
	if f.cooldownErr != nil {
		return f.cooldownErr
	}
	f.cooldowns = append(f.cooldowns, recordedCooldown{account: account, model: model, classification: classification})
	return nil
}
func (f *fakeLedger) Settle(_ context.Context, _ string, u code.Usage) error {
	f.settlements = append(f.settlements, u)
	return nil
}
func (f *fakeLedger) Abort(context.Context, string) error { return nil }
func (f *fakeLedger) ObserveHealth(_ context.Context, _ string, obs code.HealthObservation) error {
	f.health = append(f.health, obs)
	return nil
}
func (f *fakeLedger) ObserveAllowance(context.Context, string, code.Allowance) error { return nil }
func (f *fakeLedger) ObserveAllowanceCharge(_ context.Context, attempt string, charge json.RawMessage) error {
	f.chargeAttempt, f.charge = attempt, charge
	return nil
}
func (f *fakeLedger) ObserveReference(context.Context, string, string) error { return nil }
func (f *fakeLedger) ObserveOutcome(_ context.Context, _ string, outcome code.Outcome) error {
	f.outcomes = append(f.outcomes, outcome)
	return nil
}
func (f *fakeLedger) RecordRefusal(context.Context, code.Route, string, string, string) error {
	return nil
}
func (f *fakeLedger) ResolvePaidFallback(context.Context, session.Admission, keyauthority.Authority) (*code.Operation, error) {
	return nil, nil
}
func (f *fakeLedger) ResolvePaidModel(_ context.Context, in session.Admission, _ keyauthority.Authority) (code.Operation, error) {
	return in.Operation, nil
}
func (f *fakeLedger) AdmitPaid(context.Context, session.PaidAdmission) (session.PaidPermit, error) {
	return session.PaidPermit{}, nil
}

type fakeAuthorizer struct{}

func (fakeAuthorizer) AuthorizeCode(_ context.Context, _ routing.Configuration, account code.Account, _ code.Dispatch) (code.Authorization, error) {
	return code.Authorization{Principal: account.Principal, Headers: http.Header{"Authorization": {"Bearer fixture"}}}, nil
}

type fakeHost struct {
	now    time.Time
	egress *egress.Policy
}

func (h *fakeHost) AdmitInFlight(context.Context) bool { return true }
func (h *fakeHost) ReleaseInFlight(context.Context)    {}
func (h *fakeHost) Authenticate(*http.Request) (keyauthority.Authority, int, bool) {
	return keyauthority.Authority{}, 0, true
}
func (h *fakeHost) AuthenticatePresentedKey(*http.Request) (keyauthority.Authority, int, bool) {
	return keyauthority.Authority{}, 0, true
}
func (h *fakeHost) ParseAttribution(*http.Request, keyauthority.Authority) (map[string]string, int, string, bool) {
	return nil, 0, "", true
}
func (h *fakeHost) CheckKeyAddress(*http.Request, keyauthority.Authority) (int, bool) {
	return 0, true
}
func (h *fakeHost) IdentifyEndUser(_ *http.Request, a keyauthority.Authority) (keyauthority.Authority, int, bool) {
	return a, 0, true
}
func (h *fakeHost) Release() *release.Release           { return nil }
func (h *fakeHost) RoutingInputs() *usage.RoutingInputs { return nil }
func (h *fakeHost) IsCredentialEligible(string) bool    { return true }
func (h *fakeHost) Secret(context.Context, *release.Release, string) ([]byte, int64, error) {
	return nil, 0, nil
}
func (h *fakeHost) CredentialRefused(string, int64) {}
func (h *fakeHost) ApplyConnectorAuth(context.Context, *http.Request, connectors.Config, []byte, []byte) (egress.Sensitive, error) {
	return egress.Sensitive{}, nil
}
func (h *fakeHost) RedactFailureDocument(doc oif.Document, _ egress.Sensitive) (oif.Document, error) {
	return doc, nil
}
func (h *fakeHost) ReserveRate(context.Context, keyauthority.Authority, int64, time.Duration, string) (*admission.Lease, int, bool) {
	return nil, 0, true
}
func (h *fakeHost) ReserveProvider(context.Context, string, routing.Configuration, int64, time.Duration) (*admission.Lease, int, bool) {
	return nil, 0, true
}
func (h *fakeHost) ReservePaid(context.Context, *keyauthority.Authority, code.Route, routing.Provider, routing.Slot, int64, time.Duration, string, string, string) (*admission.Lease, PaidTargetLease, []*admission.Lease, error) {
	return nil, nil, nil, nil
}
func (h *fakeHost) SettleLease(context.Context, *admission.Lease, bool, *int64) {}
func (h *fakeHost) RecordPaidTerminal(PaidTerminal)                             {}
func (h *fakeHost) ValidateEndpoint(target string) error {
	if h.egress == nil {
		return nil
	}
	_, err := h.egress.ValidateEndpoint(target)
	return err
}
func (h *fakeHost) ConnectionClient(_ context.Context, _ *release.Release, cfg routing.Configuration, _ string) (*http.Client, error) {
	return h.egress.RawConnectionClient(cfg.Options.Network, nil, 5*time.Minute)
}
func (h *fakeHost) NewStreamWriter(http.ResponseWriter, protocolwire.Format) StreamWriter {
	return nil
}
func (h *fakeHost) WriteRefusal(w http.ResponseWriter, _ *http.Request, refusal *code.Refusal) {
	if d := refusal.RetryAfter(h.Now()); d > 0 {
		w.Header().Set("Retry-After", strconv.FormatInt(int64(d/time.Second), 10))
	}
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": refusal.Code, "message": RefusalMessage(refusal.Code)}})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(refusal.Status)
	_, _ = w.Write(body)
}
func (h *fakeHost) WriteTranslatedRejection(w http.ResponseWriter, r *http.Request, refusal *code.Refusal) {
	body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": refusal.Code, "message": RefusalMessage(refusal.Code)}})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(refusal.Status)
	_, _ = w.Write(body)
	_ = r
}
func (h *fakeHost) BodyLimit() int64  { return wire.MaxBody }
func (h *fakeHost) Now() time.Time    { return h.now }
func (h *fakeHost) Log() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newSubAttempt(t *testing.T, ledger *fakeLedger, adapter code.Adapter, profile, path, model string, now time.Time, anchor *string) *subscriptionAttempt {
	t.Helper()
	v, ok := vendor.Lookup(adapter)
	if !ok {
		t.Fatalf("vendor %s missing", adapter)
	}
	endpoint, ok := v.Endpoint(path)
	if !ok {
		t.Fatalf("endpoint %s/%s missing", adapter, path)
	}
	srv := &Server{
		Host:   &fakeHost{now: now, egress: &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}},
		Ledger: ledger, Authorizer: fakeAuthorizer{},
	}
	return &subscriptionAttempt{
		core: attemptCore{
			server: srv, attemptID: "attempt", model: model, publicModel: model,
			protocol: endpoint.Protocol, observer: wire.NewObserver(endpoint),
		},
		permit: session.Permit{
			InitialRecovery: true,
			Account:         code.Account{ID: "account", CycleAnchor: anchor},
			Attempt:         code.Attempt{ID: "attempt"},
		},
		config:   routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: profile}},
		vendor:   v,
		endpoint: endpoint,
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

func TestModelHintRefusalAndTranslatedRejections(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	deadline := now.Add(time.Minute)
	host := &fakeHost{now: now}
	recorder := httptest.NewRecorder()
	host.WriteRefusal(recorder, httptest.NewRequest(http.MethodGet, "/v1/responses", nil), &code.Refusal{Status: http.StatusBadRequest, Code: "code_connection_model_required", RetryAt: &deadline})
	if recorder.Code != http.StatusBadRequest || recorder.Header().Get("Retry-After") == "" || !strings.Contains(recorder.Body.String(), "X-OLP-Code-Model") {
		t.Fatalf("model hint refusal = %d, %v, %s", recorder.Code, recorder.Header(), recorder.Body.String())
	}

	for _, status := range []int{429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			ledger := &fakeLedger{}
			attempt := newSubAttempt(t, ledger, code.AdapterCodex, codexauth.ProfileID, "responses", "gpt-model", now, nil)
			attempt.core.publicModel = "public-model"
			attempt.core.dispatched = true
			writer := &notifyingResponseWriter{ResponseRecorder: httptest.NewRecorder(), written: make(chan struct{})}
			premature := make(chan struct{}, 1)
			body := afterResponseReader{ready: writer.written, premature: premature, reader: strings.NewReader(`{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`)}
			response := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(body)}
			attempt.onResponseHeaders(t.Context(), status, response.Header)
			attempt.translateResponse(t.Context(), writer, httptest.NewRequest(http.MethodPost, "/", nil), response, "public-model", false, protocolwire.Chat, false)
			attempt.finish(t.Context())
			select {
			case <-premature:
				t.Fatal("upstream body read before client response")
			default:
			}
			if writer.Code != status || !strings.Contains(writer.Body.String(), "code_upstream_rejected") {
				t.Fatalf("response = %d %s", writer.Code, writer.Body.String())
			}
			if status == 429 && (len(ledger.cooldowns) != 1 || ledger.cooldowns[0].classification.Class != code.CooldownCapacity) {
				t.Fatalf("cooldowns = %+v", ledger.cooldowns)
			}
			if status == 503 && len(ledger.health) != 1 {
				t.Fatalf("health = %+v", ledger.health)
			}
		})
	}
}

func TestStalledTranslatedRateLimitAndRecovery(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	ledger := &fakeLedger{}
	attempt := newSubAttempt(t, ledger, code.AdapterCodex, codexauth.ProfileID, "responses", "gpt-model", now, nil)
	attempt.core.dispatched = true
	body := &stalledRejectionBody{prefix: strings.NewReader(`{"error":`), waiting: make(chan struct{}), closed: make(chan struct{})}
	defer body.Close()
	response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: body}
	attempt.onResponseHeaders(t.Context(), 429, response.Header)
	result := make(chan *code.Recovery, 1)
	go func() {
		recovery, _ := attempt.recover(t.Context(), response, code.Route{InitialRecoveryAttempts: 1}, nil, true)
		result <- recovery
	}()
	<-body.waiting
	select {
	case <-body.closed:
	case <-time.After(3 * time.Second):
		_ = body.Close()
		<-result
		t.Fatal("translated recovery waited for stalled rejection")
	}
	if rec := <-result; rec != nil {
		t.Fatalf("incomplete rejection replayed: %+v", rec)
	}
}

func TestWebSocketHandshakeRejectionAndCapacityModelCooldown(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("compressed=%t", compressed), func(t *testing.T) {
			rawBody := []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":600}}`)
			header := http.Header{"Retry-After": {"60"}}
			if compressed {
				var encoded bytes.Buffer
				gz := gzip.NewWriter(&encoded)
				_, _ = gz.Write(rawBody)
				_ = gz.Close()
				rawBody = encoded.Bytes()
				header.Set("Content-Encoding", "gzip")
			}
			upstreamSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range header {
					w.Header()[k] = v
				}
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write(rawBody)
			}))
			defer upstreamSrv.Close()
			ledger := &fakeLedger{}
			attempt := newSubAttempt(t, ledger, code.AdapterCodex, codexauth.ProfileID, "responses", "gpt-model", now, nil)
			attempt.core.attemptID = ""
			writer := httptest.NewRecorder()
			base := http.DefaultTransport.(*http.Transport).Clone()
			base.DisableCompression = true
			defer base.CloseIdleConnections()
			tr := &handshakeTransport{base: base, reject: func(resp *http.Response) { forwardHandshakeRejection(t.Context(), writer, attempt, resp) }}
			_, resp, err := websocket.Dial(t.Context(), upstreamSrv.URL, &websocket.DialOptions{HTTPClient: &http.Client{Transport: tr}})
			if err == nil || resp == nil {
				t.Fatalf("dial = %+v, %v", resp, err)
			}
			attempt.flushPendingHealth(t.Context())
			if writer.Code != http.StatusTooManyRequests || !bytes.Equal(writer.Body.Bytes(), rawBody) || len(ledger.cooldowns) != 1 || ledger.cooldowns[0].classification.Class != code.CooldownQuota {
				t.Fatalf("handshake = %d, cooldowns = %+v", writer.Code, ledger.cooldowns)
			}
		})
	}

	capUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`)
	}))
	defer capUpstream.Close()
	route := code.Route{
		RevisionID: "revision", Models: []string{"gpt-model", "alternate-model"},
		ModelRewriting: true, Aliases: map[string]string{"public-model": "gpt-model"},
	}
	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: uuid.NewString()},
		CodeConnections: map[string]routing.Configuration{
			"revision:provider": {Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID, Endpoint: capUpstream.URL}},
		},
	}
	ingress, _ := vendor.ForPath("responses")
	snapshotRelease, err := release.NewRelease("", 0, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &fakeLedger{connectionPermit: session.Permit{
		Account: code.Account{ID: "account", ProviderID: "provider", Principal: "principal"},
		Pin:     code.Pin{Model: "public-model", TargetModel: "alternate-model", Principal: "principal"},
	}}
	srv := &Server{
		Host:   &fakeHost{now: now, egress: &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}},
		Ledger: ledger, Authorizer: fakeAuthorizer{},
	}
	req := httptest.NewRequest(http.MethodGet, "/responses", nil)
	req.Header.Set("X-OLP-Code-Affinity", "conversation")
	req.Header.Set("X-OLP-Code-Model", "public-model")
	rec := httptest.NewRecorder()
	srv.serveWebSocket(rec, req, snapshotRelease, route, keyauthority.Authority{}, ingress)
	if rec.Code != http.StatusTooManyRequests || len(ledger.cooldowns) != 1 || ledger.cooldowns[0].model != "alternate-model" || ledger.cooldowns[0].classification.Class != code.CooldownCapacity {
		t.Fatalf("ws capacity = %d, cooldowns = %+v", rec.Code, ledger.cooldowns)
	}
}

func TestVendorCooldownsFallbackAndCycleReset(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

	// GLM expiry suppresses fallback health and cooldown (https://docs.z.ai/api-reference/api-code, fetched 2026-11-05).
	glmLedger := &fakeLedger{}
	glmAtt := newSubAttempt(t, glmLedger, code.AdapterZAICoding, codeplans.ZAIProfile, "v1/chat/completions", "glm-model", now, nil)
	glmAtt.onResponseHeaders(t.Context(), 429, http.Header{"Retry-After": {"120"}})
	glmAtt.observe(glmAtt.core.observer.Unary([]byte(`{"error":{"code":"1309","message":"Your GLM Coding Plan package has expired"}}`)))
	glmAtt.finish(t.Context())
	if len(glmLedger.health) != 0 || len(glmLedger.cooldowns) != 0 {
		t.Fatalf("glm expiry wrote health %+v cooldowns %+v", glmLedger.health, glmLedger.cooldowns)
	}

	// Qwen insufficient_quota honors subscription cycle_anchor over shorter Retry-After (https://www.alibabacloud.com/help/en/model-studio/token-plan-team-faq, fetched 2026-11-05).
	anchor := "2026-01-15"
	qwenLedger := &fakeLedger{}
	qwenAtt := newSubAttempt(t, qwenLedger, code.AdapterQwenTokenPlan, codeplans.QwenTokenPlanProfile, "v1/messages", "qwen-model", now, &anchor)
	qwenAtt.onResponseHeaders(t.Context(), 429, http.Header{"Retry-After": {"3600"}})
	qwenAtt.observe(qwenAtt.core.observer.Unary([]byte(`{"error":{"code":"insufficient_quota","message":"Your token-plan quota has been exhausted."}}`)))
	qwenAtt.finish(t.Context())
	wantReset := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	if len(qwenLedger.cooldowns) != 1 || !qwenLedger.cooldowns[0].classification.RetryAt.Equal(wantReset) {
		t.Fatalf("qwen cooldowns = %+v, want %s", qwenLedger.cooldowns, wantReset)
	}

	// MiniMax standalone 2061 cools only target model (MiniMax-AI/cli@38aba5f, fetched 2026-11-05).
	mmLedger := &fakeLedger{}
	mmAtt := newSubAttempt(t, mmLedger, code.AdapterMiniMaxPlan, codeplans.MiniMaxProfile, "v1/chat/completions", "MiniMax-M3", now, nil)
	mmAtt.core.publicModel = "public-model"
	mmAtt.core.dispatched = true
	resp := &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"base_resp":{"status_code":2061,"status_msg":"your current token plan not support model"}}`))}
	mmAtt.onResponseHeaders(t.Context(), 429, resp.Header)
	copyHTTPResponse(t.Context(), httptest.NewRecorder(), resp, &mmAtt.core, mmAtt.observe, mmAtt.observeUnary, mmAtt.transportFailed)
	mmAtt.finish(t.Context())
	if len(mmLedger.cooldowns) != 1 || mmLedger.cooldowns[0].model != "MiniMax-M3" || len(mmLedger.health) != 0 {
		t.Fatalf("minimax cooldowns = %+v, health = %+v", mmLedger.cooldowns, mmLedger.health)
	}

	reported := json.RawMessage(`{"billing_source":"go","allowance":0.0021}`)
	openCodeLedger := &fakeLedger{}
	openCodeAtt := newSubAttempt(t, openCodeLedger, code.AdapterOpenCodeGo, codeplans.OpenCodeGoProfile, "v1/chat/completions", "glm-5", now, nil)
	openCodeAtt.observe(code.Observation{Terminal: true, AllowanceCharge: reported})
	if openCodeLedger.chargeAttempt != "attempt" || string(openCodeLedger.charge) != string(reported) {
		t.Fatalf("opencode charge = %s %s", openCodeLedger.chargeAttempt, openCodeLedger.charge)
	}
	zaiChargeLedger := &fakeLedger{}
	zaiAtt := newSubAttempt(t, zaiChargeLedger, code.AdapterZAICoding, codeplans.ZAIProfile, "v1/chat/completions", "glm-5", now, nil)
	zaiAtt.observe(code.Observation{Terminal: true, AllowanceCharge: reported})
	if zaiChargeLedger.charge != nil {
		t.Fatalf("non-opencode adapter stored charge: %s", zaiChargeLedger.charge)
	}
}
