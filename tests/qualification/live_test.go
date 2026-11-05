//go:build qualification && qualificationlive

package qualification_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
)

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
