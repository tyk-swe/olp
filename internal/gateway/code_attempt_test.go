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
		server:   server,
		permit:   resources.CodePermit{Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:   routing.Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID},
		endpoint: endpoint,
		observer: codeObserver(endpoint),
		model:    "gpt-model",
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
		server:   server,
		permit:   resources.CodePermit{InitialRecovery: true, Account: codemode.Account{ID: "account"}, Attempt: codemode.Attempt{ID: "attempt"}},
		config:   routing.Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: codeplans.ZAIProfile},
		endpoint: endpoint,
		observer: codeObserver(endpoint),
		model:    "glm-model",
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
