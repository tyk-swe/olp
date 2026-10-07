//go:build bench

package bench_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/tests/bench/mockupstream"
)

func execCombined(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// mockProcess is the mock upstream, a separate process so that it can be
// pinned to its own CPUs and never competes with the gateway.
type mockProcess struct {
	pid    int
	origin string
	client *http.Client
}

// startMock starts the mock with the roadmap's default behavior and waits for
// it to say where it listens.
func startMock(t *testing.T, s settings) *mockProcess {
	t.Helper()
	cmd := exec.Command(launcher(t, "mockupstream", s.MockBinary, s.MockCPUs), "-addr", "127.0.0.1:0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(t.TempDir(), "mockupstream.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the mock upstream: %v", err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
		if t.Failed() {
			if log, _ := os.ReadFile(logFile.Name()); len(log) > 0 {
				t.Logf("mock upstream log:\n%s", log)
			}
		}
		logFile.Close()
	})
	lines := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		lines <- line
	}()
	var announced struct{ Address string }
	select {
	case line := <-lines:
		if err := json.Unmarshal([]byte(line), &announced); err != nil || announced.Address == "" {
			t.Fatalf("the mock upstream announced %q: %v", line, err)
		}
	case <-done:
		log, _ := os.ReadFile(logFile.Name())
		t.Fatalf("the mock upstream exited before listening: %s", log)
	case <-time.After(15 * time.Second):
		t.Fatal("the mock upstream did not announce its address")
	}
	return &mockProcess{pid: cmd.Process.Pid, origin: "http://" + announced.Address, client: &http.Client{Timeout: 10 * time.Second}}
}

func (m *mockProcess) control(t testing.TB, method, path string, body any, out any) {
	t.Helper()
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, m.origin+path, payload)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("%s %s: status %d: %s", method, path, resp.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
}

// configure replaces the mock's whole behavior. The endpoint replaces rather
// than merges, so every model that must keep a rule is sent again.
func (m *mockProcess) configure(t testing.TB, spec mockupstream.Spec) {
	t.Helper()
	m.control(t, http.MethodPut, "/_mock/config", spec, nil)
}

// reset zeroes the mock's counters, including fail-first-n.
func (m *mockProcess) reset(t testing.TB) {
	t.Helper()
	m.control(t, http.MethodPost, "/_mock/reset", nil, nil)
}

func (m *mockProcess) stats(t testing.TB) mockupstream.Stats {
	t.Helper()
	var stats mockupstream.Stats
	m.control(t, http.MethodGet, "/_mock/stats", nil, &stats)
	return stats
}

// installation is one scenario's private state: a database no other scenario
// touches, which also gives it a Valkey namespace of its own, and the secrets
// the process reads.
type installation struct {
	DatabaseURL string
	// Name is the database's name, and ID the installation identity that names
	// its Valkey keys.
	Name, ID string
	Dir      string
	// Files holds the mounted secrets: the authentication key, the master key
	// ring and the bootstrap token.
	AuthKeyFile, MasterKeyFile, BootstrapFile string
	BootstrapToken                            string
}

// newInstallation creates and migrates a database and removes it, with the
// Valkey keys the installation wrote, when the test ends.
func newInstallation(t *testing.T, s settings) *installation {
	t.Helper()
	ctx := t.Context()
	admin, err := pgx.Connect(ctx, s.DatabaseURL)
	if err != nil {
		t.Fatalf("connect to PostgreSQL: %v", err)
	}
	in := &installation{Name: "bench_" + strings.ReplaceAll(randomUUID(t), "-", ""), Dir: t.TempDir()}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{in.Name}.Sanitize()); err != nil {
		admin.Close(ctx)
		t.Fatalf("create the scenario database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.Exec(ctx, "DROP DATABASE "+pgx.Identifier{in.Name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("drop the scenario database %s: %v", in.Name, err)
		}
		admin.Close(ctx)
	})
	u, err := url.Parse(s.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + in.Name
	in.DatabaseURL = u.String()

	in.AuthKeyFile = in.writeSecret(t, "auth.key", randomHex(t, 32))
	in.MasterKeyFile = in.writeSecret(t, "master.json", fmt.Sprintf(`{"active_version":1,"keys":[{"version":1,"key":%q}]}`, randomHex(t, 32)))
	in.BootstrapToken = randomHex(t, 24)
	in.BootstrapFile = in.writeSecret(t, "bootstrap.token", in.BootstrapToken)

	migrate := exec.CommandContext(ctx, s.GatewayBinary, "migrate")
	migrate.Env = testutil.Environment(in.environment())
	if out, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("migrate the scenario database: %v\n%s", err, out)
	}
	pool, err := database.Open(ctx, mustConfiguration(t, in.DatabaseURL))
	if err != nil {
		t.Fatalf("open the scenario database: %v", err)
	}
	defer pool.Close()
	if in.ID, err = database.Installation(ctx, pool); err != nil {
		t.Fatalf("read the installation identity: %v", err)
	}
	// The stream and the limiter counters live in the Valkey every scenario
	// shares and outlive the database that named them.
	t.Cleanup(func() { purgeValkey(t, s.ValkeyURL, database.ValkeyNamespace(in.ID)) })
	return in
}

func mustConfiguration(t testing.TB, rawURL string) *pgxpool.Config {
	t.Helper()
	cfg, err := database.Configuration(rawURL, 4, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func (in *installation) writeSecret(t testing.TB, name, content string) string {
	t.Helper()
	path := filepath.Join(in.Dir, name)
	if err := os.WriteFile(path, []byte(content+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// environment is what every process of the installation needs, whatever its
// mode. testutil strips the OLP_ variables of the calling environment, so
// nothing leaks in from the shell that started the benchmark.
func (in *installation) environment() map[string]string {
	return map[string]string{
		"OLP_DATABASE_URL":         in.DatabaseURL,
		"OLP_AUTH_HMAC_KEY_FILE":   in.AuthKeyFile,
		"OLP_MASTER_KEY_FILE":      in.MasterKeyFile,
		"OLP_BOOTSTRAP_TOKEN_FILE": in.BootstrapFile,
	}
}

func purgeValkey(t testing.TB, rawURL, prefix string) {
	t.Helper()
	cfg, err := coordination.Configuration(rawURL, "", 5*time.Second)
	if err != nil {
		t.Errorf("purge Valkey: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := coordination.Open(ctx, cfg)
	if err != nil {
		t.Errorf("purge Valkey: %v", err)
		return
	}
	defer c.Close()
	reply, err := c.Do(ctx, "KEYS", prefix+"*")
	if err != nil {
		t.Errorf("list the installation's Valkey keys: %v", err)
		return
	}
	// An installation that never reached Valkey owns nothing, which the server
	// reports as an empty reply rather than an empty list.
	keys, _ := reply.([]any)
	for _, key := range keys {
		if name, ok := key.(string); ok {
			if _, err := c.Do(ctx, "DEL", name); err != nil {
				t.Errorf("remove %s: %v", name, err)
			}
		}
	}
}

func randomUUID(t testing.TB) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

// gatewayProcess is the OLP process under test, started with `olp all` so that
// it serves inference and runs the worker plane that persists request
// metadata, as a single-node installation does.
type gatewayProcess struct {
	*testutil.Process
	pid     int
	public  string
	private string
	// Settings records the OLP_ settings the benchmark chose for the process,
	// which a result carries so it can be reproduced.
	Settings map[string]string
	Limits   gatewayLimits
	// VCPUs is how many CPUs the process may use, and Source says how that was
	// decided.
	VCPUs  int
	Source string
	CPUs   string
	// Instance is the gateway's identity in request-metadata epochs.
	Instance string
}

// gatewayLimits sizes what OLP bounds by default: connections, in-flight
// inference requests and each provider's connection pool. The defaults protect
// a small deployment (1,024 connections, 256 requests, and 64 upstream
// connections per provider) and would shed load a benchmark means to carry, so
// they are raised to the scenario's expected concurrency with room to spare.
type gatewayLimits struct {
	MaxConnections, MaxInFlight int
	// Providers is how many providers carry each upstream model, and Pool is
	// the connection pool of each. A provider takes at most 4,096 connections
	// to its host, so a load of more concurrent streams than that is spread
	// over several.
	Providers int
	Pool      networkTuning
}

// maxProviderConns is the most connections a provider's pool may be set to.
const maxProviderConns = 4096

func limitsFor(concurrency int) gatewayLimits {
	// A gateway that falls behind holds more requests than the load implies,
	// and limits that shed them would turn a slowdown into a collapse, so the
	// limits are several times what the load needs.
	headroom := 4*concurrency + 1024
	// The gateway picks among a model's providers at random, so each is sized
	// for its even share and then some.
	providers := max(1, (concurrency*13/10+maxProviderConns-1)/maxProviderConns)
	conns := min(maxProviderConns, max(512, 4*(concurrency/providers)+64))
	return gatewayLimits{
		MaxConnections: min(100000, headroom+1024),
		MaxInFlight:    min(100000, headroom),
		Providers:      providers,
		Pool:           networkTuning{MaxIdleConns: conns, MaxIdleConnsPerHost: conns, MaxConnsPerHost: conns},
	}
}

// startGateway starts `olp all` pinned to the gateway CPUs.
func startGateway(t *testing.T, s settings, in *installation, limits gatewayLimits) *gatewayProcess {
	t.Helper()
	assets := t.TempDir()
	if err := os.WriteFile(filepath.Join(assets, "index.html"), []byte("<html>benchmark</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	g := &gatewayProcess{Instance: "bench-gateway", Limits: limits, Settings: map[string]string{
		"OLP_HTTP_MAX_CONNECTIONS":                  strconv.Itoa(limits.MaxConnections),
		"OLP_HTTP_MAX_IN_FLIGHT_INFERENCE_REQUESTS": strconv.Itoa(limits.MaxInFlight),
		// Shadow traffic gets as much room as callers, so a mirrored scenario
		// measures mirroring rather than the drops of a small pool.
		"OLP_HTTP_MAX_IN_FLIGHT_SHADOW_REQUESTS": strconv.Itoa(limits.MaxInFlight),
		// Connections are kept for the whole run instead of being drained
		// after the default five minutes.
		"OLP_HTTP_CONNECTION_MAX_AGE_SECONDS":       "86400",
		"OLP_HTTP_CONNECTION_DRAIN_TIMEOUT_SECONDS": "60",
		"OLP_SHUTDOWN_TIMEOUT":                      "90s",
	}}
	env := in.environment()
	for name, value := range g.Settings {
		env[name] = value
	}
	env["OLP_VALKEY_URL"] = s.ValkeyURL
	env["OLP_LISTEN_ADDR"] = "127.0.0.1:0"
	env["OLP_OBSERVABILITY_LISTEN_ADDR"] = "127.0.0.1:0"
	env["OLP_PUBLIC_ORIGIN"] = benchOrigin
	env["OLP_CONSOLE_DIR"] = assets
	// The mock answers plain HTTP on loopback, which provider egress refuses
	// unless the operator opens it.
	env["OLP_PROVIDER_EGRESS_ALLOW_CIDRS"] = "127.0.0.0/8"
	env["OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS"] = "127.0.0.1"
	env["HOSTNAME"] = g.Instance

	g.Process = testutil.StartProcess(t, launcher(t, "olp", s.GatewayBinary, s.GatewayCPUs), "all", env)
	g.pid, g.public, g.private = g.Process.Pid(), g.PublicOrigin, g.PrivateOrigin
	status, err := readProcStatus(g.pid)
	if err != nil {
		t.Fatalf("read the gateway's /proc status: %v", err)
	}
	g.CPUs = status.CPUsAllowed
	if g.VCPUs, err = parseCPUList(g.CPUs); err != nil {
		t.Fatalf("the gateway reports CPUs %q: %v", g.CPUs, err)
	}
	g.Source = "affinity"
	if s.GatewayCPUs != "" {
		g.Source = "taskset"
		// Proof that the pin took, rather than trust in the wrapper.
		if want, _ := parseCPUList(s.GatewayCPUs); want != g.VCPUs {
			t.Fatalf("the gateway was to run on CPUs %s but may run on %s", s.GatewayCPUs, g.CPUs)
		}
	}
	return g
}

// scrape reads the private listener's metrics.
func (g *gatewayProcess) scrape(t testing.TB) metrics {
	t.Helper()
	m, err := g.tryScrape()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

var scrapeClient = &http.Client{Timeout: 10 * time.Second}

func (g *gatewayProcess) tryScrape() (metrics, error) {
	resp, err := scrapeClient.Get(g.private + "/metrics")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("scrape /metrics: status %d: %v", resp.StatusCode, err)
	}
	return parseMetrics(body), nil
}

// binaryDigest is the SHA-256 of a file, so a result names the exact build.
func binaryDigest(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

// binaryVersion asks the gateway binary for its version.
func binaryVersion(path string) string {
	out, err := exec.Command(path, "version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
