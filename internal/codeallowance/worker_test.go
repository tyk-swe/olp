package codeallowance_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeallowance"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

type fakeStore struct {
	due       []codeallowance.DueAccount
	dueErr    error
	observed  []codemode.Allowance
	completed []string
}

func (s *fakeStore) DueAllowanceReads(context.Context, time.Time, int, []string) ([]codeallowance.DueAccount, error) {
	return s.due, s.dueErr
}

func (s *fakeStore) ObserveAllowance(_ context.Context, _ string, allowance codemode.Allowance) error {
	s.observed = append(s.observed, allowance)
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

func dueAccount() codeallowance.DueAccount {
	return codeallowance.DueAccount{
		Account: codemode.Account{
			ID: "account-1", ProjectID: "project", ProviderID: "provider", CredentialID: "credential",
			Principal: "principal", Enabled: true,
		},
		Configuration: routing.Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID},
	}
}

func reader(store *fakeStore, authorizer *fakeAuthorizer, locker fakeLocker, transport http.RoundTripper) *codeallowance.Reader {
	return &codeallowance.Reader{
		Store: store, Authorizer: authorizer, Locker: locker,
		Client: &http.Client{Transport: transport},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return usageNow },
	}
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
		if got := codeallowance.Due(tc.account, usageNow); got != tc.want {
			t.Errorf("%s: Due = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestPassWritesActiveAllowanceFromUpstream(t *testing.T) {
	store := &fakeStore{due: []codeallowance.DueAccount{dueAccount()}}
	var seen *http.Request
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		seen = r
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(validUsage))}, nil
	})
	outcome, progress := reader(store, &fakeAuthorizer{}, fakeLocker{}, transport).Pass(t.Context())
	if outcome != usage.OutcomeSuccess || !progress {
		t.Fatalf("pass = %v, progress %v", outcome, progress)
	}
	if seen == nil || seen.Method != http.MethodGet || seen.URL.String() != codeallowance.UsageURL {
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
	store := &fakeStore{due: []codeallowance.DueAccount{dueAccount()}}
	authorizer := &fakeAuthorizer{}
	calls := 0
	transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(validUsage))}, nil
	})
	outcome, progress := reader(store, authorizer, fakeLocker{held: map[string]bool{"account-1": true}}, transport).Pass(t.Context())
	if outcome != usage.OutcomeSuccess || progress || calls != 0 || authorizer.calls != 0 || len(store.completed) != 0 {
		t.Fatalf("held lock must skip without a read: outcome %v progress %v calls %d completed %v", outcome, progress, calls, store.completed)
	}
}

func TestPassMalformedOrFailedReadWritesNothing(t *testing.T) {
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
		store := &fakeStore{due: []codeallowance.DueAccount{dueAccount()}}
		outcome, progress := reader(store, &fakeAuthorizer{}, fakeLocker{}, transport).Pass(t.Context())
		if outcome != usage.OutcomeSuccess || progress || len(store.observed) != 0 || len(store.completed) != 1 {
			t.Errorf("%s: outcome %v progress %v observed %d completed %v", name, outcome, progress, len(store.observed), store.completed)
		}
	}
	store := &fakeStore{due: []codeallowance.DueAccount{dueAccount()}}
	outcome, _ := reader(store, &fakeAuthorizer{err: errors.New("no credential")}, fakeLocker{}, roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("no upstream call without authorization")
		return nil, errors.New("unreachable")
	})).Pass(t.Context())
	if outcome != usage.OutcomeSuccess || len(store.observed) != 0 || len(store.completed) != 1 {
		t.Fatalf("authorize failure: outcome %v observed %d completed %v", outcome, len(store.observed), store.completed)
	}
}

func TestPassFailureOnlyWhenDueCannotBeRead(t *testing.T) {
	store := &fakeStore{dueErr: errors.New("database down")}
	outcome, progress := reader(store, &fakeAuthorizer{}, fakeLocker{}, nil).Pass(t.Context())
	if outcome != usage.OutcomeFailure || progress {
		t.Fatalf("pass = %v, progress %v", outcome, progress)
	}
}

func TestPassNeverReadsAnUnqualifiedMiniMaxAccount(t *testing.T) {
	for _, profile := range []string{codeplans.MiniMaxProfile, codeplans.MiniMaxCNProfile} {
		assertPassSkipsProfile(t, profile)
	}
}

func TestPassDispatchesOnlyCodexToTheUsageRead(t *testing.T) {
	for _, profile := range []string{codeplans.MiniMaxProfile, codeplans.MiniMaxCNProfile, codeplans.ZAIProfile, codeplans.KimiProfile, codeplans.QwenTokenPlanProfile, codeplans.OpenCodeGoProfile, "unknown-profile"} {
		assertPassSkipsProfile(t, profile)
	}
}

func assertPassSkipsProfile(t *testing.T, profile string) {
	t.Helper()
	due := codeallowance.DueAccount{
		Account: codemode.Account{
			ID: "account-1", ProjectID: "project", ProviderID: "provider", CredentialID: "credential",
			Principal: "principal", Enabled: true,
		},
		Configuration: routing.Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: profile},
	}
	store := &fakeStore{due: []codeallowance.DueAccount{due}}
	authorizer := &fakeAuthorizer{}
	calls := 0
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		t.Errorf("profile %q must not call %s", profile, r.URL)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(validUsage))}, nil
	})
	outcome, progress := reader(store, authorizer, fakeLocker{}, transport).Pass(t.Context())
	if outcome != usage.OutcomeSuccess || progress || calls != 0 || authorizer.calls != 0 || len(store.observed) != 0 || len(store.completed) != 0 {
		t.Fatalf("profile %q: outcome %v progress %v calls %d auth %d observed %d completed %v, want a silent skip with no network call", profile, outcome, progress, calls, authorizer.calls, len(store.observed), store.completed)
	}
}
