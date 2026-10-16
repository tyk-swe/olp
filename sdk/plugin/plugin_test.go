package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type manifestOnly Manifest

func (m manifestOnly) Manifest() Manifest { return Manifest(m) }

type signing struct {
	manifestOnly
	sign func(context.Context, SignRequest) (SignResult, error)
}

func (s signing) Sign(ctx context.Context, r SignRequest) (SignResult, error) { return s.sign(ctx, r) }

type routing struct {
	manifestOnly
	match func(RouteFeatures) bool
}

func (r routing) MatchRoute(_ context.Context, features RouteFeatures) (RouteVerdict, error) {
	return RouteVerdict{Match: r.match(features)}, nil
}

type panicking struct{}

func (panicking) Manifest() Manifest { panic("declared nothing") }

func serveWith(t *testing.T, p Plugin, request string) abi.Response {
	t.Helper()
	registered = p
	t.Cleanup(func() { registered = nil })
	var response abi.Response
	if err := json.Unmarshal(serve([]byte(request)), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

// enroller is a plugin whose profile authenticates with a grant.
type enroller struct{ manifestOnly }

// StartGrant signs in to the account the provider's option names.
func (enroller) StartGrant(ctx context.Context, start GrantStart) (GrantAuthorization, error) {
	provider, ok := ProviderOf(ctx)
	if !ok {
		return GrantAuthorization{}, errors.New("the call serves no provider")
	}
	return GrantAuthorization{URL: "https://login.acme.example/authorize?profile=" + start.Profile + "&account=" + provider.Options["account"], Session: "verifier"}, nil
}

func (enroller) ExchangeGrant(_ context.Context, exchange GrantExchange) (Grant, error) {
	switch {
	case exchange.Session != "verifier":
		return Grant{}, errors.New("lost the session")
	case exchange.Input != "code#state":
		return Grant{}, &Error{Code: abi.CodeStateMismatch, Message: "another sign-in"}
	}
	return Grant{AccessToken: "at", Principal: "user@acme.example", Facts: map[string]string{"account": "7"}}, nil
}

// A plugin whose profiles authenticate with static credentials enrolls
// no grants.

// refresher is a plugin whose grants carry a rotating refresh token.
type refresher struct{ enroller }

// RefreshGrant rotates the refresh token for the provider's account, whose
// facts the grant keeps.
func (refresher) RefreshGrant(ctx context.Context, refresh GrantRefresh) (Grant, error) {
	provider, ok := ProviderOf(ctx)
	switch {
	case !ok:
		return Grant{}, errors.New("the call serves no provider")
	case refresh.RefreshToken != "rt-1":
		return Grant{}, &Error{Code: abi.CodeInvalidGrant, Message: "the refresh token is spent"}
	}
	return Grant{AccessToken: "at-2 " + provider.Options["account"] + " " + refresh.Facts["account"], RefreshToken: "rt-2", ExpiresIn: 3600}, nil
}

// A plugin whose grants carry no refresh token refreshes none.

// poller is a plugin that enrolls grants by device authorization.
type poller struct{ enroller }

// PollGrant reports the device approved once the session says so.
func (poller) PollGrant(_ context.Context, poll GrantPoll) (Grant, error) {
	if poll.Session != "approved" {
		return Grant{}, &Error{Code: abi.CodeAuthorizationPending, Message: "not yet"}
	}
	return Grant{AccessToken: "at", Principal: "device@acme.example"}, nil
}

func TestServeReportsFailuresAsErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		plugin  Plugin
		request string
		code    string
	}{
		"unknown method":     {manifestOnly{}, `{"method":"sign"}`, abi.CodeUnknownMethod},
		"malformed":          {manifestOnly{}, `{"method":`, abi.CodeInvalidRequest},
		"nothing registered": {nil, `{"method":"manifest"}`, abi.CodeInternal},
		"panic":              {panicking{}, `{"method":"manifest"}`, abi.CodeInternal},
		"malformed sign":     {signing{}, `{"method":"sign","params":[]}`, abi.CodeInvalidRequest},
		"reported": {signing{sign: func(context.Context, SignRequest) (SignResult, error) {
			return SignResult{}, &Error{Code: "expired", Message: "The key expired."}
		}}, `{"method":"sign","params":{}}`, "expired"},
		"failed": {signing{sign: func(context.Context, SignRequest) (SignResult, error) { return SignResult{}, errors.New("no clock") }}, `{"method":"sign","params":{}}`, abi.CodeInternal},
	} {
		t.Run(name, func(t *testing.T) {
			if response := serveWith(t, tc.plugin, tc.request); response.Error == nil || response.Error.Code != tc.code {
				t.Fatalf("want %s, got %+v", tc.code, response)
			}
		})
	}
}

func TestLogRecordsFlattenAttributes(t *testing.T) {
	handler := hostHandler{}.WithAttrs([]slog.Attr{slog.String("plugin", "acme")}).WithGroup("grant").(hostHandler)
	r := slog.NewRecord(time.Time{}, slog.LevelWarn+1, "refresh failed", 0)
	r.AddAttrs(slog.Int("attempt", 2), slog.Group("upstream", slog.Int("status", 401)))
	got := handler.record(r)
	want := abi.LogRecord{Level: "warn", Message: "refresh failed", Attrs: map[string]string{"plugin": "acme", "grant.attempt": "2", "grant.upstream.status": "401"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("record %+v, want %+v", got, want)
	}
}
