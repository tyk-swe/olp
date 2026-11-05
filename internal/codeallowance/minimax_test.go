package codeallowance_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codeallowance"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestParseMiniMaxRemainsMapsNothing(t *testing.T) {
	for name, body := range map[string]string{
		"malformed":              "{",
		"empty":                  "",
		"null":                   "null",
		"empty object":           "{}",
		"array":                  "[]",
		"plausible status only":  `{"status":"ok"}`,
		"codex shaped lookalike": validUsage,
	} {
		allowance, err := codeallowance.ParseMiniMaxRemains([]byte(body), usageNow)
		if !errors.Is(err, codeallowance.ErrMiniMaxRemainsUnqualified) {
			t.Errorf("%s: err = %v, want ErrMiniMaxRemainsUnqualified", name, err)
		}
		if len(allowance.Windows) != 0 || allowance.Credits != nil {
			t.Errorf("%s: allowance = %+v, want no observation", name, allowance)
		}
	}
}

func TestMiniMaxRemainsPinNamesOnlyTheDocumentedGlobalEndpoint(t *testing.T) {
	if codeallowance.MiniMaxRemainsURL != "https://www.minimax.io/v1/token_plan/remains" {
		t.Fatalf("MiniMaxRemainsURL = %q", codeallowance.MiniMaxRemainsURL)
	}
	for _, profile := range []string{codeplans.MiniMaxProfile, codeplans.MiniMaxCNProfile} {
		if codeallowance.MiniMaxRemainsQualified(profile) {
			t.Fatalf("profile %q must stay unqualified until its schema is pinned from real keys", profile)
		}
	}
}

func TestPassNeverReadsAnUnqualifiedMiniMaxAccount(t *testing.T) {
	for _, profile := range []string{codeplans.MiniMaxProfile, codeplans.MiniMaxCNProfile} {
		due := codeallowance.DueAccount{
			Account: codemode.Account{
				ID: "minimax-1", ProjectID: "project", ProviderID: "provider", CredentialID: "credential",
				Principal: "principal", Enabled: true,
			},
			Configuration: routing.Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: profile},
		}
		store := &fakeStore{due: []codeallowance.DueAccount{due}}
		authorizer := &fakeAuthorizer{}
		calls := 0
		transport := roundTripFunc(func(*http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(validUsage))}, nil
		})
		outcome, progress := reader(store, authorizer, fakeLocker{}, transport).Pass(t.Context())
		if outcome != usage.OutcomeSuccess || progress || calls != 0 || authorizer.calls != 0 || len(store.observed) != 0 || len(store.completed) != 0 {
			t.Fatalf("profile %q: outcome %v progress %v calls %d auth %d observed %d completed %v, want a silent skip with no network call", profile, outcome, progress, calls, authorizer.calls, len(store.observed), store.completed)
		}
	}
}
