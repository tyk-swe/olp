//go:build qualification

package qualification_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/plugin/abi"
)

type runCheck struct {
	Name   string `json:"name"`
	Result string `json:"result"`
	Detail string `json:"detail,omitempty"`
}

type runRecord struct {
	Client         string     `json:"client"`
	ClientVersion  string     `json:"client_version"`
	ClientExecuted bool       `json:"client_executed"`
	Platform       string     `json:"platform"`
	Adapter        string     `json:"adapter"`
	Profile        string     `json:"profile"`
	Model          string     `json:"model"`
	Protocol       string     `json:"protocol"`
	Transport      string     `json:"transport"`
	Result         string     `json:"result"`
	Evidence       string     `json:"evidence"`
	Driver         string     `json:"driver"`
	Detail         string     `json:"detail,omitempty"`
	Checks         []runCheck `json:"checks,omitempty"`
}

type reportWriter struct {
	out io.Writer
}

func newReportWriter(t *testing.T) *reportWriter {
	t.Helper()
	path := os.Getenv("OLP_QUALIFY_REPORT")
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

func (w *reportWriter) emit(t *testing.T, record runRecord) {
	t.Helper()
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.out.Write(append(encoded, '\n')); err != nil {
		t.Fatal(err)
	}
}

type laneReport struct {
	writer  *reportWriter
	order   []string
	pending map[string]runRecord
}

func newLaneReport(writer *reportWriter, lanes []lane) *laneReport {
	report := &laneReport{writer: writer, pending: map[string]runRecord{}}
	for _, l := range lanes {
		report.order = append(report.order, l.client.name)
		report.pending[l.client.name] = l.record("no_signal", "lane did not complete", "", false)
	}
	return report
}

func (r *laneReport) complete(t *testing.T, record runRecord) {
	t.Helper()
	delete(r.pending, record.Client)
	r.writer.emit(t, record)
}

func (r *laneReport) flush(t *testing.T) {
	t.Helper()
	for _, name := range r.order {
		if record, ok := r.pending[name]; ok {
			r.writer.emit(t, record)
		}
	}
}

func clientVersion(t *testing.T, binary string) (string, bool) {
	t.Helper()
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	out, err := sandbox(t, resolved, "/client", "--version")
	if err != nil {
		return "", false
	}
	version := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	return version, version != ""
}

type laneClient struct {
	name, binary, userAgent string
}

type laneSeed struct {
	adapter   string
	profile   string
	manifest  func() abi.Manifest
	token     string
	principal string
	facts     map[string]string
	expiresAt *time.Time
}

type lane struct {
	client   laneClient
	seed     laneSeed
	protocol string
}

func (l lane) record(result, detail, version string, executed bool) runRecord {
	return runRecord{
		Client: l.client.name, ClientVersion: version, ClientExecuted: executed,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Adapter:  l.seed.adapter, Profile: l.seed.profile, Model: vendorModel, Protocol: l.protocol, Transport: "http",
		Result: result, Evidence: "fixture", Driver: "client-fixture", Detail: detail,
	}
}

func runLane(t *testing.T, l lane, bridge string, journey func(string) (string, bool, error)) runRecord {
	t.Helper()
	binaryPath, err := exec.LookPath(l.client.binary)
	if err != nil {
		return l.record("no_signal", "client binary "+l.client.binary+" unavailable", "", false)
	}
	binary, err := filepath.EvalSymlinks(binaryPath)
	if err != nil {
		return l.record("no_signal", "client binary unavailable", "", false)
	}
	versionOutput, _, err := sandboxClient(t, binary, bridge, nil, "--version")
	if err != nil {
		return l.record("no_signal", "client version unavailable: "+err.Error()+"\n"+versionOutput, "", false)
	}
	if strings.TrimSpace(versionOutput) == "" {
		return l.record("no_signal", "client version unavailable: "+versionOutput, "", false)
	}
	version := strings.TrimSpace(strings.SplitN(versionOutput, "\n", 2)[0])
	output, executed, err := journey(binary)
	if err != nil {
		return l.record("no_signal", err.Error()+"\n"+output, version, executed)
	}
	if !executed {
		return l.record("no_signal", "client did not start\n"+output, version, false)
	}
	return l.record("pass", output, version, true)
}

func codexFixtureToken() (string, time.Time) {
	expires := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	payload, _ := json.Marshal(map[string]any{
		"exp": expires.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": codexAccountID,
			"chatgpt_user_id":    codexUserID,
		},
	})
	encode := func(part []byte) string { return base64.RawURLEncoding.EncodeToString(part) }
	return encode([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + encode(payload) + ".fixture", expires
}

func lanes() []lane {
	token, expires := codexFixtureToken()
	identity, err := codexauth.TokenIdentity(token, time.Now())
	if err != nil {
		panic(err)
	}
	return []lane{
		{
			client:   laneClient{name: "codex", binary: "codex", userAgent: "codex_cli_rs (qualification fixture)"},
			seed:     laneSeed{adapter: "codex", profile: codexauth.ProfileID, manifest: codexauth.Manifest, token: token, principal: identity.Principal(), facts: identity.Facts(), expiresAt: &expires},
			protocol: "responses",
		},
		{
			client:   laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification fixture)"},
			seed:     laneSeed{adapter: "zai_coding", profile: codeplans.ZAIProfile, manifest: codeplans.ZAIManifest, token: zaiKey, principal: codeplans.Principal(codeplans.ZAIProfile, zaiKey), facts: map[string]string{}},
			protocol: "messages",
		},
		{
			client:   laneClient{name: "opencode", binary: "opencode", userAgent: "opencode (qualification fixture)"},
			seed:     laneSeed{adapter: "opencode_go", profile: codeplans.OpenCodeGoProfile, manifest: codeplans.OpenCodeGoManifest, token: openCodeGoKey, principal: codeplans.Principal(codeplans.OpenCodeGoProfile, openCodeGoKey), facts: map[string]string{}},
			protocol: "chat",
		},
	}
}

func (s *service) seedSubscription(project, ownerID string, spec laneSeed, proxy vendorProxy) (providerID, credentialID string) {
	s.t.Helper()
	providerID, credentialID, err := s.seedSubscriptionE(project, ownerID, spec, &egress.ConnectionOptions{ProxyURL: proxy.url, TrustRootsPEM: proxy.roots})
	if err != nil {
		s.t.Fatal(err)
	}
	return providerID, credentialID
}

func (s *service) seedSubscriptionE(project, ownerID string, spec laneSeed, network *egress.ConnectionOptions) (providerID, credentialID string, err error) {
	vendor, ok := codeadapter.ForConnection("plugin", "grant", spec.profile)
	if !ok {
		return "", "", fmt.Errorf("no adapter for profile %s", spec.profile)
	}
	if string(vendor.Adapter) != spec.adapter {
		return "", "", fmt.Errorf("profile %s serves adapter %s, lane expects %s", spec.profile, vendor.Adapter, spec.adapter)
	}
	address, ok := vendor.Address(spec.profile)
	if !ok {
		return "", "", fmt.Errorf("adapter %s has no address for profile %s", spec.adapter, spec.profile)
	}
	digestSum := sha256.Sum256([]byte("olp-qualification-plugin\x00" + spec.profile))
	digest := hex.EncodeToString(digestSum[:])
	manifest, err := json.Marshal(spec.manifest())
	if err != nil {
		return "", "", err
	}
	configuration, err := json.Marshal(providers.Configuration{

		Options: providers.Options{Network: network,
			CredentialHeaders: []string{},
			Models:            map[string]json.RawMessage{},
			ParameterDefaults: map[string]json.RawMessage{},
		}, Identity: providerconfig.Identity[*string]{Kind: "plugin", AuthMode: "grant", ProfileID: spec.profile, ProfileRevision: digest, Endpoint: &address},
	})
	if err != nil {
		return "", "", err
	}
	facts, err := json.Marshal(spec.facts)
	if err != nil {
		return "", "", err
	}
	served, err := json.Marshal(connectors.GrantCredential{PluginDigest: digest, ProfileID: spec.profile, AccessToken: spec.token, Facts: spec.facts})
	if err != nil {
		return "", "", err
	}
	ring, err := secrets.ParseRing([]byte(s.env.master))
	if err != nil {
		return "", "", err
	}
	providerID, credentialID = uuid.NewString(), uuid.NewString()
	tx, err := s.pool.Begin(s.t.Context())
	if err != nil {
		return "", "", err
	}
	defer tx.Rollback(s.t.Context())
	var installation string
	if err := tx.QueryRow(s.t.Context(), `SELECT id::text FROM olp.installation WHERE singleton`).Scan(&installation); err != nil {
		return "", "", err
	}
	steps := []func() error{
		func() error {
			_, err := tx.Exec(s.t.Context(), `INSERT INTO olp.plugins(digest,abi_version,manifest,module,size_bytes,etag,installed_by,approved_by,approved_at) VALUES($1,1,$2,'\x00',1,$3,$4,$4,now()) ON CONFLICT(digest) DO NOTHING`, digest, manifest, uuid.NewString(), ownerID)
			return err
		},
		func() error {
			_, err := tx.Exec(s.t.Context(), `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by,project_id) VALUES($1,$2,'plugin','active',$3,$4,$5,$6,$7)`, providerID, uniqueName("qualify-"+spec.adapter), configuration, uuid.NewString(), uuid.NewString(), ownerID, project)
			return err
		},
		func() error {
			_, err := tx.Exec(s.t.Context(), `INSERT INTO olp.provider_credentials(id,provider_id,version,plugin_digest,principal,grant_facts,profile_id) VALUES($1,$2,(SELECT coalesce(max(version),0)+1 FROM olp.provider_credentials WHERE provider_id=$2),$3,$4,$5,$6)`, credentialID, providerID, digest, spec.principal, facts, spec.profile)
			return err
		},
		func() error {
			return ring.Store(s.t.Context(), tx, installation, credentialID, secrets.ProviderCredential, served, nil)
		},
		func() error {
			_, err := tx.Exec(s.t.Context(), `INSERT INTO olp.provider_grants(credential_id,refresh_token_id,expires_at,refresh_at) VALUES($1,NULL,$2,NULL)`, credentialID, spec.expiresAt)
			return err
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return "", "", err
		}
	}
	if err := tx.Commit(s.t.Context()); err != nil {
		return "", "", err
	}
	return providerID, credentialID, nil
}

const sandboxBridge = `
import select, socket, socketserver, subprocess, sys, threading

class Handler(socketserver.BaseRequestHandler):
    def handle(self):
        with socket.socket(socket.AF_UNIX) as peer:
            peer.connect('/bridge.sock')
            while True:
                ready, _, _ = select.select([self.request, peer], [], [])
                for source in ready:
                    data = source.recv(65536)
                    if not data:
                        return
                    (peer if source is self.request else self.request).sendall(data)

server = socketserver.ThreadingTCPServer(('127.0.0.1', 18765), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
process = subprocess.Popen(sys.argv[1:])
sys.stderr.write('\x00client-started\x00')
sys.stderr.flush()
sys.exit(process.wait())
`

func sandboxClient(t *testing.T, binary, bridge string, env []string, args ...string) (string, bool, error) {
	t.Helper()
	text, err := sandbox(t, binary, slices.Concat([]string{"--ro-bind", bridge, "/bridge.sock", "/usr/bin/env"}, env, []string{"/usr/bin/python3", "-c", sandboxBridge, "/client"}, args)...)
	started := strings.Contains(text, "\x00client-started\x00")
	return strings.Replace(text, "\x00client-started\x00", "", 1), started, err
}

func sandbox(t *testing.T, binary string, argv ...string) (string, error) {
	t.Helper()
	passwd := filepath.Join(t.TempDir(), "passwd")
	entry := fmt.Sprintf("fixture:x:%d:%d::/work:/bin/sh\n", os.Getuid(), os.Getgid())
	if err := os.WriteFile(passwd, []byte(entry), 0o600); err != nil {
		return "", err
	}
	options := slices.Concat([]string{
		"--unshare-net", "--unshare-pid", "--die-with-parent", "--new-session", "--clearenv",
		"--ro-bind", "/usr", "/usr", "--ro-bind", "/lib", "/lib", "--ro-bind", "/lib64", "/lib64", "--symlink", "usr/bin", "/bin",
		"--tmpfs", "/tmp", "--dir", "/work", "--dir", "/etc", "--ro-bind", passwd, "/etc/passwd", "--proc", "/proc", "--dev", "/dev",
		"--ro-bind", binary, "/client", "--chdir", "/work", "--setenv", "PATH", "/usr/bin:/bin",
	}, argv)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bwrap", options...)
	command.Env = []string{}
	output, err := command.CombinedOutput()
	if err != nil && ctx.Err() != nil {
		return string(output), ctx.Err()
	}
	return string(output), err
}

func fixtureBridge(t *testing.T, origin string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "olp.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				server, err := net.Dial("tcp", strings.TrimPrefix(origin, "http://"))
				if err != nil {
					return
				}
				defer server.Close()
				go func() { _, _ = io.Copy(server, client) }()
				_, _ = io.Copy(client, server)
			}()
		}
	}()
	return path
}

func runFixtureClient(t *testing.T, client, binary, bridge, slug, key string) (string, bool, error) {
	base := "http://127.0.0.1:18765/code/" + slug
	prompt := "Say hello."
	switch client {
	case "codex":
		return sandboxClient(t, binary, bridge, []string{"OLP_API_KEY=" + key}, "exec", "--skip-git-repo-check", "--ephemeral", "-m", vendorModel,
			"-c", `model_provider="olp"`, "-c", `model_providers.olp.name="OLP"`, "-c", `model_providers.olp.base_url="`+base+`"`,
			"-c", `model_providers.olp.env_key="OLP_API_KEY"`, "-c", `model_providers.olp.wire_api="responses"`,
			"-c", `model_providers.olp.supports_websockets=false`, "-c", `check_for_update_on_startup=false`, prompt)
	case "claude-code":
		return sandboxClient(t, binary, bridge, []string{"ANTHROPIC_BASE_URL=" + base, "ANTHROPIC_AUTH_TOKEN=" + key, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"},
			"--print", "--bare", "--no-session-persistence", "--tools", "", "--model", vendorModel, prompt)
	case "opencode":
		config, err := json.Marshal(map[string]any{
			"model": "olp/" + vendorModel, "small_model": "olp/" + vendorModel, "enabled_providers": []string{"olp"},
			"provider": map[string]any{"olp": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "OLP",
				"options": map[string]string{"baseURL": base + "/v1", "apiKey": "{env:OLP_API_KEY}"},
				"models":  map[string]any{vendorModel: map[string]string{"name": "Fixture"}}}},
		})
		if err != nil {
			return "", false, err
		}
		return sandboxClient(t, binary, bridge, []string{"OLP_API_KEY=" + key, "OPENCODE_CONFIG_CONTENT=" + string(config), "OPENCODE_DISABLE_MODELS_FETCH=true", "OPENCODE_DISABLE_AUTOUPDATE=true"},
			"run", "--pure", "--model", "olp/"+vendorModel, prompt)
	default:
		return "", false, fmt.Errorf("unknown client %s", client)
	}
}

func waitForFixtureRoute(s *service, consumer *caller, slug, protocol string) error {
	path, body, err := fixtureRouteRequest(protocol)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		status, raw := s.do(consumer, "POST", "/code/"+slug+"/"+path, body, nil)
		if status == http.StatusOK && strings.Contains(string(raw), vendorAnswer) {
			return nil
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("published route did not become ready")
}

func fixtureRouteRequest(protocol string) (string, map[string]any, error) {
	switch protocol {
	case "responses":
		return "responses", map[string]any{"model": vendorModel, "input": "Say hello."}, nil
	case "messages":
		return "v1/messages", map[string]any{"model": vendorModel, "messages": []any{map[string]any{"role": "user", "content": "Say hello."}}, "max_tokens": 64}, nil
	case "chat":
		return "v1/chat/completions", map[string]any{"model": vendorModel, "messages": []any{map[string]any{"role": "user", "content": "Say hello."}}}, nil
	default:
		return "", nil, fmt.Errorf("unknown protocol %s", protocol)
	}
}

func TestMissingClientReportsNoSignal(t *testing.T) {
	called := false
	record := runLane(t, lane{
		client:   laneClient{name: "codex", binary: "olp-qualification-missing-client"},
		seed:     laneSeed{adapter: "codex"},
		protocol: "responses",
	}, "", func(string) (string, bool, error) {
		called = true
		return "", true, nil
	})
	if called {
		t.Fatal("journey ran without a client binary")
	}
	if record.Result != "no_signal" || record.Evidence != "fixture" || record.ClientVersion != "" || record.ClientExecuted {
		t.Fatalf("missing client must report no_signal, got %+v", record)
	}
}

func TestClientVersionProbeCannotReadHarnessSecrets(t *testing.T) {
	sentinel := "olp-qualification-sentinel-secret"
	if os.Getenv("OLP_LIVE_SENTINEL_KEY") != sentinel {
		harness := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestClientVersionProbeCannotReadHarnessSecrets$", "-test.count=1")
		harness.Env = slices.Concat(os.Environ(), []string{"OLP_LIVE_SENTINEL_KEY=" + sentinel})
		if output, err := harness.CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, output)
		}
		return
	}
	dir := t.TempDir()
	probe := "#!/bin/sh\nif env | grep -q " + sentinel + " || cat /proc/*/environ 2>/dev/null | grep -aq " + sentinel + "; then echo leaked; else echo 1.2.3; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "olp-qualification-probe"), []byte(probe), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if version, ok := clientVersion(t, "olp-qualification-probe"); !ok || version != "1.2.3" {
		t.Fatalf("client version probe = %q, %v, want 1.2.3 without harness secrets", version, ok)
	}
}

func TestCodeModeQualificationLanes(t *testing.T) {
	specs := lanes()
	report := newLaneReport(newReportWriter(t), specs)
	defer report.flush(t)

	s := startService(t)
	owner := s.owner()
	var ownerID string
	if err := s.pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users LIMIT 1`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	project := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "qualification"}, idempotent(), http.StatusCreated)["id"].(string)
	proxy := newVendorProxy(t, newVendorMux(specs[0].seed.token))
	key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "qualification", "scopes": []string{"inference"}, "project_id": project}, idempotent(), http.StatusCreated)
	consumer := &caller{name: "qualification key", token: key["secret"].(string)}
	bridge := fixtureBridge(t, s.origin)

	for _, spec := range specs {
		providerID, credentialID := s.seedSubscription(project, ownerID, spec.seed, proxy)
		account := s.want(owner, "POST", "/api/v1/code/accounts", map[string]any{
			"project_id": project, "provider_id": providerID, "credential_id": credentialID,
			"name": "qualify-" + spec.client.name, "enabled": true, "models": []string{vendorModel},
		}, idempotent(), http.StatusCreated)
		if account["adapter"] != spec.seed.adapter {
			t.Errorf("lane %s: account adapter %v, want %s", spec.client.name, account["adapter"], spec.seed.adapter)
		}
		pool := s.want(owner, "POST", "/api/v1/code/pools", map[string]any{
			"project_id": project, "name": "qualify-" + spec.client.name, "kind": "shared", "owner_user_id": nil,
			"account_ids": []any{account["id"]}, "account_weights": map[string]any{}, "api_key_ids": []any{key["id"]},
		}, idempotent(), http.StatusCreated)
		slug := uniqueName("qualify-" + spec.client.name)
		route := s.want(owner, "POST", "/api/v1/code/routes", map[string]any{
			"project_id": project, "slug": slug, "pool_id": pool["id"], "models": []string{vendorModel}, "enabled": true,
		}, idempotent(), http.StatusCreated)
		s.want(owner, "POST", "/api/v1/code/routes/"+route["id"].(string)+"/publish", nil, idempotent(etag(route)), http.StatusOK)

		lane := spec
		record := runLane(t, lane, bridge, func(binary string) (string, bool, error) {
			if err := waitForFixtureRoute(s, consumer, slug, lane.protocol); err != nil {
				return "", false, err
			}
			rawOutput, executed, err := runFixtureClient(t, lane.client.name, binary, bridge, slug, key["secret"].(string))
			output := strings.ReplaceAll(rawOutput, key["secret"].(string), "[redacted]")
			if err != nil {
				return output, executed, err
			}
			if !strings.Contains(output, vendorAnswer) {
				return output, executed, fmt.Errorf("journey did not return the fixture answer")
			}
			return output, executed, nil
		})
		report.complete(t, record)
		if record.Result != "pass" {
			t.Errorf("lane %s: result %s (%s)", lane.client.name, record.Result, record.Detail)
		}
		if record.Result == "pass" && !record.ClientExecuted {
			t.Errorf("lane %s: passing fixture must execute the client", lane.client.name)
		}
	}
}
