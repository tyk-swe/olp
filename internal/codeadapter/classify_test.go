package codeadapter_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/grokauth"
	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/plugin/abi"
)

func codexVendor(t *testing.T) (codeadapter.Vendor, codeadapter.Endpoint) {
	t.Helper()
	vendor, ok := codeadapter.Lookup(codemode.AdapterCodex)
	if !ok {
		t.Fatal("codex vendor missing")
	}
	endpoint, ok := vendor.Endpoint("responses")
	if !ok {
		t.Fatal("codex responses endpoint missing")
	}
	return vendor, endpoint
}

func TestClassifyCodexUsageLimitIsCredentialQuotaWithVendorReset(t *testing.T) {
	vendor, endpoint := codexVendor(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	reset := now.Add(2 * time.Hour)
	body := []byte(`{"error":{"type":"usage_limit_reached","resets_at":` + itoa(reset.Unix()) + `,"resets_in_seconds":60}}`)
	got, ok := vendor.Classify(endpoint, 429, body, http.Header{"Retry-After": {"30"}}, now)
	if !ok || got.Class != codeadapter.CooldownQuota || got.Scope != codeadapter.ScopeCredential || !got.RetryAt.Equal(reset) || got.Envelope != "codex_usage_limit_reached" || !got.NotExecuted {
		t.Fatalf("Classify = %+v, %v", got, ok)
	}
}

func TestClassifyCodexUsageLimitFallsBackToResetsInSecondsThenRetryAfterThenMinute(t *testing.T) {
	vendor, endpoint := codexVendor(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	cases := []struct {
		name    string
		body    string
		headers http.Header
		want    time.Time
	}{
		{"resets_in_seconds beats shorter retry-after", `{"error":{"type":"usage_limit_reached","resets_in_seconds":600}}`, http.Header{"Retry-After": {"30"}}, now.Add(600 * time.Second)},
		{"single retry-after beats minute", `{"error":{"type":"usage_limit_reached"}}`, http.Header{"Retry-After": {"300"}}, now.Add(300 * time.Second)},
		{"short retry-after beats minute", `{"error":{"type":"usage_limit_reached"}}`, http.Header{"Retry-After": {"7"}}, now.Add(7 * time.Second)},
		{"zero retry-after permits immediate retry", `{"error":{"type":"usage_limit_reached"}}`, http.Header{"Retry-After": {"0"}}, now},
		{"short vendor reset beats minute", `{"error":{"type":"usage_limit_reached","resets_in_seconds":12}}`, http.Header{}, now.Add(12 * time.Second)},
		{"multiple retry-after values ignored", `{"error":{"type":"usage_limit_reached"}}`, http.Header{"Retry-After": {"300", "600"}}, now.Add(time.Minute)},
		{"minute fallback", `{"error":{"type":"usage_limit_reached"}}`, http.Header{}, now.Add(time.Minute)},
		{"past reset ignored", `{"error":{"type":"usage_limit_reached","resets_at":` + itoa(now.Add(-time.Hour).Unix()) + `}}`, http.Header{}, now.Add(time.Minute)},
		{"resets_in_seconds beyond 32 bits ignored", `{"error":{"type":"usage_limit_reached","resets_in_seconds":4294967296}}`, http.Header{}, now.Add(time.Minute)},
		{"resets_at at year 10000 kept", `{"error":{"type":"usage_limit_reached","resets_at":253402300800}}`, http.Header{}, time.Unix(253402300800, 0).UTC()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := vendor.Classify(endpoint, 429, []byte(tc.body), tc.headers, now)
			if !ok || !got.RetryAt.Equal(tc.want) {
				t.Fatalf("Classify = %+v, %v want retry at %s", got, ok, tc.want)
			}
		})
	}
}

func TestClassifyCodexCapacityIsShortModelCooldownOnlyForExactCodes(t *testing.T) {
	vendor, endpoint := codexVendor(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	for _, body := range []string{
		`{"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded"}}`,
		`{"error":{"type":"turn_rate_limit_exceeded"}}`,
		`{"error":{"type":"rate_limit_error","code":"model_at_capacity"}}`,
		`{"error":{"type":"rate_limit_error","code":"model_is_at_capacity"}}`,
	} {
		got, ok := vendor.Classify(endpoint, 429, []byte(body), http.Header{}, now)
		if !ok || got.Class != codeadapter.CooldownCapacity || got.Scope != codeadapter.ScopeModel || !got.RetryAt.Equal(now.Add(time.Minute)) || got.NotExecuted {
			t.Fatalf("Classify(%s) = %+v, %v", body, got, ok)
		}
	}
	got, ok := vendor.Classify(endpoint, 429, []byte(`{"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded"}}`), http.Header{"Retry-After": {"120"}}, now)
	if !ok || !got.RetryAt.Equal(now.Add(120*time.Second)) {
		t.Fatalf("capacity retry-after = %+v, %v", got, ok)
	}
	for _, body := range []string{
		`{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"The model is at capacity"}}`,
		`{"error":{"type":"rate_limit_error","message":"model is at capacity, try again"}}`,
		`{"error":{"type":"usage_limit_reached"}}`,
	} {
		if got, ok := vendor.Classify(endpoint, 500, []byte(body), http.Header{}, now); ok {
			t.Fatalf("Classify(%s, 500) = %+v, want unclassified", body, got)
		}
	}
	if got, ok := vendor.Classify(endpoint, 429, []byte(`{"error":{"type":"rate_limit_error","message":"model is at capacity, try again"}}`), http.Header{}, now); ok {
		t.Fatalf("prose capacity = %+v, want unclassified", got)
	}
}

func TestClassifyStreamWrappedUsageLimitCoolsButIsNotProvenNotExecuted(t *testing.T) {
	vendor, endpoint := codexVendor(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	body := []byte(`{"type":"error","error":{"type":"usage_limit_reached","resets_in_seconds":900}}`)
	got, ok := vendor.Classify(endpoint, 429, body, http.Header{}, now)
	if !ok || got.Class != codeadapter.CooldownQuota || got.NotExecuted || !got.RetryAt.Equal(now.Add(900*time.Second)) {
		t.Fatalf("Classify = %+v, %v", got, ok)
	}
	if rejection, ok := vendor.InitialRejection(endpoint, 429, body, http.Header{}, now); ok {
		t.Fatalf("InitialRejection(stream body) = %+v, want unproven", rejection)
	}
}

func TestInitialRejectionKeepsCodexEnvelopesAndClasses(t *testing.T) {
	vendor, endpoint := codexVendor(t)
	now := time.Unix(1_800_000_000, 0).UTC()
	quota, ok := vendor.InitialRejection(endpoint, 429, []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":600}}`), http.Header{}, now)
	if !ok || quota.Class != upstream.RateLimit || quota.Envelope != "codex_usage_limit_reached" || !quota.RetryAt.Equal(now.Add(600*time.Second)) {
		t.Fatalf("quota rejection = %+v, %v", quota, ok)
	}
	credential, ok := vendor.InitialRejection(endpoint, 401, []byte(`{"error":{"type":"authentication_error","code":"invalid_api_key"}}`), http.Header{}, now)
	if !ok || credential.Class != upstream.Credential || credential.Envelope != "codex_invalid_api_key" || !credential.RetryAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("credential rejection = %+v, %v", credential, ok)
	}
	if rejection, ok := vendor.InitialRejection(endpoint, 429, []byte(`{"error":{"type":"rate_limit_error","code":"turn_rate_limit_exceeded"}}`), http.Header{}, now); ok {
		t.Fatalf("capacity rejection = %+v, want no replay proof", rejection)
	}
	other, found := codeadapter.Lookup(codemode.AdapterOpenCodeGo)
	if !found {
		t.Fatal("opencode vendor missing")
	}
	if got, ok := other.Classify(codeadapter.Endpoint{}, 429, []byte(`{"error":{"type":"usage_limit_reached"}}`), http.Header{}, now); ok {
		t.Fatalf("non-codex Classify = %+v, want unclassified this ticket", got)
	}
	if got, ok := vendor.Classify(endpoint, 429, []byte(`{"error":{"type":"usage_limit_reached"},"extra":1}`), http.Header{}, now); !ok || got.NotExecuted {
		t.Fatalf("extra-member Classify = %+v, %v want classified without proof", got, ok)
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestUnqualifiedQuotaEnvelopesDoNotAuthorizeReplay(t *testing.T) {
	for _, tc := range []struct {
		adapter codemode.Adapter
		body    string
	}{
		{codemode.AdapterZAICoding, `{"error":{"code":"1308","message":"usage limit reached"}}`},
		{codemode.AdapterKimiCode, `{"error":{"message":"quota exceeded","type":"rate_limit_error","code":"exceeded_current_quota_error"}}`},
		{codemode.AdapterQwenTokenPlan, `{"error":{"message":"Your token-plan quota has been exhausted.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`},
	} {
		vendor, ok := codeadapter.Lookup(tc.adapter)
		if !ok {
			t.Fatalf("adapter %s unavailable", tc.adapter)
		}
		endpoint, ok := vendor.Endpoint("v1/chat/completions")
		if !ok {
			t.Fatalf("adapter %s chat endpoint unavailable", tc.adapter)
		}
		now := time.Unix(1_800_000_000, 0).UTC()
		classification, ok := vendor.Classify(endpoint, 429, []byte(tc.body), http.Header{}, now)
		if !ok || classification.Class != codeadapter.CooldownQuota || classification.NotExecuted {
			t.Errorf("%s classification = %+v, %v", tc.adapter, classification, ok)
		}
		if rejection, ok := vendor.InitialRejection(endpoint, 429, []byte(tc.body), http.Header{}, now); ok {
			t.Errorf("%s unexpectedly authorizes replay: %+v", tc.adapter, rejection)
		}
	}
}

func TestGrokDeviceGrantUsesAuthenticatedSubject(t *testing.T) {
	adapter := grokauth.Adapter{Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
		switch request.URL {
		case grokauth.Issuer + "/oauth2/device/code":
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"device_code":"device-secret","user_code":"ABCD","verification_uri":"https://auth.x.ai/device","expires_in":600,"interval":5}`)}, nil
		case grokauth.Issuer + "/oauth2/token":
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"access_token":"opaque-access","refresh_token":"refresh-secret","token_type":"Bearer","expires_in":3600}`)}, nil
		case grokauth.Issuer + "/oauth2/userinfo":
			if request.Method != http.MethodGet || http.Header(request.Header).Get("Authorization") != "Bearer opaque-access" {
				t.Fatal("identity was not authenticated with the new access token")
			}
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"sub":"verified-subject","email":"display-only@example.invalid"}`)}, nil
		default:
			t.Fatalf("unexpected request %s", request.URL)
			return abi.HTTPResponse{}, nil
		}
	}}
	started, err := adapter.StartGrant(t.Context(), abi.GrantStart{Profile: grokauth.ProfileID})
	if err != nil || started.Device == nil || started.Device.UserCode != "ABCD" {
		t.Fatalf("start = %+v, %v", started, err)
	}
	grant, err := adapter.PollGrant(t.Context(), abi.GrantPoll{Profile: grokauth.ProfileID, Session: started.Session})
	if err != nil || grant.Principal != grokauth.Principal("verified-subject") || grant.Facts["subject"] != "verified-subject" || grant.AccessToken != "opaque-access" || grant.RefreshToken != "refresh-secret" || grant.ExpiresIn != 3600 {
		t.Fatalf("grant = %+v, %v", grant, err)
	}
	encoded, _ := json.Marshal(grant.Facts)
	if string(encoded) != `{"subject":"verified-subject"}` {
		t.Fatalf("unexpected authority facts %s", encoded)
	}
}

func TestGrokRefreshRejectsAmbiguousSubjectWithoutIssuingGrant(t *testing.T) {
	adapter := grokauth.Adapter{Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
		if request.URL == grokauth.Issuer+"/oauth2/token" {
			return abi.HTTPResponse{Status: 200, Body: []byte(`{"access_token":"opaque-new","token_type":"Bearer","expires_in":3600}`)}, nil
		}
		return abi.HTTPResponse{Status: 200, Body: []byte(`{"sub":"foreign-subject","sub":"verified-subject"}`)}, nil
	}}
	grant, err := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: grokauth.ProfileID, RefreshToken: "encrypted-refresh", Facts: map[string]string{"subject": "verified-subject"}})
	if err == nil || grant.AccessToken != "" {
		t.Fatalf("ambiguous subject issued grant %+v, %v", grant, err)
	}
}

func TestGrokSubscriptionNeverUsesPaidAPIOrWebSocket(t *testing.T) {
	vendor, ok := codeadapter.ForConnection("plugin", "grant", grokauth.ProfileID)
	if !ok {
		t.Fatal("Grok subscription profile is unavailable")
	}
	address, ok := vendor.Address(grokauth.ProfileID)
	if !ok || address != grokauth.Upstream {
		t.Fatalf("subscription address %q, %v", address, ok)
	}
	endpoint, ok := vendor.Endpoint("responses")
	if !ok || !endpoint.Supports(codeadapter.HTTP) || endpoint.Supports(codeadapter.WebSocket) || endpoint.Observation != codeadapter.ObserveUsage {
		t.Fatalf("subscription endpoint %+v, %v", endpoint, ok)
	}
	secret := []byte(`{"access_token":"opaque-access","facts":{"subject":"verified-subject"}}`)
	dispatch := codemode.Dispatch{Adapter: vendor.Adapter, Protocol: codemode.ProtocolResponses, ClientPath: "responses"}
	authorization, err := vendor.Authorize(secret, grokauth.ProfileID, grokauth.Principal("verified-subject"), dispatch, time.Now())
	if err != nil || authorization.Headers.Get("Authorization") != "Bearer opaque-access" {
		t.Fatalf("authorization %+v, %v", authorization, err)
	}
	if _, err := vendor.Authorize(secret, grokauth.ProfileID, grokauth.Principal("other-subject"), dispatch, time.Now()); err == nil {
		t.Fatal("credential crossed principal boundary")
	}
}

func TestGrokRefreshReverifiesSubjectAndRequiresProvenExpiry(t *testing.T) {
	for _, test := range []struct {
		name, token, identity string
		accepted              bool
		refresh               string
	}{
		{name: "retains existing refresh", token: `{"access_token":"new-access","token_type":"Bearer","expires_in":3600}`, identity: `{"sub":"verified-subject"}`, accepted: true},
		{name: "rotates refresh", token: `{"access_token":"new-access","refresh_token":"rotated-refresh","token_type":"Bearer","expires_in":3600}`, identity: `{"sub":"verified-subject"}`, accepted: true, refresh: "rotated-refresh"},
		{name: "rejects changed subject", token: `{"access_token":"new-access","token_type":"Bearer","expires_in":3600}`, identity: `{"sub":"other-subject"}`},
		{name: "rejects absent subject", token: `{"access_token":"new-access","token_type":"Bearer","expires_in":3600}`, identity: `{"email":"display-only@example.invalid"}`},
		{name: "rejects unknown expiry", token: `{"access_token":"new-access","token_type":"Bearer"}`, identity: `{"sub":"verified-subject"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := grokauth.Adapter{Fetch: func(_ context.Context, request abi.HTTPRequest) (abi.HTTPResponse, error) {
				if request.URL == grokauth.Issuer+"/oauth2/token" {
					return abi.HTTPResponse{Status: 200, Body: []byte(test.token)}, nil
				}
				if request.URL != grokauth.Issuer+"/oauth2/userinfo" || http.Header(request.Header).Get("Authorization") != "Bearer new-access" {
					t.Fatalf("unexpected identity verification %+v", request)
				}
				return abi.HTTPResponse{Status: 200, Body: []byte(test.identity)}, nil
			}}
			grant, err := adapter.RefreshGrant(t.Context(), abi.GrantRefresh{Profile: grokauth.ProfileID, RefreshToken: "current-refresh", Facts: map[string]string{"subject": "verified-subject"}})
			if !test.accepted {
				if err == nil || grant.AccessToken != "" {
					t.Fatalf("unsupported refresh issued %+v, %v", grant, err)
				}
				return
			}
			if err != nil || grant.Principal != grokauth.Principal("verified-subject") || grant.Facts["subject"] != "verified-subject" || grant.AccessToken != "new-access" || grant.RefreshToken != test.refresh {
				t.Fatalf("refresh %+v, %v", grant, err)
			}
		})
	}
}

func TestCodeGrokClientConfiguration(t *testing.T) {
	route := codemode.Route{Enabled: true, RevisionID: "revision", Slug: "coding", Models: []string{"grok-fixture-model"}}
	grok := codeclient.Offers{{Adapter: codemode.AdapterGrokBuild, Models: route.Models}}
	request := codeclient.Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientCodex, Model: route.Models[0]}
	if _, err := codeclient.Generate(route, grok, request); err == nil {
		t.Fatal("disabled Grok translation offered client configuration")
	}
	route.ResponsesToGrok = true
	configuration, err := codeclient.Generate(route, grok, request)
	if err != nil || !strings.Contains(configuration.Configuration, "supports_websockets = false") {
		t.Fatalf("Grok configuration must select HTTP: %+v, %v", configuration, err)
	}
	mixed := slices.Concat(grok, codeclient.Offers{{Adapter: codemode.AdapterCodex, Models: route.Models}})
	configuration, err = codeclient.Generate(route, mixed, request)
	if err != nil || !strings.Contains(configuration.Configuration, "supports_websockets = false") {
		t.Fatalf("mixed eligible paths must select HTTP: %+v, %v", configuration, err)
	}
	route.ResponsesToGrok = false
	configuration, err = codeclient.Generate(route, mixed, request)
	if err != nil || !strings.Contains(configuration.Configuration, "supports_websockets = true") {
		t.Fatalf("disabled Grok path must preserve native transport: %+v, %v", configuration, err)
	}
	for _, platform := range []string{"linux", "windows"} {
		configuration, err := codeclient.Generate(codemode.Route{Enabled: true, RevisionID: "fixture", Slug: "output-contract", Models: []string{"grok-fixture-model"}, MessagesToGrok: true}, codeclient.Offers{{Adapter: codemode.AdapterGrokBuild, Models: []string{"grok-fixture-model"}}}, codeclient.Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientClaudeCode, Model: "grok-fixture-model", Platform: platform})
		if err != nil || !strings.Contains(configuration.Configuration, "DISABLE_PROMPT_CACHING") || !strings.Contains(configuration.Configuration, "MAX_THINKING_TOKENS") || !strings.Contains(configuration.Configuration, "OLP_API_KEY") {
			t.Fatalf("Grok Claude setup %s = %+v, %v", platform, configuration, err)
		}
	}
}

func TestGrokMessagesPublishedPolicy(t *testing.T) {
	route := codemode.Route{}
	adapter, _ := codeadapter.Lookup(codemode.AdapterGrokBuild)
	if _, ok := codeadapter.RouteEndpoint(route, adapter, "v1/messages"); ok {
		t.Fatal("Messages enabled without publication policy")
	}
	route.MessagesToGrok = true
	if endpoint, ok := codeadapter.RouteEndpoint(route, adapter, "v1/messages"); !ok || endpoint.Protocol != codemode.ProtocolResponses {
		t.Fatal("Messages policy did not offer Responses upstream")
	}
	if protocol, ok := codeadapter.RouteClientProtocol(route, adapter, codeadapter.ClientClaudeCode, "grok-fixture-model"); !ok || protocol != codemode.ProtocolMessages {
		t.Fatal("Claude configuration unavailable")
	}
}

func TestAntigravityClaudeConfigurationOmitsEffort(t *testing.T) {
	for _, platform := range []string{"linux", "windows"} {
		for _, test := range []struct {
			name, model, plan, small string
			omit                     bool
		}{
			{name: "selected", model: "gemini-fixture", omit: true},
			{name: "planning", model: "native-model", plan: "gemini-fixture", omit: true},
			{name: "background", model: "native-model", small: "gemini-fixture", omit: true},
			{name: "unrelated", model: "native-model"},
		} {
			t.Run(platform+"/"+test.name, func(t *testing.T) {
				configuration, err := codeclient.Generate(codemode.Route{Enabled: true, RevisionID: "revision", Slug: "coding", Models: []string{"native-model", "gemini-fixture"}, MessagesToAntigravity: true}, codeclient.Offers{{Adapter: codemode.AdapterZAICoding, Models: []string{"native-model", "gemini-fixture"}}, {Adapter: codemode.AdapterAntigravity, Models: []string{"gemini-fixture"}}}, codeclient.Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientClaudeCode, Model: test.model, PlanModel: test.plan, SmallModel: test.small, Platform: platform})
				if err != nil || strings.Contains(configuration.Configuration, "CLAUDE_CODE_EFFORT_LEVEL") != test.omit || test.omit && !strings.Contains(configuration.Configuration, "'unset'") {
					t.Fatalf("Claude setup = %+v, %v", configuration, err)
				}
			})
		}
	}
}

func TestCodeGrokOpenCodeConfiguration(t *testing.T) {
	disabled := codemode.Route{Enabled: true, RevisionID: "revision", Slug: "coding", Models: []string{"grok-fixture-model"}}
	offers := codeclient.Offers{{Adapter: codemode.AdapterGrokBuild, Models: disabled.Models}}
	request := codeclient.Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientOpenCode, Model: disabled.Models[0]}
	if _, err := codeclient.Generate(disabled, offers, request); err == nil {
		t.Fatal("disabled Chat translation offered configuration")
	}
	route := codemode.Route{Enabled: true, RevisionID: "revision", Slug: "coding", Models: disabled.Models, ChatToGrok: true}
	configuration, err := codeclient.Generate(route, offers, request)
	if err != nil || !strings.Contains(configuration.Configuration, "@ai-sdk/openai-compatible") || !strings.Contains(configuration.Configuration, "/code/coding/v1") {
		t.Fatalf("Chat configuration %+v, %v", configuration, err)
	}
	codex := codeclient.Request{GatewayURL: request.GatewayURL, Client: codeadapter.ClientCodex, Model: request.Model}
	if _, err := codeclient.Generate(route, offers, codex); err == nil {
		t.Fatal("Chat opt-in enabled Responses")
	}
}

func TestSummaryCompactionClientSetupDisclosesPublishedOptIn(t *testing.T) {
	route := codemode.Route{Enabled: true, RevisionID: "revision", Slug: "coding", Models: []string{"grok-fixture-model"}, ResponsesToGrok: true, SummaryCompaction: true}
	configuration, err := codeclient.Generate(route, codeclient.Offers{{Adapter: codemode.AdapterGrokBuild, Models: route.Models}}, codeclient.Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientCodex, Model: route.Models[0]})
	if err != nil {
		t.Fatal(err)
	}
	for _, disclosure := range []string{"additional provider call", "consumes usage", "may lose detail", "client-held items", "until root retirement"} {
		if !strings.Contains(configuration.Instructions, disclosure) {
			t.Fatalf("client setup omitted %q", disclosure)
		}
	}
}

func TestSummaryCompactionClientCapabilityFollowsSelectedPublishedPath(t *testing.T) {
	for _, test := range []struct {
		name    string
		enabled bool
		model   string
		remote  bool
	}{{"default off", false, "gemini-fixture", false}, {"translated opt in", true, "gemini-fixture", true}, {"unrelated native path", true, "native-model", false}} {
		t.Run(test.name, func(t *testing.T) {
			route := codemode.Route{Enabled: true, RevisionID: "revision", Slug: "coding", Models: []string{"gemini-fixture", "native-model"}, ResponsesToAntigravity: true, SummaryCompaction: test.enabled}
			offers := codeclient.Offers{{Adapter: codemode.AdapterAntigravity, Models: []string{"gemini-fixture"}}, {Adapter: codemode.AdapterCodex, Models: []string{"native-model"}}}
			configuration, err := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientCodex, Model: test.model})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(configuration.Configuration, `remote_compaction = "v2"`) != test.remote {
				t.Fatalf("remote compaction capability does not match selected published path\n%s", configuration.Configuration)
			}
		})
	}
}
