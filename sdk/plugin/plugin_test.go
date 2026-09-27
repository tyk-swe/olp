package plugin

import (
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

func TestServeAnswersTheManifestCall(t *testing.T) {
	declared := Manifest{Name: "acme", Version: "1.0.0", Origins: []string{"https://api.acme.example"}, Profiles: []Profile{{ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat"}}}
	response := serveWith(t, manifestOnly(declared), `{"method":"manifest","params":null}`)
	var got Manifest
	if response.Error != nil || json.Unmarshal(response.Result, &got) != nil || !reflect.DeepEqual(got, declared) {
		t.Fatalf("manifest response %+v", response)
	}
}

// enroller is a plugin whose profile authenticates with a grant.
type enroller struct{ manifestOnly }

func (enroller) StartGrant(start GrantStart) (GrantAuthorization, error) {
	return GrantAuthorization{URL: "https://login.acme.example/authorize?profile=" + start.Profile, Session: "verifier"}, nil
}

func (enroller) ExchangeGrant(exchange GrantExchange) (Grant, error) {
	switch {
	case exchange.Session != "verifier":
		return Grant{}, errors.New("lost the session")
	case exchange.Input != "code#state":
		return Grant{}, &Error{Code: abi.CodeStateMismatch, Message: "another sign-in"}
	}
	return Grant{AccessToken: "at", Principal: "user@acme.example", Facts: map[string]string{"account": "7"}}, nil
}

func TestServeRunsGrantEnrollmentSteps(t *testing.T) {
	response := serveWith(t, enroller{}, `{"method":"grant_start","params":{"profile":"acme-account"}}`)
	var authorization GrantAuthorization
	if response.Error != nil || json.Unmarshal(response.Result, &authorization) != nil || authorization.URL != "https://login.acme.example/authorize?profile=acme-account" || authorization.Session != "verifier" {
		t.Fatalf("grant_start response %+v", response)
	}
	response = serveWith(t, enroller{}, `{"method":"grant_exchange","params":{"profile":"acme-account","session":"verifier","input":"code#state"}}`)
	var grant Grant
	if response.Error != nil || json.Unmarshal(response.Result, &grant) != nil || grant.AccessToken != "at" || grant.Facts["account"] != "7" {
		t.Fatalf("grant_exchange response %+v", response)
	}
	for request, want := range map[string]abi.Error{
		`{"method":"grant_exchange","params":{"session":"verifier","input":"other"}}`:   {Code: abi.CodeStateMismatch, Message: "another sign-in"},
		`{"method":"grant_exchange","params":{"session":"other","input":"code#state"}}`: {Code: abi.CodeInternal, Message: "lost the session"},
		`{"method":"grant_exchange","params":{"session":7}}`:                            {Code: abi.CodeInvalidRequest, Message: "The parameters do not match the method."},
	} {
		if response := serveWith(t, enroller{}, request); response.Error == nil || *response.Error != want {
			t.Errorf("%s: %+v", request, response.Error)
		}
	}
	// A plugin whose profiles authenticate with static credentials enrolls
	// no grants.
	if response := serveWith(t, manifestOnly{}, `{"method":"grant_start","params":{}}`); response.Error == nil || response.Error.Code != abi.CodeUnknownMethod {
		t.Fatalf("a plugin without grant enrollment answered %+v", response)
	}
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
