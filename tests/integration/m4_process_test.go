//go:build integration

package integration_test

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// m4 scenarios run the real binary: every gateway replica and every worker
// plane below is a separate operating system process started against one shared
// database and one shared Valkey, so the behaviour under test is the behaviour
// the fleet has and not the behaviour a single in-process composition has.
//
// The console surface stays in the test process. It is the same access server
// the processes would compose, reading and writing the same database, which
// lets a scenario provision an installation and read its accounting back
// without a browser session against a second listener.

// m4Install is one provisioned installation together with the shared state its
// processes coordinate through.
type m4Install struct {
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

// m4Console rebuilds the harness surface with everything the console owns,
// including the usage, pricing and recovery reports the accounting scenarios
// read back. The harness composes the catalogue itself, so it is rebuilt here
// against the same access server rather than reached through the old mux.
func m4Console(t *testing.T) *accessHarness {
	t.Helper()
	h := newAccessHarness(t)
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts: []string{"127.0.0.1"}}
	mux := http.NewServeMux()
	management.Register(mux)
	h.Server.Register(mux)
	catalogue := providers.New(h.Server, &policy)
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

// m4Installation creates an installation nothing else shares: its own database,
// its own identity, and therefore its own Valkey namespace on the Valkey every
// installation in these scenarios shares.
func m4Installation(t *testing.T) *m4Install {
	t.Helper()
	h := m4Console(t)
	installation, err := database.Installation(t.Context(), h.Pool)
	if err != nil {
		t.Fatalf("read installation identity: %v", err)
	}
	prefix := database.ValkeyNamespace(installation)
	valkey := client(t, required(t, "OLP_TEST_VALKEY_URL"), 5*time.Second)
	// Registered after the client's own cleanup, so the keys are removed while
	// the connection that has to remove them is still open.
	t.Cleanup(func() { m4Purge(t, valkey, prefix) })
	dir := t.TempDir()
	for name, value := range map[string]string{"auth": h.AuthHex, "ring": h.Ring} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatalf("write %s key material: %v", name, err)
		}
	}
	return &m4Install{t: t, h: h, owner: h.owner(), valkey: valkey,
		binary: required(t, "OLP_TEST_BINARY"), secrets: dir, prefix: prefix,
		stream: usage.StreamName(prefix), namespace: prefix + "limits"}
}

// m4Provisioned is an installation that can serve inference: one activated
// provider in front of the fixture vendor and one published route.
func m4Provisioned(t *testing.T) *m4Install {
	t.Helper()
	in := m4Installation(t)
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
func (in *m4Install) key(name string, policy map[string]any) (string, string) {
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
func (in *m4Install) env(hostname string) map[string]string {
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
func (in *m4Install) replica(hostname string) *testutil.Process {
	in.t.Helper()
	return testutil.StartProcess(in.t, in.binary, "gateway", in.env(hostname))
}

// worker starts one recovery plane: the metadata consumer, epoch detection,
// maintenance and the cost reconciliation leader.
func (in *m4Install) worker(hostname string) *testutil.Process {
	in.t.Helper()
	return testutil.StartProcess(in.t, in.binary, "worker", in.env(hostname))
}

// count runs one scalar query against the installation's database.
func (in *m4Install) count(query string, args ...any) int64 {
	in.t.Helper()
	return m4Count(in.t, in.h.Pool, query, args...)
}

func m4Count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(t.Context(), query, args...).Scan(&count); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return count
}

// m4Chat sends one unary inference request to a replica and reports the status,
// the error code an OpenAI-shaped refusal carries, and the response headers.
func m4Chat(t *testing.T, origin, secret string) (int, string, http.Header) {
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

// m4Stream runs one streaming request against a replica to completion. Errors
// are returned rather than failed on: the caller runs it in its own goroutine.
func m4Stream(ctx context.Context, origin, secret string) (int, error) {
	body := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}],"stream":true}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		origin+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 40 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, err = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, err
}

// m4Eventually waits for a condition something outside the request path decides:
// a worker pass, a settled reservation, a drained stream.
func m4Eventually(t *testing.T, what string, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if check() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		select {
		case <-t.Context().Done():
			t.Fatalf("waiting for %s: %v", what, t.Context().Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// m4RetryAfter reads the whole number of seconds a refusal advertised.
func m4RetryAfter(t *testing.T, header http.Header) int {
	t.Helper()
	raw := header.Get("Retry-After")
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("Retry-After %q is not a whole number of seconds: %v", raw, err)
	}
	return seconds
}

// m4Purge removes everything this installation left in the shared Valkey. The
// stream and the limiter counters outlive the database that named them, so a
// scenario that does not clean up leaks keys into every later run.
func m4Purge(t *testing.T, c *coordination.Client, prefix string) {
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

// m4Keys lists the shared Valkey keys an installation owns.
func m4Keys(t *testing.T, c *coordination.Client, prefix string) []string {
	t.Helper()
	items := m4List(t, do(t, c, "KEYS", prefix+"*"), "KEYS "+prefix+"*")
	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, m4Text(t, item))
	}
	return keys
}

// m4List reads a Valkey reply that is a list, treating the empty reply a
// missing key produces as an empty list rather than a protocol error.
func m4List(t *testing.T, reply any, command string) []any {
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

// m4Text reads one Valkey reply value that must be a string.
func m4Text(t *testing.T, value any) string {
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

// m4Fields reads a field container. Depending on the protocol in force these
// arrive as a map, as a flat name/value list, or as a list of name/value pairs,
// and every stream entry and XINFO row is read through one of those shapes.
func m4Fields(t *testing.T, value any) map[string]string {
	t.Helper()
	fields := map[string]string{}
	container, ok := value.([]any)
	if !ok {
		if mapped, ok := value.(map[string]any); ok {
			for name, item := range mapped {
				fields[name] = m4Scalar(item)
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
				fields[m4Scalar(pair[0])] = m4Scalar(pair[1])
			}
			return fields
		}
	}
	if len(container)%2 != 0 {
		t.Fatalf("field list %#v has an odd length", container)
	}
	for index := 0; index+1 < len(container); index += 2 {
		fields[m4Scalar(container[index])] = m4Scalar(container[index+1])
	}
	return fields
}

// m4Scalar renders one reply value as text. A field container mixes strings and
// counters, and every caller here compares or parses their text.
func m4Scalar(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return fmt.Sprint(value)
	}
}

// m4Payloads reads the event payloads currently queued on the request metadata
// stream, oldest first. Acknowledged deliveries are deleted from the stream, so
// this is what no consumer has retired yet.
func m4Payloads(t *testing.T, c *coordination.Client, stream string) []string {
	t.Helper()
	entries := map[string]string{}
	reply := do(t, c, "XRANGE", stream, "-", "+")
	if mapped, ok := reply.(map[string]any); ok {
		for id, fields := range mapped {
			entries[id] = m4Event(t, id, fields)
		}
	} else {
		for _, item := range m4List(t, reply, "XRANGE "+stream) {
			tuple, ok := item.([]any)
			if !ok || len(tuple) != 2 {
				t.Fatalf("stream entry %#v is not an identifier and a field list", item)
			}
			id := m4Text(t, tuple[0])
			entries[id] = m4Event(t, id, tuple[1])
		}
	}
	// A map reply loses the server's order, so the entries are sorted by the
	// stream identifier they were appended under, which is that order.
	ids := slices.SortedFunc(maps.Keys(entries), m4CompareStreamIDs)
	payloads := make([]string, 0, len(ids))
	for _, id := range ids {
		payloads = append(payloads, entries[id])
	}
	return payloads
}

// m4Event reads the event payload one stream entry carries.
func m4Event(t *testing.T, id string, fields any) string {
	t.Helper()
	payload, ok := m4Fields(t, fields)["event"]
	if !ok {
		t.Fatalf("stream entry %s carries no event field", id)
	}
	return payload
}

// m4CompareStreamIDs orders stream identifiers by their milliseconds and
// sequence rather than as text, so entry 10-0 follows entry 9-0.
func m4CompareStreamIDs(a, b string) int {
	firstMillis, firstSequence, errA := usage.ParseStreamID(a)
	secondMillis, secondSequence, errB := usage.ParseStreamID(b)
	if errA != nil || errB != nil {
		return strings.Compare(a, b)
	}
	if firstMillis != secondMillis {
		return cmp.Compare(firstMillis, secondMillis)
	}
	return cmp.Compare(firstSequence, secondSequence)
}

// m4Consumers names every consumer registered in the installation's persistence
// group.
func m4Consumers(t *testing.T, c *coordination.Client, stream string) []string {
	t.Helper()
	items := m4List(t, do(t, c, "XINFO", "CONSUMERS", stream, usage.Group), "XINFO CONSUMERS "+stream)
	names := make([]string, 0, len(items))
	for _, item := range items {
		fields := m4Fields(t, item)
		name, ok := fields["name"]
		if !ok {
			t.Fatalf("consumer %#v has no name", item)
		}
		names = append(names, name)
	}
	return names
}

// m4Window is the fixed minute the shared limiter is counting in and how much of
// it is left, measured on Valkey's clock because that is the only clock the
// admission scripts consult.
func m4Window(t *testing.T, c *coordination.Client) (int64, time.Duration) {
	t.Helper()
	parts, ok := do(t, c, "TIME").([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("TIME = %#v", parts)
	}
	seconds, err := strconv.ParseInt(m4Text(t, parts[0]), 10, 64)
	if err != nil {
		t.Fatalf("TIME seconds: %v", err)
	}
	return seconds / 60, time.Duration(60-seconds%60) * time.Second
}

// m4Counters reads the durable worker counters of one installation.
func m4Counters(t *testing.T, pool *pgxpool.Pool) (processed, duplicates, reclaimed int64) {
	t.Helper()
	err := pool.QueryRow(t.Context(), `SELECT request_metadata_processed_total,
            request_metadata_duplicates_total, request_metadata_reclaimed_total
        FROM olp.async_worker_counters WHERE singleton`).Scan(&processed, &duplicates, &reclaimed)
	if err != nil {
		t.Fatalf("read worker counters: %v", err)
	}
	return processed, duplicates, reclaimed
}

// TestM4ReplicasShareOneAdmissionDecision proves that two inference processes
// admitting the same key are one gateway, not two: the request window is spent
// across replicas, and a concurrency slot one replica holds is a slot the other
// cannot take until it comes back.
func TestM4ReplicasShareOneAdmissionDecision(t *testing.T) {
	in := m4Provisioned(t)
	_, throttled := in.key("rpm", map[string]any{"requests_per_minute": 2})
	_, spare := in.key("rpm-retry", map[string]any{"requests_per_minute": 2})
	_, exclusive := in.key("concurrency", map[string]any{"max_concurrency": 1})
	a := in.replica("m4-admission-a")
	b := in.replica("m4-admission-b")
	// The worker plane is part of the topology under test: nothing here depends
	// on it, but the metadata these replicas emit must have a consumer.
	in.worker("m4-admission-worker")
	origins := []string{a.PublicOrigin, b.PublicOrigin, a.PublicOrigin, b.PublicOrigin}

	t.Run("a request window is spent across replicas", func(t *testing.T) {
		for attempt, secret := range []string{throttled, spare} {
			// The window is a fixed minute of Valkey's clock. Starting a burst
			// with most of one left is what keeps the count deterministic, and
			// a burst that still straddled a boundary is retried on a key whose
			// window is untouched rather than asserted against.
			if _, remaining := m4Window(t, in.valkey); remaining < 20*time.Second {
				time.Sleep(remaining)
			}
			window, _ := m4Window(t, in.valkey)
			dispatched := in.vendor.chats.Load()
			admitted, refused := 0, 0
			for _, origin := range origins {
				status, code, header := m4Chat(t, origin, secret)
				switch status {
				case http.StatusOK:
					admitted++
				case http.StatusTooManyRequests:
					refused++
					if code != "rate_limit_exceeded" {
						t.Fatalf("refusal from %s: %s", origin, code)
					}
					if retry := m4RetryAfter(t, header); retry < 1 || retry > 60 {
						t.Fatalf("Retry-After %d is outside the window it refers to", retry)
					}
				default:
					t.Fatalf("%s answered %d %s", origin, status, code)
				}
			}
			if current, _ := m4Window(t, in.valkey); current != window {
				if attempt == 0 {
					continue
				}
				t.Fatal("the request window rolled over twice while bursting")
			}
			if admitted != 2 || refused != 2 {
				t.Fatalf("a key limited to two requests per minute served %d and refused %d "+
					"across two replicas", admitted, refused)
			}
			if reached := in.vendor.chats.Load() - dispatched; reached != 2 {
				t.Fatalf("%d requests reached the upstream, want only the admitted two", reached)
			}
			return
		}
	})

	t.Run("a concurrency slot is held against every replica", func(t *testing.T) {
		in.vendor.delay.Store(int64(300 * time.Millisecond))
		defer in.vendor.delay.Store(0)
		served := in.vendor.chats.Load()
		streamed := make(chan int, 1)
		go func() {
			status, _ := m4Stream(t.Context(), a.PublicOrigin, exclusive)
			streamed <- status
		}()
		m4Eventually(t, "the upstream to take the request the first replica dispatched",
			15*time.Second, func() bool { return in.vendor.chats.Load() > served })
		status, code, header := m4Chat(t, b.PublicOrigin, exclusive)
		if status != http.StatusTooManyRequests || code != "rate_limit_exceeded" {
			t.Fatalf("the second replica answered %d %s while the slot was held", status, code)
		}
		if retry := m4RetryAfter(t, header); retry < 1 || retry > 5 {
			t.Fatalf("Retry-After %d for a slot that frees as soon as the request ends", retry)
		}
		if status := <-streamed; status != http.StatusOK {
			t.Fatalf("the streamed request ended %d", status)
		}
		m4Eventually(t, "the second replica to admit once the slot came back", 20*time.Second,
			func() bool {
				status, _, _ := m4Chat(t, b.PublicOrigin, exclusive)
				return status == http.StatusOK
			})
	})
}

// TestM4AccountingIsDurableAcrossReplicas proves that requests served by
// different processes become one ledger: every request is recorded once with the
// tokens the upstream reported and the cost the revision in force priced them
// at, the console reports the same totals, and a delivery that arrives twice
// changes neither.
func TestM4AccountingIsDurableAcrossReplicas(t *testing.T) {
	in := m4Provisioned(t)
	// Pricing is published before any traffic, so every attempt is priced at
	// ingestion against a revision that was already in force when it was made.
	effective := time.Now().UTC().Add(-time.Hour)
	in.h.want(in.owner, http.MethodPost, "/api/v1/pricing/revisions", map[string]any{
		"effective_at": effective.Format(time.RFC3339),
		"prices": []any{map[string]any{"provider_kind": "openai_compatible", "model": vendorModel,
			"operation": "generation", "currency": "USD",
			"input_per_million": "0.150000", "output_per_million": "0.600000"}},
	}, map[string]string{"Idempotency-Key": "pricing"}, 201)
	_, secret := in.key("accounted", nil)

	a := in.replica("m4-ledger-a")
	b := in.replica("m4-ledger-b")
	const requests = 6
	for i := range requests {
		origin := a.PublicOrigin
		if i%2 == 1 {
			origin = b.PublicOrigin
		}
		if status, code, _ := m4Chat(t, origin, secret); status != http.StatusOK {
			t.Fatalf("request %d to %s: %d %s", i, origin, status, code)
		}
	}
	// Nothing consumes yet, so the stream holds exactly what the replicas
	// emitted and one payload can be captured for the duplicate delivery below.
	m4Eventually(t, "both replicas to publish their request metadata", 30*time.Second,
		func() bool { return len(m4Payloads(t, in.valkey, in.stream)) == requests })
	replay := m4Payloads(t, in.valkey, in.stream)[0]

	in.worker("m4-ledger-worker")
	priced := "0.000004200000"
	m4Eventually(t, "every served request to become an accounted fact", 60*time.Second, func() bool {
		return in.count("SELECT count(*) FROM olp.attempt_usage_facts") == requests
	})
	if rows := in.count("SELECT count(*) FROM olp.requests"); rows != requests {
		t.Fatalf("requests = %d, want %d", rows, requests)
	}
	if rows := in.count("SELECT count(*) FROM olp.attempts"); rows != requests {
		t.Fatalf("attempts = %d, want %d", rows, requests)
	}
	if rows := in.count(`SELECT count(*) FROM olp.attempt_usage_facts
        WHERE input_tokens = 4 AND output_tokens = 6 AND usage_complete
          AND charge_status = 'billable' AND NOT unpriced AND currency = 'USD'
          AND estimated_cost::text = $1`, priced); rows != requests {
		t.Fatalf("%d of %d facts carry the reported tokens and the priced cost %s",
			rows, requests, priced)
	}

	total := "0.000025200000"
	summary := in.summary(effective)
	if summary["request_count"] != float64(requests) || summary["input_tokens"] != "24" ||
		summary["output_tokens"] != "36" || summary["estimated_cost"] != total ||
		summary["currency"] != "USD" || summary["unpriced_count"] != float64(0) {
		t.Fatalf("usage summary disagrees with the ledger: %v", summary)
	}

	// A delivery that arrives a second time is the shape a crash before
	// acknowledgement leaves behind: it must be retired, never re-accounted.
	_, duplicates, _ := m4Counters(t, in.h.Pool)
	if id := do(t, in.valkey, "XADD", in.stream, "*", "event", replay); id == nil {
		t.Fatal("republishing a delivered event produced no stream entry")
	}
	m4Eventually(t, "the consumer to retire the repeated delivery", 60*time.Second, func() bool {
		_, repeated, _ := m4Counters(t, in.h.Pool)
		return repeated > duplicates
	})
	if rows := in.count("SELECT count(*) FROM olp.attempt_usage_facts"); rows != requests {
		t.Fatalf("a repeated delivery created %d facts, want %d", rows, requests)
	}
	if rows := in.count("SELECT count(*) FROM olp.requests"); rows != requests {
		t.Fatalf("a repeated delivery created %d requests, want %d", rows, requests)
	}
	if again := in.summary(effective); again["estimated_cost"] != total ||
		again["request_count"] != float64(requests) {
		t.Fatalf("a repeated delivery changed what the installation spent: %v", again)
	}
}

// summary reads the console usage report over a range that starts before the
// first priced attempt and ends after the last.
func (in *m4Install) summary(from time.Time) map[string]any {
	in.t.Helper()
	query := "?start=" + url.QueryEscape(from.Add(-time.Hour).Format(time.RFC3339)) +
		"&end=" + url.QueryEscape(time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	return in.h.want(in.owner, http.MethodGet, "/api/v1/usage/summary"+query, nil, nil, 200)
}
