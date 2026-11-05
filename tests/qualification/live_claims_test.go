//go:build qualification

package qualification_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/plugin/abi"
)

type liveLane struct {
	client       laneClient
	adapter      string
	profile      string
	manifest     func() abi.Manifest
	keyEnv       string
	modelEnv     string
	secondKeyEnv string
	path         string
	protocol     string
	headers      map[string]string
}

func (l liveLane) id() string { return l.adapter + "/" + l.profile }

func liveLanes() []liveLane {
	messages := map[string]string{"Anthropic-Version": "2023-06-01"}
	return []liveLane{
		{client: laneClient{name: "codex", binary: "codex", userAgent: "codex_cli_rs (qualification live)"},
			adapter: "codex", profile: codexauth.ProfileID, manifest: codexauth.Manifest,
			keyEnv: "OLP_LIVE_CODEX_AUTH_JSON", modelEnv: "OLP_LIVE_CODEX_MODEL", secondKeyEnv: "OLP_LIVE_CODEX_AUTH_JSON_2",
			path: "responses", protocol: "responses"},
		{client: laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification live)"},
			adapter: "zai_coding", profile: codeplans.ZAIProfile, manifest: codeplans.ZAIManifest,
			keyEnv: "OLP_LIVE_GLM_GLOBAL_KEY", modelEnv: "OLP_LIVE_GLM_GLOBAL_MODEL",
			path: "v1/messages", protocol: "messages", headers: messages},
		{client: laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification live)"},
			adapter: "zai_coding", profile: codeplans.BigModelProfile, manifest: codeplans.ZAIManifest,
			keyEnv: "OLP_LIVE_GLM_CN_KEY", modelEnv: "OLP_LIVE_GLM_CN_MODEL",
			path: "v1/messages", protocol: "messages", headers: messages},
		{client: laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification live)"},
			adapter: "minimax_plan", profile: codeplans.MiniMaxProfile, manifest: codeplans.MiniMaxManifest,
			keyEnv: "OLP_LIVE_MINIMAX_GLOBAL_KEY", modelEnv: "OLP_LIVE_MINIMAX_GLOBAL_MODEL",
			path: "v1/messages", protocol: "messages", headers: messages},
		{client: laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification live)"},
			adapter: "minimax_plan", profile: codeplans.MiniMaxCNProfile, manifest: codeplans.MiniMaxManifest,
			keyEnv: "OLP_LIVE_MINIMAX_CN_KEY", modelEnv: "OLP_LIVE_MINIMAX_CN_MODEL",
			path: "v1/messages", protocol: "messages", headers: messages},
		{client: laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification live)"},
			adapter: "kimi_code", profile: codeplans.KimiProfile, manifest: codeplans.KimiManifest,
			keyEnv: "OLP_LIVE_KIMI_GLOBAL_KEY", modelEnv: "OLP_LIVE_KIMI_GLOBAL_MODEL",
			path: "v1/messages", protocol: "messages", headers: messages},
		{client: laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification live)"},
			adapter: "kimi_code", profile: codeplans.KimiCNProfile, manifest: codeplans.KimiManifest,
			keyEnv: "OLP_LIVE_KIMI_CN_KEY", modelEnv: "OLP_LIVE_KIMI_CN_MODEL",
			path: "v1/messages", protocol: "messages", headers: messages},
		{client: laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification live)"},
			adapter: "qwen_token_plan", profile: codeplans.QwenTokenPlanProfile, manifest: codeplans.QwenTokenPlanManifest,
			keyEnv: "OLP_LIVE_QWEN_GLOBAL_KEY", modelEnv: "OLP_LIVE_QWEN_GLOBAL_MODEL",
			path: "v1/messages", protocol: "messages", headers: messages},
		{client: laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification live)"},
			adapter: "qwen_token_plan", profile: codeplans.QwenTokenPlanCNProfile, manifest: codeplans.QwenTokenPlanManifest,
			keyEnv: "OLP_LIVE_QWEN_CN_KEY", modelEnv: "OLP_LIVE_QWEN_CN_MODEL",
			path: "v1/messages", protocol: "messages", headers: messages},
		{client: laneClient{name: "opencode", binary: "opencode", userAgent: "opencode (qualification live)"},
			adapter: "opencode_go", profile: codeplans.OpenCodeGoProfile, manifest: codeplans.OpenCodeGoManifest,
			keyEnv: "OLP_LIVE_OPENCODE_GO_KEY", modelEnv: "OLP_LIVE_OPENCODE_GO_MODEL",
			path: "v1/chat/completions", protocol: "chat"},
	}
}

func (l liveLane) provided() (key, model string, missing []string) {
	key, model = os.Getenv(l.keyEnv), strings.TrimSpace(os.Getenv(l.modelEnv))
	if key == "" {
		missing = append(missing, l.keyEnv)
	}
	if model == "" {
		missing = append(missing, l.modelEnv)
	}
	return key, model, missing
}

func (l liveLane) record(result, detail string, checks []runCheck) runRecord {
	version, _ := clientVersion(l.client.binary)
	_, model, _ := l.provided()
	return runRecord{
		Client: l.client.name, ClientVersion: version, ClientExecuted: false,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Adapter:  l.adapter, Profile: l.profile, Model: model, Protocol: l.protocol, Transport: "http",
		Result: result, Evidence: "live", Driver: "protocol-live", Detail: detail, Checks: checks,
	}
}

type claimsSummary struct {
	Type               string   `json:"type"`
	Evidence           string   `json:"evidence"`
	ResetAwareOrdering []string `json:"reset_aware_ordering"`
	LiveInference      []string `json:"live_inference"`
	CooldownEvidence   []string `json:"cooldown_evidence"`
	FailoverEvidence   []string `json:"failover_evidence"`
}

func checkPassed(record runRecord, name string) bool {
	return slices.ContainsFunc(record.Checks, func(check runCheck) bool {
		return check.Name == name && check.Result == "pass"
	})
}

func deriveClaims(records []runRecord) claimsSummary {
	claims := claimsSummary{Type: "claims", Evidence: "live", ResetAwareOrdering: []string{}, LiveInference: []string{}, CooldownEvidence: []string{}, FailoverEvidence: []string{}}
	for _, record := range records {
		id := record.Adapter + "/" + record.Profile
		if record.Result == "pass" {
			claims.LiveInference = append(claims.LiveInference, id)
		}
		if record.Result == "pass" && checkPassed(record, "allowance") && checkPassed(record, "bands") {
			claims.ResetAwareOrdering = append(claims.ResetAwareOrdering, id)
		}
		if record.Result == "pass" && checkPassed(record, "cooldown") {
			claims.CooldownEvidence = append(claims.CooldownEvidence, id)
		}
		if record.Result == "pass" && checkPassed(record, "failover") {
			claims.FailoverEvidence = append(claims.FailoverEvidence, id)
		}
	}
	return claims
}

var errFixtureReport = errors.New("live report path must differ from the fixture report path")

func liveReportPath() (string, error) {
	live := os.Getenv("OLP_QUALIFY_LIVE_REPORT")
	if live != "" && live == os.Getenv("OLP_QUALIFY_REPORT") {
		return "", errFixtureReport
	}
	return live, nil
}

func (w *reportWriter) emitClaims(t *testing.T, claims claimsSummary) {
	t.Helper()
	encoded, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.out.Write(append(encoded, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (l liveLane) seedSpec(secret string, now time.Time) (laneSeed, error) {
	if l.adapter != "codex" {
		if !codeplans.ValidKey(secret) {
			return laneSeed{}, fmt.Errorf("env %s does not hold a plausible vendor key", l.keyEnv)
		}
		return laneSeed{adapter: l.adapter, profile: l.profile, manifest: l.manifest, token: secret, principal: codeplans.Principal(l.profile, secret), facts: map[string]string{}}, nil
	}
	if _, err := codexauth.ParseCredentialImport(secret, now); err != nil {
		return laneSeed{}, fmt.Errorf("env %s does not hold a supported Codex auth.json document", l.keyEnv)
	}
	var document struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(secret), &document); err != nil || document.Tokens.AccessToken == "" {
		return laneSeed{}, fmt.Errorf("env %s does not hold a supported Codex auth.json document", l.keyEnv)
	}
	identity, err := codexauth.TokenIdentity(document.Tokens.AccessToken, now)
	if err != nil {
		return laneSeed{}, fmt.Errorf("env %s access token identity is unusable or expired", l.keyEnv)
	}
	expires := identity.ExpiresAt
	return laneSeed{adapter: l.adapter, profile: l.profile, manifest: l.manifest, token: document.Tokens.AccessToken, principal: identity.Principal(), facts: identity.Facts(), expiresAt: &expires}, nil
}

func TestLiveRecordsNeverContainSecrets(t *testing.T) {
	sentinel := "sentinel-live-secret-value-1234567890"
	lane := liveLanes()[1]
	t.Setenv(lane.keyEnv, sentinel)
	t.Setenv(lane.modelEnv, "some-model")
	record := lane.record("no_signal", "missing env: "+lane.keyEnv, []runCheck{{Name: "inference", Result: "no_signal", Detail: "env " + lane.keyEnv + " unusable"}})
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := json.Marshal(deriveClaims([]runRecord{record}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), sentinel) || strings.Contains(string(claims), sentinel) {
		t.Fatal("live report leaked an env value")
	}
	if _, err := lane.seedSpec(sentinel[:8], time.Now()); err == nil || !strings.Contains(err.Error(), lane.keyEnv) || strings.Contains(err.Error(), sentinel[:8]) {
		t.Fatalf("seed error = %v, want env name only", err)
	}
}

func TestLiveClaimsDerivation(t *testing.T) {
	pass := func(adapter, profile string, checks ...runCheck) runRecord {
		return runRecord{Adapter: adapter, Profile: profile, Result: "pass", Evidence: "live", Checks: checks}
	}
	ok := func(name string) runCheck { return runCheck{Name: name, Result: "pass"} }
	none := deriveClaims([]runRecord{{Adapter: "codex", Profile: "codex-subscription", Result: "no_signal", Evidence: "live"}})
	if len(none.ResetAwareOrdering) != 0 || len(none.LiveInference) != 0 {
		t.Fatalf("missing lanes must claim nothing, got %+v", none)
	}
	notPassing := deriveClaims([]runRecord{
		{Adapter: "codex", Profile: "codex-subscription", Result: "fail", Evidence: "live", Checks: []runCheck{ok("cooldown"), ok("failover")}},
		{Adapter: "zai_coding", Profile: "zai-coding-plan", Result: "no_signal", Evidence: "live", Checks: []runCheck{ok("cooldown"), ok("failover")}},
	})
	if len(notPassing.CooldownEvidence) != 0 || len(notPassing.FailoverEvidence) != 0 {
		t.Fatalf("non-passing lanes must claim no cooldown or failover evidence, got %+v", notPassing)
	}
	inferenceOnly := deriveClaims([]runRecord{pass("codex", "codex-subscription", ok("inference"), ok("allowance"))})
	if len(inferenceOnly.LiveInference) != 1 || len(inferenceOnly.ResetAwareOrdering) != 0 {
		t.Fatalf("codex without bands must not claim ordering, got %+v", inferenceOnly)
	}
	full := deriveClaims([]runRecord{
		pass("codex", "codex-subscription", ok("inference"), ok("allowance"), ok("bands"), ok("cooldown")),
		pass("zai_coding", "zai-coding-plan", ok("inference"), ok("failover")),
	})
	if !slices.Equal(full.ResetAwareOrdering, []string{"codex/codex-subscription"}) {
		t.Fatalf("ordering claims = %+v", full)
	}
	if !slices.Equal(full.LiveInference, []string{"codex/codex-subscription", "zai_coding/zai-coding-plan"}) {
		t.Fatalf("inference claims = %+v", full)
	}
	if !slices.Equal(full.CooldownEvidence, []string{"codex/codex-subscription"}) || !slices.Equal(full.FailoverEvidence, []string{"zai_coding/zai-coding-plan"}) {
		t.Fatalf("evidence claims = %+v", full)
	}
}

func TestLiveReportPathRefusesFixtureReport(t *testing.T) {
	t.Setenv("OLP_QUALIFY_REPORT", "/tmp/fixture-report.jsonl")
	t.Setenv("OLP_QUALIFY_LIVE_REPORT", "/tmp/fixture-report.jsonl")
	if _, err := liveReportPath(); !errors.Is(err, errFixtureReport) {
		t.Fatalf("liveReportPath error = %v, want fixture refusal", err)
	}
	t.Setenv("OLP_QUALIFY_LIVE_REPORT", "/tmp/live-report.jsonl")
	if path, err := liveReportPath(); err != nil || path != "/tmp/live-report.jsonl" {
		t.Fatalf("liveReportPath = %q, %v", path, err)
	}
}
