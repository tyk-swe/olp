//go:build qualification

package qualification_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
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
		report.pending[l.client.name] = l.record("no_signal", "lane did not complete")
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
	client   laneClient
	seed     laneSeed
	path     string
	protocol string
	body     map[string]any
	headers  map[string]string
}

func (l lane) record(result, detail string) runRecord {
	version, _ := clientVersion(l.client.binary)
	return runRecord{
		Client: l.client.name, ClientVersion: version, ClientExecuted: false,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Adapter:  l.seed.adapter, Profile: l.seed.profile, Model: vendorModel, Protocol: l.protocol, Transport: "http",
		Result: result, Evidence: "fixture", Driver: "protocol-fixture", Detail: detail,
	}
}

func runLane(l lane, journey func() error) runRecord {
	if _, ok := clientVersion(l.client.binary); !ok {
		return l.record("no_signal", "client binary "+l.client.binary+" unavailable")
	}
	if err := journey(); err != nil {
		return l.record("fail", err.Error())
	}
	return l.record("pass", "")
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
			path:     "responses",
			protocol: "responses",
			body:     map[string]any{"model": vendorModel, "input": "hello"},
		},
		{
			client:   laneClient{name: "claude-code", binary: "claude", userAgent: "claude-cli (qualification fixture)"},
			seed:     laneSeed{adapter: "zai_coding", profile: codeplans.ZAIProfile, manifest: codeplans.ZAIManifest, token: zaiKey, principal: codeplans.Principal(codeplans.ZAIProfile, zaiKey), facts: map[string]string{}},
			path:     "v1/messages",
			protocol: "messages",
			headers:  map[string]string{"Anthropic-Version": "2023-06-01"},
			body:     map[string]any{"model": vendorModel, "max_tokens": 64, "messages": []any{map[string]any{"role": "user", "content": "hello"}}},
		},
		{
			client:   laneClient{name: "opencode", binary: "opencode", userAgent: "opencode (qualification fixture)"},
			seed:     laneSeed{adapter: "opencode_go", profile: codeplans.OpenCodeGoProfile, manifest: codeplans.OpenCodeGoManifest, token: openCodeGoKey, principal: codeplans.Principal(codeplans.OpenCodeGoProfile, openCodeGoKey), facts: map[string]string{}},
			path:     "v1/chat/completions",
			protocol: "chat",
			body:     map[string]any{"model": vendorModel, "messages": []any{map[string]any{"role": "user", "content": "hello"}}},
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

// seedSubscriptionE inserts the provider, credential, and grant rows grant
// enrollment would store for spec. A nil network leaves the connection on
// the adapter's own vendor address with ordinary egress, as production
// requires; the fixture lanes pass the fixture proxy instead.
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
		Kind: "plugin", AuthMode: "grant", ProfileID: spec.profile, ProfileRevision: digest, Endpoint: &address,
		Options: providers.Options{
			Network:           network,
			CredentialHeaders: []string{},
			Models:            map[string]json.RawMessage{},
			ParameterDefaults: map[string]json.RawMessage{},
		},
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

func TestMissingClientReportsNoSignal(t *testing.T) {
	called := false
	record := runLane(lane{
		client:   laneClient{name: "codex", binary: "olp-qualification-missing-client"},
		seed:     laneSeed{adapter: "codex"},
		protocol: "responses",
	}, func() error {
		called = true
		return nil
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
	proxy := newVendorProxy(t, newVendorMux(specs[0].seed.token))
	key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "qualification", "scopes": []string{"inference"}, "project_id": project}, idempotent(), http.StatusCreated)
	consumer := &caller{name: "qualification key", token: key["secret"].(string)}

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
		record := runLane(lane, func() error {
			deadline := time.Now().Add(45 * time.Second)
			last := ""
			for time.Now().Before(deadline) {
				headers := map[string]string{"User-Agent": lane.client.userAgent}
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
		report.complete(t, record)
		if record.Result != "pass" {
			t.Errorf("lane %s: result %s (%s)", lane.client.name, record.Result, record.Detail)
		}
	}
}
