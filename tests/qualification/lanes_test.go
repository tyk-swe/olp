//go:build qualification

package qualification_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/antigravityauth"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/grokauth"
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
		record := l.record("no_signal", "lane did not complete", "", false)
		identity := pendingLaneScope(record)
		report.order = append(report.order, identity)
		report.pending[identity] = record
	}
	return report
}

func pendingLaneScope(record runRecord) string {
	scope, _ := json.Marshal([]string{record.Client, record.Platform, record.Adapter, record.Profile, record.Model, record.Protocol, record.Transport})
	return string(scope)
}

func (r *laneReport) complete(t *testing.T, record runRecord) {
	t.Helper()
	delete(r.pending, pendingLaneScope(record))
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
	signedContinuation bool
	client             laneClient
	seed               laneSeed
	protocol           string
	toolRoundTrip      bool
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
	codexSeed := laneSeed{adapter: "codex", profile: codexauth.ProfileID, manifest: codexauth.Manifest, token: token, principal: identity.Principal(), facts: identity.Facts(), expiresAt: &expires}
	return []lane{
		{client: laneClient{name: "codex", binary: "codex", userAgent: "codex_cli_rs (qualification fixture)"}, seed: laneSeed{adapter: "grok-build", profile: grokauth.ProfileID, manifest: grokauth.Manifest, token: grokToken, principal: grokauth.Principal("fixture-subject"), facts: map[string]string{"subject": "fixture-subject"}, expiresAt: &expires}, protocol: "responses", toolRoundTrip: true},
		{client: laneClient{name: "opencode", binary: "opencode", userAgent: "opencode (qualification fixture)"}, seed: laneSeed{adapter: "grok-build", profile: grokauth.ProfileID, manifest: grokauth.Manifest, token: grokToken, principal: grokauth.Principal("fixture-subject"), facts: map[string]string{"subject": "fixture-subject"}, expiresAt: &expires}, protocol: "chat", toolRoundTrip: true},
		{client: laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification fixture)"}, seed: laneSeed{adapter: "grok-build", profile: grokauth.ProfileID, manifest: grokauth.Manifest, token: grokToken, principal: grokauth.Principal("fixture-subject"), facts: map[string]string{"subject": "fixture-subject"}, expiresAt: &expires}, protocol: "messages", toolRoundTrip: true},
		{
			client:   laneClient{name: "codex", binary: "codex", userAgent: "codex_cli_rs (qualification fixture)"},
			seed:     codexSeed,
			protocol: "responses",
		},
		{
			client:   laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification fixture)"},
			seed:     laneSeed{adapter: "zai_coding", profile: codeplans.ZAIProfile, manifest: codeplans.ZAIManifest, token: zaiKey, principal: codeplans.Principal(codeplans.ZAIProfile, zaiKey), facts: map[string]string{}},
			protocol: "messages",
		},
		{client: laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification fixture)"}, seed: codexSeed, protocol: "messages", toolRoundTrip: true},
		{
			client:   laneClient{name: "opencode", binary: "opencode", userAgent: "opencode (qualification fixture)"},
			seed:     laneSeed{adapter: "opencode_go", profile: codeplans.OpenCodeGoProfile, manifest: codeplans.OpenCodeGoManifest, token: openCodeGoKey, principal: codeplans.Principal(codeplans.OpenCodeGoProfile, openCodeGoKey), facts: map[string]string{}},
			protocol: "chat",
		},
		{client: laneClient{name: "codex", binary: "codex", userAgent: "codex_cli_rs (qualification fixture)"}, seed: laneSeed{adapter: "antigravity", profile: antigravityauth.ProfileID, manifest: antigravityauth.Manifest, token: "antigravity-fixture-token", principal: antigravityauth.Principal("fixture-subject", "fixture-project"), facts: map[string]string{"subject": "fixture-subject", "project_id": "fixture-project"}, expiresAt: &expires}, protocol: "responses", toolRoundTrip: true, signedContinuation: true},
		{client: laneClient{name: "opencode", binary: "opencode", userAgent: "opencode (qualification fixture)"}, seed: laneSeed{adapter: "antigravity", profile: antigravityauth.ProfileID, manifest: antigravityauth.Manifest, token: "antigravity-fixture-token", principal: antigravityauth.Principal("fixture-subject", "fixture-project"), facts: map[string]string{"subject": "fixture-subject", "project_id": "fixture-project"}, expiresAt: &expires}, protocol: "chat", toolRoundTrip: true, signedContinuation: true},
		{client: laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification fixture)"}, seed: laneSeed{adapter: "antigravity", profile: antigravityauth.ProfileID, manifest: antigravityauth.Manifest, token: "antigravity-fixture-token", principal: antigravityauth.Principal("fixture-subject", "fixture-project"), facts: map[string]string{"subject": "fixture-subject", "project_id": "fixture-project"}, expiresAt: &expires}, protocol: "messages", toolRoundTrip: true, signedContinuation: true},
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

		Options: providers.Options{PluginOptions: func() map[string]string {
			if spec.adapter == string(codemode.AdapterAntigravity) {
				return map[string]string{"client_id": "fixture.apps.googleusercontent.com"}
			}
			return nil
		}(), Network: network,
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

func (s *service) enrollGrokSubscription(owner *caller, project string, proxy vendorProxy) (string, string) {
	s.t.Helper()
	modulePath := filepath.Join(s.t.TempDir(), "grok-build.wasm")
	build := exec.CommandContext(s.t.Context(), "go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-buildmode=c-shared", "-o", modulePath, "./internal/grokauth/grok")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		s.t.Fatalf("build Grok plugin: %v %s", err, output)
	}
	module, err := os.ReadFile(modulePath)
	if err != nil {
		s.t.Fatal(err)
	}
	request, err := http.NewRequestWithContext(s.t.Context(), http.MethodPost, s.origin+"/api/v1/plugins", bytes.NewReader(module))
	if err != nil {
		s.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/wasm")
	request.Header.Set("Origin", s.origin)
	request.Header.Set("X-CSRF-Token", owner.csrf)
	for _, cookie := range owner.cookies {
		request.AddCookie(cookie)
	}
	response, err := client.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer response.Body.Close()
	var plugin struct {
		Digest string `json:"digest"`
		ETag   string `json:"etag"`
	}
	if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&plugin) != nil {
		s.t.Fatalf("install real Grok WASM failed with status %d", response.StatusCode)
	}
	s.want(owner, "POST", "/api/v1/plugins/"+plugin.Digest+"/approve", map[string]any{"origins": grokauth.Manifest().Origins}, idempotent(map[string]string{"If-Match": strconv.Quote(plugin.ETag)}), http.StatusOK)
	provider := s.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": uniqueName("qualify-grok-grant"), "project_id": project,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "grant", "profile_id": grokauth.ProfileID, "profile_revision": plugin.Digest, "options": map[string]any{"network": map[string]string{"proxy_url": proxy.url, "trust_roots_pem": proxy.roots}}},
	}, idempotent(), http.StatusCreated)
	providerID := provider["id"].(string)
	enrollment := s.want(owner, "POST", "/api/v1/providers/"+providerID+"/grant-enrollments", map[string]any{}, idempotent(etag(provider)), http.StatusCreated)
	device := enrollment["device"].(map[string]any)
	if device["verification_url"] != "https://auth.x.ai/device" || device["user_code"] != "FIXTURE" {
		s.t.Fatal("device enrollment did not expose the controlled approval step")
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		status := s.want(owner, "POST", "/api/v1/providers/"+providerID+"/grant-enrollments/"+enrollment["id"].(string)+"/poll", nil, idempotent(), http.StatusOK)
		if status["status"] == "completed" {
			completion := status["completion"].(map[string]any)
			if completion["principal"] != grokauth.Principal("fixture-subject") {
				s.t.Fatal("device enrollment did not retain authenticated subject")
			}
			return providerID, completion["credential_id"].(string)
		}
		if status["status"] != "pending" {
			s.t.Fatal("device enrollment did not complete")
		}
		time.Sleep(250 * time.Millisecond)
	}
	s.t.Fatal("device enrollment did not complete before deadline")
	return "", ""
}

type grokGrantSnapshot struct {
	generation                          int64
	principal                           string
	facts                               map[string]string
	served                              connectors.GrantCredential
	refresh                             string
	accessCiphertext, refreshCiphertext []byte
}

func (s *service) grokGrant(credentialID string) grokGrantSnapshot {
	s.t.Helper()
	var snapshot grokGrantSnapshot
	var installation, refreshID string
	err := s.pool.QueryRow(s.t.Context(), `SELECT i.id::text,c.principal,c.grant_facts,g.generation,g.refresh_token_id::text,a.ciphertext,r.ciphertext
 FROM olp.installation i,olp.provider_credentials c JOIN olp.provider_grants g ON g.credential_id=c.id
 JOIN olp.secrets a ON a.id=c.id JOIN olp.secrets r ON r.id=g.refresh_token_id WHERE i.singleton AND c.id=$1`, credentialID).Scan(&installation, &snapshot.principal, &snapshot.facts, &snapshot.generation, &refreshID, &snapshot.accessCiphertext, &snapshot.refreshCiphertext)
	if err != nil {
		s.t.Fatal(err)
	}
	ring, err := secrets.ParseRing([]byte(s.env.master))
	if err != nil {
		s.t.Fatal(err)
	}
	served, err := ring.Read(s.t.Context(), s.pool, installation, credentialID, secrets.ProviderCredential)
	if err != nil {
		s.t.Fatal(err)
	}
	if json.Unmarshal(served, &snapshot.served) != nil {
		s.t.Fatal("invalid encrypted served grant")
	}
	refresh, err := ring.Read(s.t.Context(), s.pool, installation, refreshID, secrets.ProviderGrantRefresh)
	if err != nil {
		s.t.Fatal(err)
	}
	snapshot.refresh = string(refresh)
	if bytes.Contains(snapshot.accessCiphertext, []byte(snapshot.served.AccessToken)) || bytes.Contains(snapshot.refreshCiphertext, refresh) {
		s.t.Fatal("grant token persisted without encryption")
	}
	return snapshot
}

func (s *service) refreshGrokSubscription(credentialID string) {
	s.t.Helper()
	before := s.grokGrant(credentialID)
	if before.served.AccessToken != grokToken || before.refresh != grokRefreshToken || before.principal != grokauth.Principal("fixture-subject") || before.facts["subject"] != "fixture-subject" {
		s.t.Fatal("initial encrypted device grant did not retain verified identity and refresh continuity")
	}
	if _, err := s.pool.Exec(s.t.Context(), `UPDATE olp.provider_grants SET refresh_at=now() WHERE credential_id=$1`, credentialID); err != nil {
		s.t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var generation int64
		if err := s.pool.QueryRow(s.t.Context(), `SELECT generation FROM olp.provider_grants WHERE credential_id=$1`, credentialID).Scan(&generation); err != nil {
			s.t.Fatal(err)
		}
		if generation > before.generation {
			after := s.grokGrant(credentialID)
			if after.generation != before.generation+1 || after.principal != before.principal || !maps.Equal(after.facts, before.facts) || !maps.Equal(after.served.Facts, before.served.Facts) {
				s.t.Fatal("refresh changed grant identity or repeated rotation")
			}
			if after.served.AccessToken != grokRotatedToken || after.refresh != grokRotatedRefresh || bytes.Equal(after.accessCiphertext, before.accessCiphertext) || bytes.Equal(after.refreshCiphertext, before.refreshCiphertext) {
				s.t.Fatal("refresh did not rotate both encrypted grant tokens")
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	s.t.Fatal("real grant worker did not refresh the enrolled Grok grant before deadline")
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
	passwd := filepath.Join(t.TempDir(), "passwd")
	entry := fmt.Sprintf("fixture:x:%d:%d::/work:/bin/sh\n", os.Getuid(), os.Getgid())
	if err := os.WriteFile(passwd, []byte(entry), 0o600); err != nil {
		return "", false, err
	}
	options := slices.Concat([]string{
		"--unshare-net", "--unshare-pid", "--die-with-parent", "--new-session", "--clearenv",
		"--ro-bind", "/usr", "/usr", "--ro-bind", "/lib", "/lib", "--ro-bind", "/lib64", "/lib64", "--symlink", "usr/bin", "/bin",
		"--tmpfs", "/tmp", "--dir", "/work", "--dir", "/etc", "--ro-bind", passwd, "/etc/passwd", "--proc", "/proc", "--dev", "/dev",
		"--ro-bind", binary, "/client", "--chdir", "/work", "--setenv", "PATH", "/usr/bin:/bin",
		"--ro-bind", bridge, "/bridge.sock", "/usr/bin/env",
	}, env, []string{"/usr/bin/python3", "-c", sandboxBridge, "/client"}, args)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "bwrap", options...)
	command.Env = []string{}
	output, err := command.CombinedOutput()
	text := string(output)
	started := strings.Contains(text, "\x00client-started\x00")
	cleaned := strings.Replace(text, "\x00client-started\x00", "", 1)
	if err != nil && ctx.Err() != nil {
		return cleaned, started, ctx.Err()
	}
	return cleaned, started, err
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

func runFixtureClient(t *testing.T, client, binary, bridge, slug, key, configuration string, toolRoundTrip bool) (string, bool, error) {
	base := "http://127.0.0.1:18765/code/" + slug
	prompt := "Say hello."
	switch client {
	case "codex":
		if toolRoundTrip {
			prompt = "Execute printf olp-tool-round-trip with the shell tool, then say hello."
		}
		return sandboxClient(t, binary, bridge, []string{"OLP_API_KEY=" + key}, "exec", "--skip-git-repo-check", "--ephemeral", "-m", vendorModel,
			"-c", `model_provider="olp"`, "-c", `model_providers.olp.name="OLP"`, "-c", `model_providers.olp.base_url="`+base+`"`,
			"-c", `model_providers.olp.env_key="OLP_API_KEY"`, "-c", `model_providers.olp.wire_api="responses"`,
			"-c", `model_providers.olp.supports_websockets=false`, "-c", `check_for_update_on_startup=false`, "-c", `features.apply_patch_freeform=false`, "-c", `features.multi_agent=false`, "-c", `features.goals=false`, "-c", `web_search="disabled"`, "-c", `model_reasoning_summary="none"`, "-c", `model_reasoning_effort="none"`, "--dangerously-bypass-approvals-and-sandbox", prompt)
	case "claude-code":
		if toolRoundTrip {
			return sandboxClient(t, binary, bridge, fixtureConfigurationEnvironment(t, configuration, key), "--print", "--bare", "--no-session-persistence", "--tools", "Bash", "--allowedTools", "Bash", "--max-turns", "3", "--model", vendorModel, "Execute printf olp-tool-round-trip using Bash, then say hello.")
		}
		return sandboxClient(t, binary, bridge, []string{"ANTHROPIC_BASE_URL=" + base, "ANTHROPIC_AUTH_TOKEN=" + key, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"},
			"--print", "--bare", "--no-session-persistence", "--tools", "", "--model", vendorModel, prompt)
	case "opencode":
		toolPrompt := func() string {
			if toolRoundTrip {
				return "Execute printf olp-tool-round-trip using bash, then say hello."
			}
			return prompt
		}()
		config, err := json.Marshal(map[string]any{
			"model": "olp/" + vendorModel, "small_model": "olp/" + vendorModel, "enabled_providers": []string{"olp"},
			"permission": map[string]string{"bash": "allow"},
			"provider": map[string]any{"olp": map[string]any{"npm": "@ai-sdk/openai-compatible", "name": "OLP",
				"options": map[string]string{"baseURL": base + "/v1", "apiKey": "{env:OLP_API_KEY}"},
				"models":  map[string]any{vendorModel: map[string]string{"name": "Fixture"}}}},
		})
		if err != nil {
			return "", false, err
		}
		selectedConfig, selectedModel, configErr := func() ([]byte, string, error) {
			if configuration == "" {
				return config, "olp/" + vendorModel, nil
			}
			var generated struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal([]byte(configuration), &generated); err != nil {
				return nil, "", err
			}
			return []byte(configuration), generated.Model, nil
		}()
		if configErr != nil {
			return "", false, configErr
		}
		return sandboxClient(t, binary, bridge, []string{"OLP_API_KEY=" + key, "OPENCODE_CONFIG_CONTENT=" + string(selectedConfig), "OPENCODE_DISABLE_MODELS_FETCH=true", "OPENCODE_DISABLE_AUTOUPDATE=true"},
			"run", "--pure", "--model", selectedModel, toolPrompt)
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
	toolProof := make(chan struct{}, 1)
	proxy := newVendorProxy(t, newVendorMux(codexFixtureAccessToken(specs), toolProof))
	key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "qualification", "scopes": []string{"inference"}, "project_id": project}, idempotent(), http.StatusCreated)
	consumer := &caller{name: "qualification key", token: key["secret"].(string)}
	bridge := fixtureBridge(t, s.origin)

	for _, spec := range specs {
		t.Run(spec.client.name+"-"+spec.seed.adapter, func(t *testing.T) {
			providerID, credentialID := func() (string, string) {
				if spec.seed.adapter == "grok-build" {
					return s.enrollGrokSubscription(owner, project, proxy)
				}
				return s.seedSubscription(project, ownerID, spec.seed, proxy)
			}()
			account := s.want(owner, "POST", "/api/v1/code/accounts", map[string]any{
				"project_id": project, "provider_id": providerID, "credential_id": credentialID,
				"name": "qualify-" + spec.client.name, "enabled": true, "models": func() []string {
					if spec.signedContinuation {
						return []string{"gemini-fixture"}
					}
					return []string{vendorModel}
				}(),
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
				"project_id": project, "slug": slug, "pool_id": pool["id"], "models": func() []string {
					if spec.signedContinuation {
						return []string{"gemini-fixture"}
					}
					return []string{vendorModel}
				}(), "enabled": true, "messages_to_codex": spec.toolRoundTrip && spec.seed.adapter == "codex", "messages_codex_safety_identifier": spec.toolRoundTrip && spec.seed.adapter == "codex", "chat_to_codex": spec.toolRoundTrip && spec.seed.adapter == "codex", "responses_to_grok": spec.seed.adapter == "grok-build" && spec.protocol == "responses", "chat_to_grok": spec.seed.adapter == "grok-build" && spec.protocol == "chat", "messages_to_grok": spec.seed.adapter == "grok-build" && spec.protocol == "messages", "messages_grok_safety_identifier": spec.seed.adapter == "grok-build" && spec.protocol == "messages", "grok_cli_identity": spec.seed.adapter == "grok-build", "chat_to_antigravity": spec.signedContinuation && spec.protocol == "chat", "responses_to_antigravity": spec.signedContinuation && spec.protocol == "responses", "responses_antigravity_compatibility": spec.signedContinuation && spec.protocol == "responses", "messages_to_antigravity": spec.signedContinuation && spec.protocol == "messages", "messages_antigravity_safety_identifier": spec.signedContinuation && spec.protocol == "messages", "model_rewriting": spec.signedContinuation, "aliases": func() map[string]string {
					if spec.signedContinuation {
						return map[string]string{vendorModel: "gemini-fixture"}
					}
					return map[string]string{}
				}(),
			}, idempotent(), http.StatusCreated)
			s.want(owner, "POST", "/api/v1/code/routes/"+route["id"].(string)+"/publish", nil, idempotent(etag(route)), http.StatusOK)
			if spec.seed.adapter == "grok-build" {
				s.refreshGrokSubscription(credentialID)
			}

			lane := spec
			record := runLane(t, lane, bridge, func(binary string) (string, bool, error) {
				if err := waitForFixtureRoute(s, consumer, slug, lane.protocol); err != nil {
					return "", false, err
				}
				configuration := func() string {
					if !lane.toolRoundTrip || (lane.client.name != "claude-code" && lane.client.name != "opencode") {
						return ""
					}
					config := s.want(owner, "GET", "/api/v1/code/routes/"+route["id"].(string)+"/client-config?client="+lane.client.name+"&platform=linux&gateway_url=http%3A%2F%2F127.0.0.1%3A18765", nil, nil, http.StatusOK)
					return config["configuration"].(string)
				}()
				rawOutput, executed, err := runFixtureClient(t, lane.client.name, binary, bridge, slug, key["secret"].(string), configuration, lane.toolRoundTrip)
				output := strings.ReplaceAll(rawOutput, key["secret"].(string), "[redacted]")
				if err != nil {
					return output, executed, err
				}
				if !strings.Contains(output, vendorAnswer) {
					return output, executed, fmt.Errorf("journey did not return the fixture answer")
				}
				if lane.toolRoundTrip {
					select {
					case <-toolProof:
					default:
						return output, executed, fmt.Errorf("client did not execute the fixture tool and continue")
					}
				}
				return output, executed, nil
			})
			if lane.toolRoundTrip && lane.seed.adapter == "codex" && record.Result == "pass" {
				assertTranslatedCodeRates(t, s, owner, project, slug, pool, providerID)
				s.want(owner, "POST", "/api/v1/code/budgets", map[string]any{"project_id": project, "route_id": route["id"], "api_key_id": key["id"], "daily_tokens": 100, "enabled": true}, idempotent(), http.StatusCreated)
				consumer := &caller{name: "hard token bound", token: key["secret"].(string)}
				status, reply := s.do(consumer, "POST", "/code/"+slug+"/v1/messages", json.RawMessage(`{"model":"`+vendorModel+`","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`), nil)
				if status != http.StatusUnprocessableEntity || !strings.Contains(string(reply), "code_token_bound_unavailable") {
					t.Fatalf("incomplete bound admitted = %d %s", status, reply)
				}
			}

			report.complete(t, record)
			if record.Result != "pass" {
				t.Errorf("lane %s: result %s (%s)", lane.client.name, record.Result, record.Detail)
			}
			if record.Result == "pass" && !record.ClientExecuted {
				t.Errorf("lane %s: passing fixture must execute the client", lane.client.name)
			}
		})
	}
}

func TestLaneReportRetainsEveryAdapterScope(t *testing.T) {
	specs := lanes()
	codex := slices.IndexFunc(specs, func(spec lane) bool { return spec.seed.adapter == "codex" })
	grok := slices.IndexFunc(specs, func(spec lane) bool { return spec.seed.adapter == "grok-build" })
	if codex < 0 || grok < 0 {
		t.Fatal("report requires two distinct adapter fixtures")
	}
	first := specs[codex]
	second := lane{client: first.client, seed: specs[grok].seed, protocol: first.protocol}
	var output bytes.Buffer
	report := newLaneReport(&reportWriter{out: &output}, []lane{first, second})
	report.complete(t, first.record("pass", "fixture only", "test-version", true))
	report.flush(t)
	decoder := json.NewDecoder(&output)
	var completed, pending runRecord
	if err := decoder.Decode(&completed); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&pending); err != nil {
		t.Fatalf("second adapter lane disappeared: %v", err)
	}
	if completed.Adapter != first.seed.adapter || pending.Adapter != second.seed.adapter || pending.Result != "no_signal" {
		t.Fatalf("scoped report records = %+v, %+v", completed, pending)
	}
	var extra runRecord
	if decoder.Decode(&extra) != io.EOF {
		t.Fatalf("unexpected duplicate record %+v", extra)
	}
}

func assertTranslatedCodeRates(t *testing.T, s *service, owner *caller, project, slug string, pool map[string]any, provider string) {
	t.Helper()
	for _, path := range []string{"v1/messages", "v1/chat/completions"} {
		body := `{"model":"` + vendorModel + `","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`
		cap := max(1, len(body)/4) + 2
		key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "cap admission", "scopes": []string{"inference"}, "project_id": project, "tokens_per_minute": cap}, idempotent(), http.StatusCreated)
		if _, err := s.pool.Exec(t.Context(), `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, pool["id"], key["id"]); err != nil {
			t.Fatal(err)
		}
		consumer := &caller{name: "limited cap", token: key["secret"].(string)}
		status, reply := fixtureCapRequest(s, consumer, slug, path, body)
		if status != http.StatusBadRequest || !strings.Contains(string(reply), "code_rate_limited") {
			t.Fatalf("%s key effective cap admission = %d %s", path, status, reply)
		}
	}
	consumerKey := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "provider cap admission", "scopes": []string{"inference"}, "project_id": project}, idempotent(), http.StatusCreated)
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, pool["id"], consumerKey["id"]); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"v1/messages", "v1/chat/completions"} {
		body := `{"model":"` + vendorModel + `","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`
		cap := max(1, len(body)/4) + 2
		if _, err := s.pool.Exec(t.Context(), `UPDATE olp.providers SET configuration=jsonb_set(configuration::jsonb,'{options,limits}',jsonb_build_object('tokens_per_minute',$2::bigint)) WHERE id=$1`, provider, cap); err != nil {
			t.Fatal(err)
		}
		providerSlug := uniqueName("qualify-provider-cap")
		providerRoute := s.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": project, "slug": providerSlug, "pool_id": pool["id"], "models": []string{vendorModel}, "enabled": true, "messages_to_codex": true, "chat_to_codex": true}, idempotent(), http.StatusCreated)
		s.want(owner, "POST", "/api/v1/code/routes/"+providerRoute["id"].(string)+"/publish", nil, idempotent(etag(providerRoute)), http.StatusOK)
		consumer := &caller{name: "provider limited cap", token: consumerKey["secret"].(string)}
		status, reply := fixtureCapRequest(s, consumer, providerSlug, path, body)
		if status != http.StatusTooManyRequests || !strings.Contains(string(reply), "code_provider_rate_limited") {
			t.Fatalf("%s provider effective cap admission = %d %s", path, status, reply)
		}
	}
}

func fixtureCapRequest(s *service, consumer *caller, slug, path, body string) (int, []byte) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, reply := s.do(consumer, "POST", "/code/"+slug+"/"+path, json.RawMessage(body), nil)
		if (status != http.StatusUnauthorized && !(status == http.StatusNotFound && strings.Contains(string(reply), "code_route_unavailable"))) || time.Now().After(deadline) {
			return status, reply
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func fixtureConfigurationEnvironment(t *testing.T, configuration, key string) []string {
	t.Helper()
	command := exec.Command("bash", "-c", configuration+"\n/usr/bin/env -0")
	command.Env = []string{"OLP_API_KEY=" + key}
	command.Dir = t.TempDir()
	output, err := command.Output()
	if err != nil {
		t.Fatalf("generated setup failed: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
}

func codexFixtureAccessToken(specs []lane) string {
	for _, spec := range specs {
		if spec.seed.adapter == "codex" {
			return spec.seed.token
		}
	}
	return ""
}
