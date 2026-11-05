//go:build qualification

package qualification_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type runRecord struct {
	Client        string `json:"client"`
	ClientVersion string `json:"client_version"`
	Platform      string `json:"platform"`
	Adapter       string `json:"adapter"`
	Model         string `json:"model"`
	Protocol      string `json:"protocol"`
	Transport     string `json:"transport"`
	Result        string `json:"result"`
	Evidence      string `json:"evidence"`
	Driver        string `json:"driver"`
	Detail        string `json:"detail,omitempty"`
}

var reportState struct {
	sync.Mutex
	file *os.File
	done bool
}

func emitRecord(t *testing.T, record runRecord) {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	reportState.Lock()
	defer reportState.Unlock()
	if !reportState.done {
		reportState.done = true
		if path := os.Getenv("OLP_QUALIFY_REPORT"); path != "" {
			file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			reportState.file = file
		}
	}
	if reportState.file != nil {
		if _, err := reportState.file.Write(append(encoded, '\n')); err != nil {
			t.Fatal(err)
		}
		return
	}
	fmt.Println(string(encoded))
}

func clientVersion(binary string) (string, bool) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").CombinedOutput()
	if err != nil {
		return "", false
	}
	version := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	return version, version != ""
}

type lane struct {
	client, binary, path, protocol, kind, profile, surface, userAgent string
	body                                                              map[string]any
	headers                                                           map[string]string
}

func (l lane) adapter() string {
	if l.profile != "" {
		return l.profile
	}
	return l.kind
}

func (l lane) record(result, detail string) runRecord {
	version, _ := clientVersion(l.binary)
	return runRecord{
		Client: l.client, ClientVersion: version, Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Adapter: l.adapter(), Model: vendorModel, Protocol: l.protocol, Transport: "http",
		Result: result, Evidence: "fixture", Driver: "protocol-fixture", Detail: detail,
	}
}

func runLane(l lane, journey func() error) runRecord {
	if _, ok := clientVersion(l.binary); !ok {
		return l.record("no_signal", "client binary "+l.binary+" unavailable")
	}
	if err := journey(); err != nil {
		return l.record("fail", err.Error())
	}
	return l.record("pass", "")
}

func lanes() []lane {
	return []lane{
		{
			client: "codex", binary: "codex", path: "responses", protocol: "responses",
			kind: "openai_compatible", profile: "compatible-responses", surface: "openai",
			userAgent: "codex_cli_rs (qualification fixture)",
			body:      map[string]any{"model": vendorModel, "input": "hello"},
		},
		{
			client: "claude-code", binary: "claude", path: "v1/messages", protocol: "messages",
			kind: "anthropic", profile: "anthropic-messages", surface: "anthropic",
			userAgent: "claude-cli (qualification fixture)",
			headers:   map[string]string{"Anthropic-Version": "2023-06-01"},
			body:      map[string]any{"model": vendorModel, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "hello"}}},
		},
		{
			client: "opencode", binary: "opencode", path: "v1/chat/completions", protocol: "chat",
			kind: "openai_compatible", profile: "compatible-chat", surface: "openai",
			userAgent: "opencode (qualification fixture)",
			body:      map[string]any{"model": vendorModel, "messages": []any{map[string]any{"role": "user", "content": "hello"}}},
		},
	}
}

func TestMissingClientReportsNoSignal(t *testing.T) {
	called := false
	record := runLane(lane{client: "codex", binary: "olp-qualification-missing-client", protocol: "responses", kind: "openai_compatible"}, func() error {
		called = true
		return nil
	})
	if called {
		t.Fatal("journey ran without a client binary")
	}
	if record.Result != "no_signal" || record.Evidence != "fixture" || record.ClientVersion != "" {
		t.Fatalf("missing client must report no_signal, got %+v", record)
	}
}

func TestCodeModeQualificationLanes(t *testing.T) {
	s := startService(t)
	owner := s.owner()
	project := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "qualification"}, idempotent(), http.StatusCreated)["id"].(string)
	vendor := newVendor(t)
	key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "qualification", "scopes": []string{"inference"}, "project_id": project}, idempotent(), http.StatusCreated)
	pool := s.want(owner, "POST", "/api/v1/code/pools", map[string]any{
		"project_id": project, "name": "qualification", "kind": "shared", "owner_user_id": nil,
		"account_ids": []any{}, "account_weights": map[string]any{}, "api_key_ids": []any{key["id"]},
	}, idempotent(), http.StatusCreated)
	consumer := &caller{name: "qualification key", token: key["secret"].(string)}

	for _, l := range lanes() {
		provider := s.activeProvider(owner, uniqueName("qualify-"+l.client), vendor.URL, project, l.kind, l.profile, l.surface)
		s.priceModel(owner, provider, l.kind)
		slug := uniqueName("qualify-" + l.client)
		route := s.want(owner, "POST", "/api/v1/code/routes", map[string]any{
			"project_id": project, "slug": slug, "pool_id": pool["id"], "models": []string{vendorModel}, "enabled": true,
			"paid_participation": true,
			"paid_targets":       map[string]any{vendorModel: map[string]any{"provider_id": provider["id"], "slot_id": s.providerSlot(owner, provider)}},
		}, idempotent(), http.StatusCreated)
		s.want(owner, "POST", "/api/v1/code/routes/"+route["id"].(string)+"/publish", nil, idempotent(etag(route)), http.StatusOK)

		lane := l
		record := runLane(lane, func() error {
			deadline := time.Now().Add(45 * time.Second)
			last := ""
			for time.Now().Before(deadline) {
				headers := map[string]string{"User-Agent": lane.userAgent}
				for name, value := range lane.headers {
					headers[name] = value
				}
				status, raw := s.do(consumer, "POST", "/code/"+slug+"/"+lane.path, lane.body, headers)
				if status == http.StatusOK && strings.Contains(string(raw), vendorAnswer) {
					return nil
				}
				last = describe(status, raw)
				time.Sleep(250 * time.Millisecond)
			}
			return fmt.Errorf("journey did not return the fixture answer: %s", last)
		})
		emitRecord(t, record)
		if record.Result != "pass" {
			t.Errorf("lane %s: result %s (%s)", lane.client, record.Result, record.Detail)
		}
	}

	var attempts int64
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.code_attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts < int64(len(lanes())) {
		t.Fatalf("expected at least %d code attempts, got %d", len(lanes()), attempts)
	}
}
