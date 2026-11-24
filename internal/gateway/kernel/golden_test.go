package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/gateway/quota"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	"github.com/tyk-swe/olp/internal/limits/fleet"
	"github.com/tyk-swe/olp/internal/limits/scripts"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness/money"
)

type recordedEnvelope struct {
	Operation  string           `json:"operation"`
	Surface    string           `json:"surface"`
	Mode       string           `json:"mode"`
	Route      string           `json:"route"`
	Outcome    string           `json:"outcome"`
	Status     int              `json:"status"`
	ErrorClass string           `json:"error_class"`
	Attempts   []settledAttempt `json:"attempts,omitempty"`
}

type settledAttempt struct {
	recordedAttempt
	UsageComplete    bool `json:"usage_complete"`
	BillingUncertain bool `json:"billing_uncertain"`
}

type surfaceRecord struct {
	Status      int                `json:"status"`
	ContentType string             `json:"content_type"`
	Headers     map[string]string  `json:"headers,omitempty"`
	Body        any                `json:"body"`
	Upstream    int                `json:"upstream_calls"`
	Envelopes   []recordedEnvelope `json:"envelopes,omitempty"`
	Cost        string             `json:"cost,omitempty"`
	Holds       []money.Hold       `json:"holds,omitempty"`
}

var surfaceRefusalHeaders = []string{"Allow", "Retry-After", "Www-Authenticate", "X-Should-Retry", "Anthropic-Ratelimit-Requests-Reset"}

var volatileSurfaceText = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|req_[A-Za-z0-9]+|"created":[0-9]+`)

type surfaceHarness struct {
	*harness
	returned chan struct{}
	ledger   *money.Ledger
}

func newSurfaceHarness(t *testing.T, limited bool) *surfaceHarness {
	t.Helper()
	keys, err := secrets.ParseRingWithUnwrapper(context.Background(), fmt.Appendf(nil, `{"active_version":1,"keys":[{"version":1,"key":%q}]}`, strings.Repeat("01", 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	admission, ledger := money.Scripted(t)
	dependencies := Dependencies{Resources: resources.NewEncrypted(nil, "surface-golden", keys), Admission: admission}
	if limited {
		runner, err := scripts.NewRunner(unreachableValkey{}, "test")
		if err != nil {
			t.Fatal(err)
		}
		dependencies.Admission = quota.NewAdmission(limitadmission.NewDurable(runner, nil), fleet.New(runner), func() limitadmission.OutagePolicy { return limitadmission.FailClosed }, slog.New(slog.DiscardHandler))
	}
	h := newHarness(t, Config{MaxMediaBodyBytes: 1 << 20}, dependencies)
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	h.gateway.Deps.Media = &MediaDeps{Transport: transport.New(transport.Deps{
		Client: h.gateway.client, Auth: h.gateway.auth, Egress: h.gateway.Egress,
		Spool: spool, MaxResponseBytes: 1 << 20, Log: h.gateway.Log,
	})}
	h.gateway.Deps.Staging = spool
	h.gateway.Deps.Uploads = staging.NewAdmission(spool.CapacityBytes())
	requests := int64(60)
	for secret, authority := range h.rt.keys {
		authority.Policy.AllowProviderState = true
		if limited {
			authority.LookupID = "lookup" + secret
			authority.Policy.RequestsPerMinute = &requests
		}
		h.rt.keys[secret] = authority
	}
	mux := http.NewServeMux()
	RegisterGateway(h.gateway, mux)
	returned := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { returned <- struct{}{} }()
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	h.server = server
	return &surfaceHarness{harness: h, returned: returned, ledger: ledger}
}

func (h *surfaceHarness) price(t *testing.T, budgeted bool) {
	t.Helper()
	h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) {
		if budgeted {
			h.rt.keys[fullKey] = h.ledger.Budget(h.rt.keys[fullKey])
			h.ledger.Cap(snapshot, routeSlug)
		}
		h.rt.inputs = money.Inputs(t, snapshot, money.Operations...)
	})
}

func (h *surfaceHarness) record(t *testing.T, response *http.Response) surfaceRecord {
	t.Helper()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	<-h.returned
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	record := surfaceRecord{Status: response.StatusCode, ContentType: mediaType, Upstream: h.mock.count("a") + h.mock.count("b")}
	record.Body = h.stable(raw)
	if mediaType == "application/x-protobuf" {
		record.Body = digest(raw)
	}
	headers := map[string]string{}
	for _, name := range surfaceRefusalHeaders {
		if value := response.Header.Get(name); value != "" {
			headers[name] = value
		}
	}
	if len(headers) > 0 {
		record.Headers = headers
	}
	record.Envelopes = h.envelopes()
	record.Cost, record.Holds = response.Header.Get(money.CostHeader), h.ledger.Drain()
	return record
}

func (h *surfaceHarness) stable(raw []byte) any {
	scrubbed := volatileSurfaceText.ReplaceAllFunc(raw, func(match []byte) []byte {
		if bytes.HasPrefix(match, []byte(`"created"`)) {
			return []byte(`"created":0`)
		}
		return []byte("{id}")
	})
	if canonical, ok := canonicalJSON(scrubbed, h.upstream.URL); ok {
		return canonical
	}
	return strings.ReplaceAll(string(scrubbed), h.upstream.URL, "{upstream}")
}

func (h *surfaceHarness) envelopes() []recordedEnvelope {
	h.sink.mu.Lock()
	defer h.sink.mu.Unlock()
	recorded := make([]recordedEnvelope, 0, len(h.sink.envs))
	for _, envelope := range h.sink.envs {
		attempts := make([]settledAttempt, 0, len(envelope.Attempts))
		for _, attempt := range envelope.Attempts {
			settled := settledAttempt{
				recordedAttempt: recordedAttempt{Mode: attempt.Mode, Status: attempt.Status, Class: attempt.Class, Committed: attempt.Committed, Price: money.AttemptPrice(envelope.Operation, &attempt)},
				UsageComplete:   attempt.UsageComplete, BillingUncertain: attempt.BillingUncertain,
			}
			if attempt.Usage != nil {
				settled.Usage = attempt.Usage
			}
			attempts = append(attempts, settled)
		}
		recorded = append(recorded, recordedEnvelope{
			Operation: envelope.Operation, Surface: envelope.Surface, Mode: envelope.Mode, Route: envelope.Route,
			Outcome: envelope.Outcome, Status: envelope.Status, ErrorClass: envelope.ErrorClass, Attempts: attempts,
		})
	}
	return recorded
}

type goldenCase struct {
	name string
	run  func(*testing.T) any
}

func checkGolden(t *testing.T, path string, cases []goldenCase) {
	t.Helper()
	want := map[string]json.RawMessage{}
	if !*update {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	got := map[string]json.RawMessage{}
	t.Run("cases", func(t *testing.T) {
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				encoded, err := json.Marshal(c.run(t))
				if err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				got[c.name] = encoded
				mu.Unlock()
				if !*update && !jsonEqual(want[c.name], encoded) {
					t.Errorf("behavior changed\nwant %s\ngot  %s", want[c.name], encoded)
				}
			})
		}
	})
	if !*update {
		if len(want) != len(got) {
			t.Errorf("golden holds %d cases and the suite ran %d", len(want), len(got))
		}
		return
	}
	lines := make([]string, 0, len(got))
	for _, name := range slices.Sorted(maps.Keys(got)) {
		lines = append(lines, strconv.Quote(name)+": "+string(got[name]))
	}
	if err := os.WriteFile(path, []byte("{\n"+strings.Join(lines, ",\n")+"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
