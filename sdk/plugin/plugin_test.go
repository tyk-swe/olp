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

// A Signer signs with the request and the provider it is for, whose option
// values ProviderOf returns.
func TestServeAnswersTheSignCall(t *testing.T) {
	var got SignRequest
	var provider Provider
	var found bool
	p := signing{sign: func(ctx context.Context, r SignRequest) (SignResult, error) {
		got = r
		provider, found = ProviderOf(ctx)
		return SignResult{Headers: map[string]string{"X-Signature": "signed"}}, nil
	}}
	response := serveWith(t, p, `{"method":"sign","params":{"profile":"acme-chat","method":"POST","url":"https://api.acme.example/v1/chat/completions","header":{"Accept":["application/json"]},"body":"e30=","credential":"sk-acme"},"provider":{"profile":"acme-chat","options":{"account":"acme"}}}`)
	want := SignRequest{Profile: "acme-chat", Method: "POST", URL: "https://api.acme.example/v1/chat/completions", Header: map[string][]string{"Accept": {"application/json"}}, Body: []byte("{}"), Credential: "sk-acme"}
	if response.Error != nil || string(response.Result) != `{"headers":{"X-Signature":"signed"}}` || !reflect.DeepEqual(got, want) {
		t.Fatalf("sign response %+v for %+v", response, got)
	}
	if !found || !reflect.DeepEqual(provider, Provider{Profile: "acme-chat", Options: map[string]string{"account": "acme"}}) {
		t.Fatalf("signed for provider %+v", provider)
	}
}

// A plugin can't declare a signing profile it has no Signer for: it reports
// no manifest, so OLP never installs it.
func TestServeRefusesASigningProfileWithoutASigner(t *testing.T) {
	declared := Manifest{Name: "acme", Version: "1.0.0", Profiles: []Profile{{ID: "acme-chat", Label: "Acme Chat", Dialect: "openai-chat", Signing: true}}}
	if response := serveWith(t, manifestOnly(declared), `{"method":"manifest"}`); response.Error == nil || response.Error.Code != abi.CodeInternal {
		t.Fatalf("manifest response %+v", response)
	}
	if response := serveWith(t, signing{manifestOnly: manifestOnly(declared)}, `{"method":"manifest"}`); response.Error != nil {
		t.Fatalf("manifest response %+v", response)
	}
}

// A method OLP calls on behalf of a provider finds the provider's profile and
// option values in its context; any other call's context has none.
func TestServeHandsMethodsTheProviderTheyServe(t *testing.T) {
	methods["provider_method"] = func(ctx context.Context, _ json.RawMessage) (any, error) {
		provider, ok := ProviderOf(ctx)
		return map[string]any{"ok": ok, "provider": provider}, nil
	}
	t.Cleanup(func() { delete(methods, "provider_method") })
	for request, want := range map[string]string{
		`{"method":"provider_method","provider":{"profile":"acme-chat","options":{"account":"acme"}}}`: `{"ok":true,"provider":{"profile":"acme-chat","options":{"account":"acme"}}}`,
		`{"method":"provider_method"}`: `{"ok":false,"provider":{"profile":"","options":null}}`,
	} {
		if response := serveWith(t, manifestOnly{}, request); response.Error != nil || string(response.Result) != want {
			t.Fatalf("%s: response %s %v", request, response.Result, response.Error)
		}
	}
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

// A GrantEnroller runs the enrollment steps for the provider they serve, whose
// option values ProviderOf returns.
func TestServeRunsGrantEnrollmentSteps(t *testing.T) {
	response := serveWith(t, enroller{}, `{"method":"grant_start","params":{"profile":"acme-account"},"provider":{"profile":"acme-account","options":{"account":"acme"}}}`)
	var authorization GrantAuthorization
	if response.Error != nil || json.Unmarshal(response.Result, &authorization) != nil || authorization.URL != "https://login.acme.example/authorize?profile=acme-account&account=acme" || authorization.Session != "verifier" {
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

// A plugin can't declare a profile that authenticates with a grant without a
// GrantEnroller: it reports no manifest, so OLP never installs it.
func TestServeRefusesAGrantProfileWithoutAGrantEnroller(t *testing.T) {
	declared := Manifest{Name: "acme", Version: "1.0.0", Profiles: []Profile{{ID: "acme-account", Label: "Acme Account", Dialect: "openai-chat", Grant: &GrantAuthentication{}}}}
	if response := serveWith(t, manifestOnly(declared), `{"method":"manifest"}`); response.Error == nil || response.Error.Code != abi.CodeInternal {
		t.Fatalf("manifest response %+v", response)
	}
	if response := serveWith(t, enroller{manifestOnly: manifestOnly(declared)}, `{"method":"manifest"}`); response.Error != nil {
		t.Fatalf("manifest response %+v", response)
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
