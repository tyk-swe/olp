package codeallowance

import (
	"context"
	"errors"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/routing"
)

type fakeStore struct {
	due         []DueAccount
	dueErr      error
	observed    []codemode.Allowance
	completed   []string
	invalidated []string
}

func (s *fakeStore) DueAllowanceReads(context.Context, time.Time, int, []string) ([]DueAccount, error) {
	return s.due, s.dueErr
}

func (s *fakeStore) ObserveAllowance(_ context.Context, _ string, allowance codemode.Allowance) error {
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

func (a *fakeAuthorizer) AuthorizeCode(context.Context, routing.Configuration, codemode.Account, codemode.Dispatch) (codemode.Authorization, error) {
	a.calls++
	if a.err != nil {
		return codemode.Authorization{}, a.err
	}
	return codemode.Authorization{Principal: "principal", Headers: http.Header{
		"Authorization":      {"Bearer token"},
		"Chatgpt-Account-Id": {"account-1"},
	}}, nil
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

func dueAccount() DueAccount {
	return DueAccount{
		Account: codemode.Account{
			ID: "account-1", ProjectID: "project", ProviderID: "provider", CredentialID: "credential",
			Principal: "principal", Enabled: true,
		},
		Configuration: routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID}},
	}
}

func reader(store *fakeStore, authorizer *fakeAuthorizer, locker fakeLocker, transport http.RoundTripper) *Reader {
	return &Reader{
		Store: store, Authorizer: authorizer, Locker: locker,
		Client: &http.Client{Transport: transport},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return usageNow },
	}
}

func readFixture(ctx context.Context, r *Reader, entry DueAccount) (workerhealth.Outcome, bool) {
	progress, err := r.read(ctx, entry)
	if err != nil {
		return workerhealth.OutcomeFailure, progress
	}
	return workerhealth.OutcomeSuccess, progress
}

func TestDueOperatorRequestAndStaleActiveEvidence(t *testing.T) {
	freshActive := &codemode.Allowance{ObservedAt: usageNow, Windows: []codemode.AllowanceWindow{{
		LimitID: "codex", Window: "primary", Unit: codemode.AllowanceUnitPercent, Evidence: codemode.AllowanceEvidenceActive,
		UsedPercent: 10, RemainingPercent: 90, ObservedAt: usageNow.Add(-time.Minute),
	}}}
	staleActive := &codemode.Allowance{ObservedAt: usageNow, Windows: []codemode.AllowanceWindow{{
		LimitID: "codex", Window: "primary", Unit: codemode.AllowanceUnitPercent, Evidence: codemode.AllowanceEvidenceActive,
		UsedPercent: 10, RemainingPercent: 90, ObservedAt: usageNow.Add(-10 * time.Minute),
	}}}
	freshPassive := &codemode.Allowance{ObservedAt: usageNow, Windows: []codemode.AllowanceWindow{{
		LimitID: "codex", Window: "primary", Unit: codemode.AllowanceUnitPercent, Evidence: codemode.AllowanceEvidencePassive,
		UsedPercent: 10, RemainingPercent: 90, ObservedAt: usageNow.Add(-time.Minute),
	}}}
	requested := usageNow.Add(-time.Second)
	readLongAgo := usageNow.Add(-time.Hour)
	readJustNow := usageNow.Add(-time.Minute)
	cases := []struct {
		name    string
		account codemode.Account
		want    bool
	}{
		{"operator requested", codemode.Account{Allowance: freshActive, AllowanceReadRequestedAt: &requested}, true},
		{"request already read", codemode.Account{Allowance: freshActive, AllowanceReadRequestedAt: &readLongAgo, AllowanceReadAt: &readJustNow}, false},
		{"fresh active", codemode.Account{Allowance: freshActive}, false},
		{"stale active", codemode.Account{Allowance: staleActive}, true},
		{"passive does not count", codemode.Account{Allowance: freshPassive}, true},
		{"no allowance", codemode.Account{}, true},
		{"recent failed read backs off", codemode.Account{AllowanceReadAt: &readJustNow}, false},
		{"old failed read retries", codemode.Account{AllowanceReadAt: &readLongAgo}, true},
	}
	for _, tc := range cases {
		if got := Due(tc.account, usageNow); got != tc.want {
			t.Errorf("%s: Due = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestReadWritesActiveAllowanceFromUpstream(t *testing.T) {
	store := &fakeStore{due: []DueAccount{dueAccount()}}
	var seen *http.Request
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(validUsage))}, nil
	})
	outcome, progress := readFixture(t.Context(), reader(store, &fakeAuthorizer{}, fakeLocker{}, transport), store.due[0])
	if outcome != workerhealth.OutcomeSuccess || !progress {
		t.Fatalf("pass = %v, progress %v", outcome, progress)
	}
	if seen == nil || seen.Method != http.MethodGet || seen.URL.String() != UsageURL {
		t.Fatalf("request = %+v", seen)
	}
	if seen.Header.Get("Authorization") != "Bearer token" || seen.Header.Get("Chatgpt-Account-Id") != "account-1" {
		t.Fatalf("credential headers must come from the authorizer, got %v", seen.Header)
	}
	if len(store.observed) != 1 || len(store.completed) != 1 {
		t.Fatalf("observed %v completed %v", store.observed, store.completed)
	}
	for _, w := range store.observed[0].Windows {
		if w.Evidence != codemode.AllowanceEvidenceActive {
			t.Fatalf("written window evidence = %q", w.Evidence)
		}
	}
}

func TestPassSkipsAccountAnotherWorkerHolds(t *testing.T) {
	store := &fakeStore{due: []DueAccount{dueAccount()}}
	authorizer := &fakeAuthorizer{}
	calls := 0
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(validUsage))}, nil
	})
	outcome, progress := reader(store, authorizer, fakeLocker{held: map[string]bool{"account-1": true}}, transport).Pass(t.Context())
	if outcome != workerhealth.OutcomeSuccess || progress || calls != 0 || authorizer.calls != 0 || len(store.completed) != 0 {
		t.Fatalf("held lock must skip without a read: outcome %v progress %v calls %d completed %v", outcome, progress, calls, store.completed)
	}
}

func TestReadMalformedOrFailedReadInvalidatesActiveEvidence(t *testing.T) {
	for name, transport := range map[string]http.RoundTripper{
		"malformed": roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"plan_type": 4}`))}, nil
		}),
		"upstream error": roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(validUsage))}, nil
		}),
		"transport error": roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		}),
	} {
		store := &fakeStore{due: []DueAccount{dueAccount()}}
		outcome, progress := readFixture(t.Context(), reader(store, &fakeAuthorizer{}, fakeLocker{}, transport), store.due[0])
		if outcome != workerhealth.OutcomeSuccess || progress || len(store.observed) != 0 || len(store.completed) != 1 || len(store.invalidated) != 1 {
			t.Errorf("%s: outcome %v progress %v observed %d completed %v", name, outcome, progress, len(store.observed), store.completed)
		}
	}
	store := &fakeStore{due: []DueAccount{dueAccount()}}
	outcome, _ := readFixture(t.Context(), reader(store, &fakeAuthorizer{err: errors.New("no credential")}, fakeLocker{}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("no upstream call without authorization")
		return nil, errors.New("unreachable")
	})), store.due[0])
	if outcome != workerhealth.OutcomeSuccess || len(store.observed) != 0 || len(store.completed) != 1 || len(store.invalidated) != 1 {
		t.Fatalf("authorize failure: outcome %v observed %d completed %v", outcome, len(store.observed), store.completed)
	}
}

func TestPassFailureOnlyWhenDueCannotBeRead(t *testing.T) {
	store := &fakeStore{dueErr: errors.New("database down")}
	outcome, progress := reader(store, &fakeAuthorizer{}, fakeLocker{}, nil).Pass(t.Context())
	if outcome != workerhealth.OutcomeFailure || progress {
		t.Fatalf("pass = %v, progress %v", outcome, progress)
	}
}

// Source: MiniMax-AI/cli@38aba5f endpoints.ts, fetched 2026-11-05.
func TestReadWritesActiveAllowanceFromMiniMaxRemains(t *testing.T) {
	for _, tc := range []struct {
		profile string
		url     string
	}{
		{codeplans.MiniMaxProfile, "https://api.minimax.io/v1/token_plan/remains"},
		{codeplans.MiniMaxCNProfile, "https://api.minimax.cn/v1/token_plan/remains"},
	} {
		due := DueAccount{
			Account: codemode.Account{
				ID: "account-1", ProjectID: "project", ProviderID: "provider", CredentialID: "credential",
				Principal: "principal", Enabled: true, Models: []string{"MiniMax-M3"},
			},
			Configuration: routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: tc.profile}},
		}
		store := &fakeStore{due: []DueAccount{due}}
		var seen *http.Request
		transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
			seen = r
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(minimaxBody(minimaxBucket("MiniMax-M3", ""))))}, nil
		})
		outcome, progress := readFixture(t.Context(), reader(store, &fakeAuthorizer{}, fakeLocker{}, transport), store.due[0])
		if outcome != workerhealth.OutcomeSuccess || !progress {
			t.Fatalf("profile %q pass = %v, progress %v", tc.profile, outcome, progress)
		}
		if seen == nil || seen.Method != http.MethodGet || seen.URL.String() != tc.url {
			t.Fatalf("profile %q request = %+v, want GET %s", tc.profile, seen, tc.url)
		}
		if seen.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("profile %q remains must carry the authorized bearer credential, got %v", tc.profile, seen.Header)
		}
		if len(store.observed) != 1 || len(store.completed) != 1 {
			t.Fatalf("profile %q observed %v completed %v", tc.profile, store.observed, store.completed)
		}
		primary := window(t, store.observed[0], "minimax", "primary")
		weekly := window(t, store.observed[0], "minimax", "weekly")
		if primary.Evidence != codemode.AllowanceEvidenceActive || weekly.Evidence != codemode.AllowanceEvidenceActive ||
			primary.RemainingPercent != 80 || weekly.RemainingPercent != 60 {
			t.Fatalf("profile %q windows = %+v", tc.profile, store.observed[0].Windows)
		}
	}
}

func TestPassDispatchesOnlyQualifiedAdaptersToAnActiveRead(t *testing.T) {
	for _, profile := range []string{codexauth.ProfileID, codeplans.MiniMaxProfile, codeplans.MiniMaxCNProfile, codeplans.ZAIProfile, codeplans.KimiProfile, codeplans.QwenTokenPlanProfile, codeplans.OpenCodeGoProfile, "unknown-profile"} {
		assertPassSkipsProfile(t, profile)
	}
}

func assertPassSkipsProfile(t *testing.T, profile string) {
	t.Helper()
	due := DueAccount{
		Account: codemode.Account{
			ID: "account-1", ProjectID: "project", ProviderID: "provider", CredentialID: "credential",
			Principal: "principal", Enabled: true,
		},
		Configuration: routing.Configuration{Identity: providerconfig.Identity[string]{Kind: "plugin", AuthMode: "grant", ProfileID: profile}},
	}
	store := &fakeStore{due: []DueAccount{due}}
	authorizer := &fakeAuthorizer{}
	calls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		t.Errorf("profile %q must not call %s", profile, r.URL)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(validUsage))}, nil
	})
	outcome, progress := reader(store, authorizer, fakeLocker{}, transport).Pass(t.Context())
	if outcome != workerhealth.OutcomeSuccess || progress || calls != 0 || authorizer.calls != 0 || len(store.observed) != 0 || len(store.completed) != 0 {
		t.Fatalf("profile %q: outcome %v progress %v calls %d auth %d observed %d completed %v, want a silent skip with no network call", profile, outcome, progress, calls, authorizer.calls, len(store.observed), store.completed)
	}
}
