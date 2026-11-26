package codeforward

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode/codeclient"
	"github.com/tyk-swe/olp/internal/codemode/codeplans"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/connectors/providerconfig"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/routing"
)

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
	server := &Forwarder{now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ledger: ledger}
	return &codeAttempt{
		forwarder:   server,
		permit:      codesessions.Permit{InitialRecovery: true, Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.ZAIProfile}},
		endpoint:    endpoint,
		observer:    codeObserver(endpoint, 1),
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
	attempt.observe(t.Context(), attempt.observer.Unary([]byte(`{"error":{"code":"1316","message":"Usage limit reached for the past 5 hours. Insufficient balance for extra usage. Resets at `+reset.Format(time.RFC3339)+`."}}`)))
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
	attempt.observe(t.Context(), attempt.observer.Unary([]byte(`{"error":{"code":"1309","message":"Your GLM Coding Plan package has expired"}}`)))
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
	server := &Forwarder{now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ledger: ledger}
	return &codeAttempt{
		forwarder:   server,
		permit:      codesessions.Permit{InitialRecovery: true, Account: codemode.Account{ID: "account", CycleAnchor: anchor}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.QwenTokenPlanProfile}},
		endpoint:    endpoint,
		observer:    codeObserver(endpoint, 1),
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
	attempt.observe(t.Context(), attempt.observer.Unary([]byte(`{"error":{"code":"insufficient_quota","message":"Your token-plan quota has been exhausted."}}`)))
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
	attempt.observe(t.Context(), attempt.observer.Unary([]byte(`{"error":{"code":"insufficient_quota","message":"Your token-plan quota has been exhausted."}}`)))
	attempt.finish(t.Context())
	if len(ledger.cooldowns) != 1 || !ledger.cooldowns[0].classification.RetryAt.Equal(want) {
		t.Fatalf("cooldowns = %+v, want anchor reset %s over one hour retry-after", ledger.cooldowns, want)
	}
	longLedger := &fakeCodeLedger{}
	longAttempt := qwenAttempt(t, longLedger, now, &anchor)
	longAttempt.response(t.Context(), 429, http.Header{"Retry-After": {"2592000"}})
	longAttempt.observe(t.Context(), longAttempt.observer.Unary([]byte(`{"error":{"code":"insufficient_quota","message":"Your token-plan quota has been exhausted."}}`)))
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
	attempt.observe(t.Context(), attempt.observer.Unary([]byte(`{"error":{"code":"insufficient_quota","message":"Your token-plan quota has been exhausted."}}`)))
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
	server := &Forwarder{now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ledger: ledger}
	return &codeAttempt{
		forwarder:   server,
		permit:      codesessions.Permit{InitialRecovery: true, Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.MiniMaxProfile}},
		endpoint:    endpoint,
		observer:    codeObserver(endpoint, 1),
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
	attempt.observe(t.Context(), attempt.observer.Unary([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)","http_code":"429"},"request_id":"r"}`)))
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
	attempt.observe(t.Context(), attempt.observer.Unary([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"M Plan rate limit reached (2062)","http_code":"429"},"request_id":"r"}`)))
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
	attempt.observe(t.Context(), attempt.observer.Unary([]byte(`{"base_resp":{"status_code":2061,"status_msg":"your current token plan not support model"},"error":{"message":"usage limit exceeded","type":"rate_limit_error","code":2056,"http_code":"429"}}`)))
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
	server := &Forwarder{now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil)), ledger: ledger}
	return &codeAttempt{
		forwarder:   server,
		permit:      codesessions.Permit{InitialRecovery: true, Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.KimiProfile}},
		endpoint:    endpoint,
		observer:    codeObserver(endpoint, 1),
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

type chargeLedger struct {
	charge  json.RawMessage
	attempt string
}

func (l *chargeLedger) ResolvePaidFallback(context.Context, codesessions.Admission, keyauthority.Authority) (*codemode.Operation, error) {
	return nil, nil
}
func (l *chargeLedger) ResolvePaidModel(context.Context, codesessions.Admission, keyauthority.Authority) (codemode.Operation, error) {
	return codemode.Operation{}, errors.New("unexpected paid model resolution")
}
func (l *chargeLedger) AdmitPaid(context.Context, codesessions.PaidAdmission) (codesessions.PaidPermit, error) {
	return codesessions.PaidPermit{}, errors.New("unexpected paid admission")
}

func (l *chargeLedger) Models(context.Context, codemode.Route, string) (codeclient.Offers, error) {
	return nil, nil
}
func (l *chargeLedger) ThreadStatus(context.Context, codemode.Route, string, codemode.Identity, time.Time) (codemode.ThreadStatus, error) {
	return codemode.ThreadStatus{}, nil
}
func (l *chargeLedger) Admit(context.Context, codesessions.Admission) (codesessions.Permit, error) {
	return codesessions.Permit{}, nil
}
func (l *chargeLedger) BindConnection(context.Context, codemode.Route, string, codemode.Identity, string, []string) (codesessions.Permit, error) {
	return codesessions.Permit{}, nil
}
func (l *chargeLedger) MarkDispatched(context.Context, string) error { return nil }
func (l *chargeLedger) ConfirmNonexecution(context.Context, string, int) error {
	return nil
}
func (l *chargeLedger) ObserveCooldown(context.Context, string, time.Time) error { return nil }
func (l *chargeLedger) ObserveCooldownClass(context.Context, string, string, codeadapter.Classification, time.Time) error {
	return nil
}
func (l *chargeLedger) Settle(context.Context, string, codemode.Usage) error { return nil }
func (l *chargeLedger) Abort(context.Context, string) error                  { return nil }
func (l *chargeLedger) ObserveHealth(context.Context, string, codemode.HealthObservation) error {
	return nil
}
func (l *chargeLedger) ObserveAllowance(context.Context, string, codemode.Allowance) error {
	return nil
}
func (l *chargeLedger) ObserveAllowanceCharge(_ context.Context, attempt string, charge json.RawMessage) error {
	l.attempt, l.charge = attempt, charge
	return nil
}
func (l *chargeLedger) ObserveReference(context.Context, string, string) error { return nil }
func (l *chargeLedger) ObserveOutcome(context.Context, string, codemode.Outcome) error {
	return nil
}
func (l *chargeLedger) RecordRefusal(context.Context, codemode.Route, string, string, string) error {
	return nil
}

func TestCodeAttemptStoresAllowanceChargeOnlyForOpenCodeGo(t *testing.T) {
	reported := json.RawMessage(`{"billing_source":"go","allowance":0.0021}`)
	for name, tc := range map[string]struct {
		adapter codemode.Adapter
		paid    bool
		want    bool
	}{
		"opencode go attempt":     {adapter: codemode.AdapterOpenCodeGo, want: true},
		"other adapter attempt":   {adapter: codemode.AdapterZAICoding},
		"paid opencode-shaped":    {adapter: codemode.AdapterOpenCodeGo, paid: true},
		"unknown adapter attempt": {},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := &chargeLedger{}
			attempt := &codeAttempt{forwarder: &Forwarder{ledger: ledger}, adapter: tc.adapter, permit: codesessions.Permit{Attempt: codemode.Attempt{ID: "attempt-1"}}}
			if tc.paid {
				attempt.paid = &paidCodeAttempt{}
			}
			attempt.observe(t.Context(), codemode.Observation{Terminal: true, AllowanceCharge: reported})
			if tc.want && (ledger.attempt != "attempt-1" || string(ledger.charge) != string(reported)) {
				t.Fatalf("ledger = %s %s, want the reported charge on attempt-1", ledger.attempt, ledger.charge)
			}
			if !tc.want && ledger.charge != nil {
				t.Fatalf("charge stored = %s, want none", ledger.charge)
			}
		})
	}
}

func TestCodeAttemptLastTerminalChargeWins(t *testing.T) {
	ledger := &chargeLedger{}
	attempt := &codeAttempt{forwarder: &Forwarder{ledger: ledger}, adapter: codemode.AdapterOpenCodeGo, permit: codesessions.Permit{Attempt: codemode.Attempt{ID: "attempt-1"}}}
	attempt.observe(t.Context(), codemode.Observation{Terminal: true, AllowanceCharge: json.RawMessage(`{"billing_source":"go","allowance":0.001}`)})
	terminal := json.RawMessage(`{"billing_source":"go","allowance":0.0021}`)
	attempt.observe(t.Context(), codemode.Observation{Terminal: true, AllowanceCharge: terminal})
	if string(ledger.charge) != string(terminal) {
		t.Fatalf("charge = %s, want the last terminal charge %s", ledger.charge, terminal)
	}
}
