package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
)

type recordedCooldown struct {
	account        string
	model          string
	classification codeadapter.Classification
}

type fakeCodeLedger struct {
	health      []codemode.HealthObservation
	cooldowns   []recordedCooldown
	cooldownErr error
}

func (f *fakeCodeLedger) Models(context.Context, codemode.Route, string) (codeclient.Offers, error) {
	return codeclient.Offers{}, nil
}
func (f *fakeCodeLedger) Admit(context.Context, resources.CodeAdmission) (resources.CodePermit, error) {
	return resources.CodePermit{}, nil
}
func (f *fakeCodeLedger) BindConnection(context.Context, codemode.Route, string, codemode.Identity, string, []string) (resources.CodePermit, error) {
	return resources.CodePermit{}, nil
}
func (f *fakeCodeLedger) MarkDispatched(context.Context, string) error { return nil }
func (f *fakeCodeLedger) ConfirmNonexecution(context.Context, string, int) error {
	return nil
}
func (f *fakeCodeLedger) ObserveCooldown(context.Context, string, time.Time) error { return nil }
func (f *fakeCodeLedger) ObserveCooldownClass(_ context.Context, account, model string, classification codeadapter.Classification, _ time.Time) error {
	if f.cooldownErr != nil {
		return f.cooldownErr
	}
	f.cooldowns = append(f.cooldowns, recordedCooldown{account: account, model: model, classification: classification})
	return nil
}
func (f *fakeCodeLedger) Settle(context.Context, string, codemode.Usage) error { return nil }
func (f *fakeCodeLedger) Abort(context.Context, string) error                  { return nil }
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
func (f *fakeCodeLedger) ObserveOutcome(context.Context, string, codemode.Outcome) error {
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
	server := &Server{CodeLedger: ledger, now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &codeAttempt{
		server:      server,
		permit:      resources.CodePermit{Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID},
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
	if got.account != "account" || got.model != "public-model" || got.classification.Class != codeadapter.CooldownCapacity || got.classification.Scope != codeadapter.ScopeModel || !got.classification.RetryAt.Equal(now.Add(120*time.Second)) {
		t.Fatalf("cooldown = %+v", got)
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
	server := &Server{CodeLedger: ledger, now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &codeAttempt{
		server:      server,
		permit:      resources.CodePermit{InitialRecovery: true, Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.ZAIProfile},
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

func TestCodeAttemptGLMProvenQuotaRecoversAndAmbiguousDoesNot(t *testing.T) {
	// Source: https://docs.z.ai/api-reference/api-code (fetched 2026-11-05), code 1308.
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	reset := now.Add(2 * time.Hour)
	route := codemode.Route{InitialRecoveryAttempts: 1}
	proven := glmAttempt(t, &fakeCodeLedger{}, now, "v1/chat/completions")
	response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"1308","message":"Usage limit reached for 5 hours. Your limit will reset at ` + reset.Format(time.RFC3339) + `"}}`))}
	recovery, err := proven.recover(t.Context(), response, route, nil)
	if err != nil || recovery == nil || recovery.Envelope != "glm_1308" || recovery.Number != 1 || recovery.From != "attempt" {
		t.Fatalf("recover(proven) = %+v, %v", recovery, err)
	}
	ambiguous := glmAttempt(t, &fakeCodeLedger{}, now, "v1/chat/completions")
	response = &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"1308","message":"Usage limit reached for 5 hours. Your limit will reset at ` + reset.Format(time.RFC3339) + `"},"extra":1}`))}
	recovery, err = ambiguous.recover(t.Context(), response, route, nil)
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
	server := &Server{CodeLedger: ledger, now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &codeAttempt{
		server:      server,
		permit:      resources.CodePermit{InitialRecovery: true, Account: codemode.Account{ID: "account", CycleAnchor: anchor}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.QwenTokenPlanProfile},
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
	server := &Server{CodeLedger: ledger, now: func() time.Time { return now }, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return &codeAttempt{
		server:      server,
		permit:      resources.CodePermit{InitialRecovery: true, Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:      routing.Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.MiniMaxProfile},
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

func TestCodeAttemptMiniMaxUsageAndAmbiguousDoNotRecover(t *testing.T) {
	// Sources: https://platform.minimax.io/docs/api-reference/errorcode (fetched 2026-11-05), codes 2056 and 2062, and https://platform.minimax.io/docs/api-reference/speech-to-text (fetched 2026-11-05), which documents the OaiError envelope only for speech-to-text. It is not proven for chat/messages inference, so replay waits for live envelope qualification and neither shape replays today
	now := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	route := codemode.Route{InitialRecoveryAttempts: 1}
	proven := minimaxAttempt(t, &fakeCodeLedger{}, now, "v1/chat/completions")
	response := &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"usage limit exceeded (2056)","http_code":"429"},"request_id":"r"}`))}
	recovery, err := proven.recover(t.Context(), response, route, nil)
	if err != nil || recovery != nil {
		t.Fatalf("recover(proven) = %+v, %v, want no replay pending live envelope qualification", recovery, err)
	}
	ambiguous := minimaxAttempt(t, &fakeCodeLedger{}, now, "v1/chat/completions")
	response = &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"rate_limit_error","message":"M Plan rate limit reached (2062)","http_code":"429"},"request_id":"r"}`))}
	recovery, err = ambiguous.recover(t.Context(), response, route, nil)
	if err != nil || recovery != nil {
		t.Fatalf("recover(ambiguous) = %+v, %v, want no replay", recovery, err)
	}
}
