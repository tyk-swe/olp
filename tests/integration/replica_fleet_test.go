//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/internal/usage"
)

// Fleet scenarios run the real binary: every gateway replica and every worker
// plane below is a separate operating system process started against one shared
// database and one shared Valkey, so the behaviour under test is the behaviour
// the fleet has and not the behaviour a single in-process composition has.
//
// The console surface stays in the test process. It is the same access server
// the processes would compose, reading and writing the same database, which
// lets a scenario provision an installation and read its accounting back
// without a browser session against a second listener.

// fleetInstall is one provisioned installation together with the shared state its
// processes coordinate through.
type fleetInstall struct {
	t *testing.T
	// h is the console surface: access, the catalogue, routes, and the usage,
	// pricing and recovery reports the processes never serve themselves here.
	h      *accessHarness
	owner  *browser
	vendor *vendor
	valkey *coordination.Client
	binary string
	// secrets is the directory holding the mounted key files every process
	// reads, written from the same material the console surface was built with.
	secrets string
	// prefix is this installation's Valkey namespace; stream and namespace are
	// the request metadata stream and the limiter keyspace inside it.
	prefix    string
	stream    string
	namespace string
	provider  string
	path      string
}

// fleetConsole rebuilds the harness surface with everything the console owns,
// including the usage, pricing and recovery reports the accounting scenarios
// read back. The harness composes the catalogue itself, so it is rebuilt here
// against the same access server rather than reached through the harness mux.
func fleetConsole(t *testing.T) *accessHarness {
	t.Helper()
	h := newAccessHarness(t)
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts: []string{"127.0.0.1"}}
	mux := http.NewServeMux()
	management.Register(mux)
	h.Server.Register(mux)
	catalogue := providers.New(h.Server, &policy, nil)
	catalogue.Register(mux)
	(&management.Overview{Access: h.Server}).Register(mux)
	(&observability.Management{Access: h.Server, Cache: observability.NewCache(), Pool: h.Pool}).Register(mux)
	routes.New(h.Server).Register(mux)
	(&usage.Server{Access: h.Server, VendorKind: providers.VendorKind}).Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	h.HTTP = server
	return h
}

// fleetInstallation creates an installation nothing else shares: its own database,
// its own identity, and therefore its own Valkey namespace on the Valkey every
// installation in these scenarios shares.
func fleetInstallation(t *testing.T) *fleetInstall {
	t.Helper()
	h := fleetConsole(t)
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatalf("read installation identity: %v", err)
	}
	prefix := database.ValkeyNamespace(installation)
	valkey := client(t, required(t, "OLP_TEST_VALKEY_URL"), 5*time.Second)
	// Registered after the client's own cleanup, so the keys are removed while
	// the connection that has to remove them is still open.
	t.Cleanup(func() { fleetPurge(t, valkey, prefix) })
	dir := t.TempDir()
	for name, value := range map[string]string{"auth": h.AuthHex, "ring": h.Ring} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatalf("write %s key material: %v", name, err)
		}
	}
	return &fleetInstall{t: t, h: h, owner: h.owner(), valkey: valkey,
		binary: required(t, "OLP_TEST_BINARY"), secrets: dir, prefix: prefix,
		stream: usage.StreamName(prefix), namespace: prefix + "limits"}
}

// fleetProvisioned is an installation that can serve inference: one activated
// provider in front of the fixture vendor and one published route.
func fleetProvisioned(t *testing.T) *fleetInstall {
	t.Helper()
	in := fleetInstallation(t)
	in.vendor = newVendor(t)
	created := in.h.want(in.owner, http.MethodPost, "/api/v1/providers", map[string]any{
		"name": "Fixture vendor", "model": vendorModel, "credential": vendorSecret,
		"configuration": map[string]any{"kind": "openai_compatible", "auth_mode": "api_key",
			"endpoint": in.vendor.URL + "/v1"},
	}, map[string]string{"Idempotency-Key": "provider"}, 201)
	in.provider = created["id"].(string)
	in.path = "/api/v1/providers/" + in.provider
	if probe := in.h.want(in.owner, http.MethodPost, in.path+"/probe", nil,
		etagHeader(created), 200); probe["succeeded"] != true {
		t.Fatalf("probe failed: %v", probe)
	}
	models := in.h.want(in.owner, http.MethodGet, in.path+"/models", nil, nil, 200)
	model := models["items"].([]any)[0].(map[string]any)["id"].(string)
	detail := in.h.want(in.owner, http.MethodGet, in.path, nil, nil, 200)
	if certified := in.h.want(in.owner, http.MethodPost, in.path+"/models/"+model+"/certify", nil,
		etagHeader(detail), 200); certified["status"] != "certified" {
		t.Fatalf("certification failed: %v", certified)
	}
	detail = in.h.want(in.owner, http.MethodGet, in.path, nil, nil, 200)
	in.h.want(in.owner, http.MethodPost, in.path+"/activate", nil,
		withMatch(detail, map[string]string{"Idempotency-Key": "activate"}), 200)
	draft := in.h.want(in.owner, http.MethodPost, "/api/v1/route-drafts", map[string]any{
		"slug": routeSlug, "overall_timeout_ms": 20000, "max_attempts": 1, "fidelity": map[string]any{"mode": "transformed"},
		"targets": []any{map[string]any{"provider_id": in.provider, "provider_model": vendorModel,
			"priority": 0, "weight": 1, "timeout_ms": 15000}},
	}, map[string]string{"Idempotency-Key": "draft"}, 201)
	draftPath := "/api/v1/route-drafts/" + draft["id"].(string)
	validated := in.h.want(in.owner, http.MethodPost, draftPath+"/validate", nil, etagHeader(draft), 200)
	in.h.want(in.owner, http.MethodPost, draftPath+"/activate", nil,
		withMatch(validated, map[string]string{"Idempotency-Key": "route-activate"}), 200)
	return in
}

// key mints one inference key carrying the given policy. Keys are minted before
// the replicas start: a replica loads its authority once at startup and then
// polls, so provisioning first is what keeps a scenario free of that delay.
func (in *fleetInstall) key(name string, policy map[string]any) (string, string) {
	in.t.Helper()
	body := map[string]any{"name": name, "scopes": []string{"inference"},
		"allowed_routes": []string{routeSlug}}
	for field, value := range policy {
		body[field] = value
	}
	created := in.h.want(in.owner, http.MethodPost, "/api/v1/api-keys", body,
		map[string]string{"Idempotency-Key": "key-" + name}, 201)
	return created["id"].(string), created["secret"].(string)
}

// env is the configuration every process of this installation is started with.
// HOSTNAME is set explicitly because it is the gateway instance identity an
// epoch is recorded against and the prefix of a consumer's name: two replicas
// sharing one host must still be two producers and two consumers.
func (in *fleetInstall) env(hostname string) map[string]string {
	return map[string]string{
		"OLP_DATABASE_URL":             in.h.DBURL,
		"OLP_DATABASE_MAX_CONNECTIONS": "6",
		"OLP_VALKEY_URL":               required(in.t, "OLP_TEST_VALKEY_URL"),
		"OLP_AUTH_HMAC_KEY_FILE":       filepath.Join(in.secrets, "auth"),
		"OLP_MASTER_KEY_FILE":          filepath.Join(in.secrets, "ring"),
		"OLP_LISTEN_ADDR":              "127.0.0.1:0",
		// The fixture vendor answers plain HTTP on loopback, which provider
		// egress refuses unless the operator opened it deliberately.
		"OLP_OBSERVABILITY_LISTEN_ADDR":        "127.0.0.1:0",
		"OLP_PROVIDER_EGRESS_ALLOW_CIDRS":      "127.0.0.0/8",
		"OLP_PROVIDER_EGRESS_ALLOW_HTTP_HOSTS": "127.0.0.1",
		"OLP_SHUTDOWN_TIMEOUT":                 "8s",
		"HOSTNAME":                             hostname,
	}
}

// replica starts one inference process: admission, dispatch and the request
// metadata it emits, with no management surface and no worker plane.
func (in *fleetInstall) replica(hostname string) *testutil.Process {
	in.t.Helper()
	return testutil.StartProcess(in.t, in.binary, "gateway", in.env(hostname))
}

// worker starts one recovery plane: the metadata consumer, epoch detection,
// maintenance and the cost reconciliation leader.
func (in *fleetInstall) worker(hostname string) *testutil.Process {
	in.t.Helper()
	return testutil.StartProcess(in.t, in.binary, "worker", in.env(hostname))
}

// count runs one scalar query against the installation's database.
func (in *fleetInstall) count(query string, args ...any) int64 {
	in.t.Helper()
	return fleetCount(in.t, in.h.Pool, query, args...)
}

func fleetCount(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&count); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return count
}

// fleetChat sends one unary inference request to a replica and reports the status,
// the error code an OpenAI-shaped refusal carries, and the response headers.
func fleetChat(t *testing.T, origin, secret string) (int, string, http.Header) {
	t.Helper()
	body := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}]}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		origin+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 40 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("inference request to %s: %v", origin, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read inference response from %s: %v", origin, err)
	}
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, "", resp.Header
	}
	var problem struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &problem); err != nil {
		t.Fatalf("%d from %s is not an OpenAI error envelope: %s", resp.StatusCode, origin, raw)
	}
	return resp.StatusCode, problem.Error.Code, resp.Header
}

// fleetPurge removes everything this installation left in the shared Valkey. The
// stream and the limiter counters outlive the database that named them, so a
// scenario that does not clean up leaks keys into every later run.
func fleetPurge(t *testing.T, c *coordination.Client, prefix string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	reply, err := c.Do(ctx, "KEYS", prefix+"*")
	if err != nil {
		t.Errorf("list installation keys: %v", err)
		return
	}
	// An installation that never reached Valkey owns nothing to remove, which
	// the server reports as an empty reply rather than as an empty list.
	items, ok := reply.([]any)
	if !ok && reply != nil {
		t.Errorf("KEYS %s* = %#v", prefix, reply)
		return
	}
	for _, item := range items {
		key, ok := item.(string)
		if !ok {
			t.Errorf("KEYS %s* returned %#v", prefix, item)
			continue
		}
		if _, err := c.Do(ctx, "DEL", key); err != nil {
			t.Errorf("remove %s: %v", key, err)
		}
	}
}

// fleetKeys lists the shared Valkey keys an installation owns.
func fleetKeys(t *testing.T, c *coordination.Client, prefix string) []string {
	t.Helper()
	items := fleetList(t, do(t, c, "KEYS", prefix+"*"), "KEYS "+prefix+"*")
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, fleetText(t, item))
	}
	return keys
}

// fleetList reads a Valkey reply that is a list, treating the empty reply a
// missing key produces as an empty list rather than a protocol error.
func fleetList(t *testing.T, reply any, command string) []any {
	t.Helper()
	if reply == nil {
		return nil
	}
	items, ok := reply.([]any)
	if !ok {
		t.Fatalf("%s returned %#v, want a list", command, reply)
	}
	return items
}

// fleetText reads one Valkey reply value that must be a string.
func fleetText(t *testing.T, value any) string {
	t.Helper()
	switch text := value.(type) {
	case string:
		return text
	case []byte:
		return string(text)
	default:
		t.Fatalf("reply %#v is not a string", value)
		return ""
	}
}

// fleetFields reads a field container. Depending on the protocol in force these
// arrive as a map, as a flat name/value list, or as a list of name/value pairs,
// and every stream entry and XINFO row is read through one of those shapes.
func fleetFields(t *testing.T, value any) map[string]string {
	t.Helper()
	fields := map[string]string{}
	container, ok := value.([]any)
	if !ok {
		if mapped, ok := value.(map[string]any); ok {
			for name, item := range mapped {
				fields[name] = fleetScalar(item)
			}
			return fields
		}
		t.Fatalf("reply %#v is not a field container", value)
	}
	if len(container) > 0 {
		if _, paired := container[0].([]any); paired {
			for _, item := range container {
				pair, ok := item.([]any)
				if !ok || len(pair) != 2 {
					t.Fatalf("field pair %#v is not a name and a value", item)
				}
				fields[fleetScalar(pair[0])] = fleetScalar(pair[1])
			}
			return fields
		}
	}
	if len(container)%2 != 0 {
		t.Fatalf("field list %#v has an odd length", container)
	}
	for index := 0; index+1 < len(container); index += 2 {
		fields[fleetScalar(container[index])] = fleetScalar(container[index+1])
	}
	return fields
}

// fleetScalar renders one reply value as text. A field container mixes strings and
// counters, and every caller here compares or parses their text.
func fleetScalar(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return fmt.Sprint(value)
	}
}

// fleetConsumers names every consumer registered in the installation's persistence
// group.
func fleetConsumers(t *testing.T, c *coordination.Client, stream string) []string {
	t.Helper()
	items := fleetList(t, do(t, c, "XINFO", "CONSUMERS", stream, usage.Group), "XINFO CONSUMERS "+stream)
	names := make([]string, 0, len(items))
	for _, item := range items {
		fields := fleetFields(t, item)
		name, ok := fields["name"]
		if !ok {
			t.Fatalf("consumer %#v has no name", item)
		}
		names = append(names, name)
	}
	return names
}

// fleetCounters reads the durable worker counters of one installation.
func fleetCounters(t *testing.T, pool *pgxpool.Pool) (processed, duplicates, reclaimed int64) {
	t.Helper()
	err := pool.QueryRow(t.Context(), `SELECT request_metadata_processed_total,
            request_metadata_duplicates_total, request_metadata_reclaimed_total
        FROM olp.async_worker_counters WHERE singleton`).Scan(&processed, &duplicates, &reclaimed)
	if err != nil {
		t.Fatalf("read worker counters: %v", err)
	}
	return processed, duplicates, reclaimed
}

// The worker plane is part of the topology under test: nothing here depends
// on it, but the metadata these replicas emit must have a consumer.

// The window is a fixed minute of Valkey's clock. Starting a burst
// with most of one left is what keeps the count deterministic, and
// a burst that still straddled a boundary is retried on a key whose
// window is untouched rather than asserted against.

// Pricing is published before any traffic, so every attempt is priced at
// ingestion against a revision that was already in force when it was made.

// Nothing consumes yet, so the stream holds exactly what the replicas
// emitted and one payload can be captured for the duplicate delivery below.

// A delivery that arrives a second time is the shape a crash before
// acknowledgement leaves behind: it must be retired, never re-accounted.

// summary reads the console usage report over a range that starts before the
// first priced attempt and ends after the last.
func (in *fleetInstall) summary(from time.Time) map[string]any {
	in.t.Helper()
	query := "?start=" + url.QueryEscape(from.Add(-time.Hour).Format(time.RFC3339)) +
		"&end=" + url.QueryEscape(time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	return in.h.want(in.owner, http.MethodGet, "/api/v1/usage/summary"+query, nil, nil, 200)
}
