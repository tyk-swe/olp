package allowance

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/routing"
)

// Fixture fields pinned from openai/codex c0100ad and MiniMax-AI/cli@38aba5f (fetched 2026-11-05).
var usageNow = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

const validUsage = `{
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true,
    "limit_reached": false,
    "primary_window": {"used_percent": 25, "limit_window_seconds": 18000, "reset_after_seconds": 3600, "reset_at": 1794038400},
    "secondary_window": {"used_percent": 40, "limit_window_seconds": 604800, "reset_after_seconds": 7200, "reset_at": 1794643200}
  },
  "credits": {"has_credits": true, "unlimited": false, "balance": "12.50"},
  "additional_rate_limits": null
}`

const (
	remainsStart     = int64(1776355200000)
	remainsEnd       = remainsStart + 18000000
	remainsWeeklyEnd = remainsStart + 604800000
)

func minimaxBucket(model, extra string) string {
	return fmt.Sprintf(`{"model_name":%q,"start_time":%d,"end_time":%d,"remains_time":18000000,`+
		`"current_interval_total_count":100,"current_interval_usage_count":80,"current_interval_remaining_percent":80,`+
		`"current_weekly_total_count":500,"current_weekly_usage_count":400,"current_weekly_remaining_percent":60,`+
		`"current_interval_status":1,"current_weekly_status":1,`+
		`"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":604800000%s}`,
		model, remainsStart, remainsEnd, remainsStart, remainsWeeklyEnd, extra)
}

func minimaxBody(buckets ...string) string {
	return `{"model_remains":[` + strings.Join(buckets, ",") + `],"base_resp":{"status_code":0,"status_msg":"success"}}`
}

type fakeStore struct {
	due         []DueAccount
	dueErr      error
	observed    []code.Allowance
	completed   []string
	invalidated []string
}

func (s *fakeStore) DueAllowanceReads(context.Context, time.Time, int, []string) ([]DueAccount, error) {
	return s.due, s.dueErr
}
func (s *fakeStore) ObserveAllowance(_ context.Context, _ string, allowance code.Allowance) error {
	s.observed = append(s.observed, allowance)
	return nil
}
func (s *fakeStore) InvalidateActiveAllowance(_ context.Context, account string) error {
	s.invalidated = append(s.invalidated, account)
	return nil
}
func (s *fakeStore) CompleteAllowanceRead(_ context.Context, account string, _ time.Time) error {
	s.completed = append(s.completed, account)
	return nil
}

type fakeAuthorizer struct {
	err   error
	calls int
}

func (a *fakeAuthorizer) AuthorizeCode(context.Context, routing.Configuration, code.Account, code.Dispatch) (code.Authorization, error) {
	a.calls++
	if a.err != nil {
		return code.Authorization{}, a.err
	}
	return code.Authorization{Principal: "principal", Headers: http.Header{"Authorization": {"Bearer token"}, "Chatgpt-Account-Id": {"account-1"}}}, nil
}

type fakeLocker struct{ held map[string]bool }

func (l fakeLocker) TryLock(_ context.Context, key string, run func() (bool, error)) (bool, bool, error) {
	if l.held[key] {
		return false, false, nil
	}
	progress, err := run()
	return true, progress, err
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestParseUsageAndRemainsAndWorkerDispatch(t *testing.T) {
	parsed, ok := ParseUsage([]byte(validUsage), usageNow)
	if !ok || len(parsed.Windows) != 2 || parsed.RemainingPercent != nil || parsed.Credits == nil || *parsed.Credits.Balance != "12.50" {
		t.Fatalf("ParseUsage = %+v, %v", parsed, ok)
	}
	if _, ok := ParseUsage([]byte(`{"plan_type": 3}`), usageNow); ok {
		t.Fatal("malformed usage accepted")
	}

	remains, ok := ParseRemains([]byte(minimaxBody(minimaxBucket("speech-hd", ""), minimaxBucket("MiniMax-M*", ""))), nil, usageNow)
	if !ok || len(remains.Windows) != 2 || remains.Windows[0].RemainingPercent != 80 {
		t.Fatalf("ParseRemains = %+v, %v", remains, ok)
	}
	if _, ok := ParseRemains([]byte(`{"model_remains":[{"model_name":"speech-hd"}`), nil, usageNow); ok {
		t.Fatal("malformed remains accepted")
	}

	r := &Reader{}
	for _, adapter := range []code.Adapter{code.AdapterZAICoding, code.AdapterKimiCode, code.AdapterQwenTokenPlan, code.AdapterClaude, code.AdapterOpenCodeGo, code.AdapterAntigravity, ""} {
		if _, _, err := r.fetchForAdapter(context.Background(), adapter, DueAccount{}, usageNow); !errors.Is(err, errAllowanceReadUnqualified) {
			t.Fatalf("adapter %q err = %v", adapter, err)
		}
	}

	due := DueAccount{
		Account:       code.Account{ID: "account-1", ProjectID: "project", ProviderID: "provider", CredentialID: "credential", Principal: "principal", Enabled: true},
		Configuration: routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID}},
	}
	store := &fakeStore{due: []DueAccount{due}}
	worker := &Reader{
		Store: store, Authorizer: &fakeAuthorizer{}, Locker: fakeLocker{},
		Client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(validUsage))}, nil
		})},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: func() time.Time { return usageNow },
	}
	if progress, err := worker.read(t.Context(), due); err != nil || !progress || len(store.observed) != 1 || len(store.completed) != 1 {
		t.Fatalf("worker read = %v, %v, observed=%d completed=%v", progress, err, len(store.observed), store.completed)
	}

	for _, profile := range []string{codexauth.ProfileID, codeplans.MiniMaxProfile, codeplans.ZAIProfile} {
		unqualifiedDue := DueAccount{Account: due.Account, Configuration: routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: profile}}}
		s := &fakeStore{due: []DueAccount{unqualifiedDue}}
		w := &Reader{Store: s, Authorizer: &fakeAuthorizer{}, Locker: fakeLocker{}, Log: worker.Log, Now: worker.Now}
		if outcome, progress := w.Pass(t.Context()); outcome != workerhealth.OutcomeSuccess || progress || len(s.observed) != 0 {
			t.Fatalf("profile %q pass = %v, %v", profile, outcome, progress)
		}
	}
}
