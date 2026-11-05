//go:build qualification && qualificationlive

package qualification_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
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

func liveBody(protocol, model string) map[string]any {
	switch protocol {
	case "responses":
		return map[string]any{"model": model, "input": "Reply with the word ready.", "stream": false, "store": false}
	case "messages":
		return map[string]any{"model": model, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "Reply with the word ready."}}}
	default:
		return map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "Reply with the word ready."}}}
	}
}

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

func runLiveLane(l liveLane, journey func() []runCheck) runRecord {
	if _, _, missing := l.provided(); len(missing) > 0 {
		return l.record("no_signal", "missing env: "+strings.Join(missing, ", "), nil)
	}
	if _, ok := clientVersion(l.client.binary); !ok {
		return l.record("no_signal", "client binary "+l.client.binary+" unavailable", nil)
	}
	checks := journey()
	return l.record(liveLaneResult(l, checks), "", checks)
}

func liveLaneResult(l liveLane, checks []runCheck) string {
	byName := map[string]string{}
	for _, check := range checks {
		byName[check.Name] = check.Result
	}
	if byName["inference"] != "pass" {
		if byName["inference"] == "no_signal" {
			return "no_signal"
		}
		return "fail"
	}
	if l.adapter == "codex" && byName["allowance"] != "pass" {
		return "fail"
	}
	return "pass"
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
		if checkPassed(record, "cooldown") {
			claims.CooldownEvidence = append(claims.CooldownEvidence, id)
		}
		if checkPassed(record, "failover") {
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

func newLiveReportWriter(t *testing.T) *reportWriter {
	t.Helper()
	path, err := liveReportPath()
	if err != nil {
		t.Fatal(err)
	}
	if path == "" {
		return &reportWriter{out: os.Stdout}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return &reportWriter{out: file}
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

func (s *service) liveList(c *caller, path string) ([]map[string]any, error) {
	status, raw := s.do(c, "GET", path, nil, nil)
	if status != http.StatusOK {
		return nil, fmt.Errorf("GET %s returned status %d", path, status)
	}
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, fmt.Errorf("GET %s returned invalid JSON", path)
	}
	return page.Items, nil
}

func (s *service) liveCreate(c *caller, path string, body any, headers map[string]string, status int) (map[string]any, error) {
	got, raw := s.do(c, "POST", path, body, headers)
	if got != status {
		return nil, fmt.Errorf("POST %s returned status %d, want %d", path, got, status)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, fmt.Errorf("POST %s returned invalid JSON", path)
		}
	}
	return out, nil
}

func pollLive(window, interval time.Duration, ready func() bool) bool {
	deadline := time.Now().Add(window)
	for {
		if ready() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(interval)
	}
}

func allowanceWindows(account map[string]any) []any {
	allowance, ok := account["allowance"].(map[string]any)
	if !ok {
		return nil
	}
	windows, _ := allowance["windows"].([]any)
	return windows
}

func (s *service) liveAccount(owner *caller, project, accountID string) (map[string]any, bool) {
	items, err := s.liveList(owner, "/api/v1/code/accounts?project_id="+project+"&limit=200")
	if err != nil {
		return nil, false
	}
	for _, item := range items {
		if item["id"] == accountID {
			return item, true
		}
	}
	return nil, false
}

func (s *service) seedLiveAccount(owner *caller, project, ownerID, model, name string, spec laneSeed, adapter string) (string, error) {
	providerID, credentialID, err := s.seedSubscriptionE(project, ownerID, spec, nil)
	if err != nil {
		return "", fmt.Errorf("seed subscription: %w", err)
	}
	account, err := s.liveCreate(owner, "/api/v1/code/accounts", map[string]any{
		"project_id": project, "provider_id": providerID, "credential_id": credentialID,
		"name": name, "enabled": true, "models": []string{model},
	}, idempotent(), http.StatusCreated)
	if err != nil {
		return "", err
	}
	if account["adapter"] != adapter {
		return "", fmt.Errorf("account adapter %v, want %s", account["adapter"], adapter)
	}
	id, _ := account["id"].(string)
	if id == "" {
		return "", fmt.Errorf("account response carries no id")
	}
	return id, nil
}

func (s *service) liveJourney(owner, consumer *caller, project, ownerID, keyID string, l liveLane) []runCheck {
	setupFail := func(stage string, err error) []runCheck {
		return []runCheck{{Name: "inference", Result: "no_signal", Detail: stage + ": " + err.Error()}}
	}
	key, model, _ := l.provided()
	spec, err := l.seedSpec(key, time.Now())
	if err != nil {
		return setupFail("credential", err)
	}
	primary, err := s.seedLiveAccount(owner, project, ownerID, model, "qualify-live-"+l.profile, spec, l.adapter)
	if err != nil {
		return setupFail("account", err)
	}
	accountIDs := []string{primary}
	secondDetail := ""
	if l.secondKeyEnv == "" {
		secondDetail = "lane defines no second account env"
	} else if second := os.Getenv(l.secondKeyEnv); second == "" {
		secondDetail = "second account env " + l.secondKeyEnv + " not provided"
	} else if spec2, err := l.seedSpec(second, time.Now()); err != nil {
		secondDetail = "second credential: " + err.Error()
	} else if id2, err := s.seedLiveAccount(owner, project, ownerID, model, "qualify-live-"+l.profile+"-2", spec2, l.adapter); err != nil {
		secondDetail = "second account: " + err.Error()
	} else {
		accountIDs = append(accountIDs, id2)
	}
	poolAccounts := make([]any, len(accountIDs))
	for i, id := range accountIDs {
		poolAccounts[i] = id
	}
	pool, err := s.liveCreate(owner, "/api/v1/code/pools", map[string]any{
		"project_id": project, "name": "qualify-live-" + l.profile, "kind": "shared", "owner_user_id": nil,
		"account_ids": poolAccounts, "account_weights": map[string]any{}, "api_key_ids": []any{keyID},
	}, idempotent(), http.StatusCreated)
	if err != nil {
		return setupFail("pool", err)
	}
	slug := uniqueName("qualify-live-" + l.profile)
	routeBody := map[string]any{
		"project_id": project, "slug": slug, "pool_id": pool["id"], "models": []string{model}, "enabled": true,
		"allowance_ordering": l.adapter == "codex",
	}
	if len(accountIDs) > 1 {
		routeBody["initial_recovery_attempts"] = 1
	}
	route, err := s.liveCreate(owner, "/api/v1/code/routes", routeBody, idempotent(), http.StatusCreated)
	if err != nil {
		return setupFail("route", err)
	}
	if _, err := s.liveCreate(owner, "/api/v1/code/routes/"+route["id"].(string)+"/publish", nil, idempotent(etag(route)), http.StatusOK); err != nil {
		return setupFail("publish", err)
	}
	return s.liveChecks(owner, consumer, project, slug, primary, accountIDs, secondDetail, l)
}

func (s *service) liveInference(consumer *caller, slug string, l liveLane) int {
	_, model, _ := l.provided()
	headers := map[string]string{"User-Agent": l.client.userAgent}
	for name, value := range l.headers {
		headers[name] = value
	}
	status := 0
	deadline := time.Now().Add(60 * time.Second)
	for {
		status, _ = s.do(consumer, "POST", "/code/"+slug+"/"+l.path, liveBody(l.protocol, model), headers)
		if status == http.StatusOK || !slices.Contains([]int{http.StatusNotFound, http.StatusBadGateway, http.StatusServiceUnavailable}, status) || time.Now().After(deadline) {
			return status
		}
		time.Sleep(2 * time.Second)
	}
}

func (s *service) liveAttempts(owner *caller, project string) []map[string]any {
	items, err := s.liveList(owner, "/api/v1/code/attempts?project_id="+project+"&limit=200")
	if err != nil {
		return nil
	}
	return items
}

func (s *service) liveDecisions(owner *caller, project string) []map[string]any {
	items, err := s.liveList(owner, "/api/v1/code/decisions?project_id="+project+"&limit=200")
	if err != nil {
		return nil
	}
	return items
}

func (s *service) liveChecks(owner, consumer *caller, project, slug, primary string, accountIDs []string, secondDetail string, l liveLane) []runCheck {
	status := s.liveInference(consumer, slug, l)
	inference := runCheck{Name: "inference", Result: "fail", Detail: fmt.Sprintf("journey returned status %d", status)}
	if status == http.StatusOK {
		settled := pollLive(15*time.Second, time.Second, func() bool {
			return slices.ContainsFunc(s.liveAttempts(owner, project), func(attempt map[string]any) bool {
				return slices.Contains(accountIDs, fmt.Sprint(attempt["account_id"])) && attempt["upstream_status"] == float64(http.StatusOK)
			})
		})
		if settled {
			inference = runCheck{Name: "inference", Result: "pass"}
		} else {
			inference.Detail = "journey returned status 200 without settled attempt diagnostics"
		}
	}
	checks := []runCheck{inference}
	if inference.Result != "pass" {
		return append(checks,
			runCheck{Name: "allowance", Result: "no_signal", Detail: "inference did not pass"},
			runCheck{Name: "bands", Result: "no_signal", Detail: "inference did not pass"},
			runCheck{Name: "cooldown", Result: "no_signal", Detail: "inference did not pass"},
			runCheck{Name: "failover", Result: "no_signal", Detail: "inference did not pass"})
	}
	return append(checks,
		s.liveAllowanceCheck(owner, project, primary, l),
		s.liveBandsCheck(owner, project, accountIDs, secondDetail, l),
		s.liveCooldownCheck(owner, project, accountIDs),
		s.liveFailoverCheck(owner, project, accountIDs, l))
}

func (s *service) liveAllowanceCheck(owner *caller, project, primary string, l liveLane) runCheck {
	if l.adapter != "codex" {
		return runCheck{Name: "allowance", Result: "no_signal", Detail: "no qualified allowance signal in stage one"}
	}
	observed := func() bool {
		account, ok := s.liveAccount(owner, project, primary)
		return ok && len(allowanceWindows(account)) > 0
	}
	if pollLive(20*time.Second, time.Second, observed) {
		return runCheck{Name: "allowance", Result: "pass", Detail: "allowance windows observed after inference"}
	}
	if _, err := s.liveCreate(owner, "/api/v1/code/accounts/"+primary+"/allowance-reads", nil, idempotent(), http.StatusOK); err != nil {
		return runCheck{Name: "allowance", Result: "fail", Detail: "active allowance read request failed: " + err.Error()}
	}
	if pollLive(90*time.Second, 2*time.Second, observed) {
		return runCheck{Name: "allowance", Result: "pass", Detail: "allowance windows observed after active read"}
	}
	return runCheck{Name: "allowance", Result: "fail", Detail: "no allowance windows observed after inference and active read"}
}

func (s *service) liveBandsCheck(owner *caller, project string, accountIDs []string, secondDetail string, l liveLane) runCheck {
	if l.adapter != "codex" {
		return runCheck{Name: "bands", Result: "no_signal", Detail: "band ordering is scoped to Codex in stage one"}
	}
	if len(accountIDs) < 2 {
		return runCheck{Name: "bands", Result: "no_signal", Detail: secondDetail}
	}
	evidenced := pollLive(15*time.Second, time.Second, func() bool {
		return slices.ContainsFunc(s.liveDecisions(owner, project), func(decision map[string]any) bool {
			return slices.Contains(accountIDs, fmt.Sprint(decision["chosen_account_id"])) && decision["evidence"] != nil
		})
	})
	if evidenced {
		return runCheck{Name: "bands", Result: "pass", Detail: "selection decision carries allowance evidence across two accounts"}
	}
	return runCheck{Name: "bands", Result: "fail", Detail: "no selection decision carries allowance evidence"}
}

func (s *service) liveCooldownCheck(owner *caller, project string, accountIDs []string) runCheck {
	for _, id := range accountIDs {
		if account, ok := s.liveAccount(owner, project, id); ok {
			if cooldowns, _ := account["cooldowns"].([]any); len(cooldowns) > 0 {
				return runCheck{Name: "cooldown", Result: "pass", Detail: "account diagnostics carry a classified cooldown"}
			}
		}
	}
	skipped := slices.ContainsFunc(s.liveDecisions(owner, project), func(decision map[string]any) bool {
		entries, _ := decision["skipped"].([]any)
		return slices.ContainsFunc(entries, func(entry any) bool {
			skip, _ := entry.(map[string]any)
			return skip["class"] == "cooldown" && slices.Contains(accountIDs, fmt.Sprint(skip["account_id"]))
		})
	})
	if skipped {
		return runCheck{Name: "cooldown", Result: "pass", Detail: "selection decision skipped an account for cooldown"}
	}
	return runCheck{Name: "cooldown", Result: "no_signal", Detail: "no cooldown observed during the lane; exhaustion was not induced"}
}

func (s *service) liveFailoverCheck(owner *caller, project string, accountIDs []string, l liveLane) runCheck {
	recovered := slices.ContainsFunc(s.liveAttempts(owner, project), func(attempt map[string]any) bool {
		return attempt["recovery_from"] != nil && slices.Contains(accountIDs, fmt.Sprint(attempt["account_id"]))
	})
	if recovered {
		return runCheck{Name: "failover", Result: "pass", Detail: "attempt diagnostics carry a recovery attempt"}
	}
	detail := "no proven nonexecution rejection occurred during the lane; replay was not induced"
	if l.adapter == "minimax_plan" || l.adapter == "kimi_code" {
		detail = "replay held pending live envelope qualification; " + detail
	}
	return runCheck{Name: "failover", Result: "no_signal", Detail: detail}
}

func TestCodeModeLiveQualificationLanes(t *testing.T) {
	specs := liveLanes()
	writer := newLiveReportWriter(t)
	records := make([]runRecord, 0, len(specs))
	emitted := map[string]bool{}
	defer func() {
		for _, spec := range specs {
			if !emitted[spec.id()] {
				record := spec.record("no_signal", "lane did not complete", nil)
				records = append(records, record)
				writer.emit(t, record)
			}
		}
		writer.emitClaims(t, deriveClaims(records))
	}()
	complete := func(record runRecord) {
		emitted[record.Adapter+"/"+record.Profile] = true
		records = append(records, record)
		writer.emit(t, record)
		if record.Result != "pass" {
			t.Errorf("lane %s/%s: result %s (%s)", record.Adapter, record.Profile, record.Result, record.Detail)
		}
	}

	anyProvided := slices.ContainsFunc(specs, func(spec liveLane) bool {
		_, _, missing := spec.provided()
		return len(missing) == 0
	})
	if !anyProvided {
		for _, spec := range specs {
			complete(runLiveLane(spec, func() []runCheck {
				return []runCheck{{Name: "inference", Result: "no_signal", Detail: "no live lane env provided"}}
			}))
		}
		return
	}

	s := startServiceAdjusted(t, func(env *environment) {
		delete(env.values, "OLP_PROVIDER_EGRESS_ALLOW_CIDRS")
		delete(env.values, "OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS")
	})
	owner := s.owner()
	var ownerID string
	if err := s.pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users LIMIT 1`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	project := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "qualification-live"}, idempotent(), http.StatusCreated)["id"].(string)
	key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "qualification-live", "scopes": []string{"inference"}, "project_id": project}, idempotent(), http.StatusCreated)
	consumer := &caller{name: "qualification live key", token: key["secret"].(string)}
	keyID := key["id"].(string)

	for _, spec := range specs {
		lane := spec
		complete(runLiveLane(lane, func() []runCheck {
			return s.liveJourney(owner, consumer, project, ownerID, keyID, lane)
		}))
	}
}

func TestLiveLaneTableCoversRequiredProfiles(t *testing.T) {
	want := []string{
		"codex/codex-subscription",
		"zai_coding/zai-coding-plan", "zai_coding/bigmodel-coding-plan",
		"minimax_plan/minimax-plan", "minimax_plan/minimax-plan-cn",
		"kimi_code/kimi-code", "kimi_code/kimi-code-cn",
		"qwen_token_plan/qwen-token-plan", "qwen_token_plan/qwen-token-plan-cn",
		"opencode_go/opencode-go",
	}
	specs := liveLanes()
	got := make([]string, len(specs))
	keyEnvs := map[string]bool{}
	for i, spec := range specs {
		got[i] = spec.id()
		if spec.keyEnv == "" || spec.modelEnv == "" || keyEnvs[spec.keyEnv] {
			t.Fatalf("lane %s has missing or duplicate env names", spec.id())
		}
		keyEnvs[spec.keyEnv] = true
		vendor, ok := codeadapter.ForConnection("plugin", "grant", spec.profile)
		if !ok || string(vendor.Adapter) != spec.adapter {
			t.Fatalf("lane %s does not resolve through codeadapter", spec.id())
		}
		if _, ok := vendor.Address(spec.profile); !ok {
			t.Fatalf("lane %s has no real vendor address", spec.id())
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("live lanes = %v, want %v", got, want)
	}
}

func TestLiveMissingEnvRecordsNoSignal(t *testing.T) {
	lane := liveLanes()[1]
	t.Setenv(lane.keyEnv, "")
	t.Setenv(lane.modelEnv, "")
	called := false
	record := runLiveLane(lane, func() []runCheck {
		called = true
		return nil
	})
	if called {
		t.Fatal("journey ran without lane env")
	}
	if record.Result != "no_signal" || record.Evidence != "live" || record.Profile != lane.profile || !strings.Contains(record.Detail, lane.keyEnv) || !strings.Contains(record.Detail, lane.modelEnv) {
		t.Fatalf("missing env must report no_signal naming the env, got %+v", record)
	}
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
