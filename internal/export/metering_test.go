package export

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
)

// stubQueryer fakes the EXISTS probe customer validation needs and the
// retention lookup a focus start_date checks against.
type stubQueryer struct {
	exists    bool
	usageDays string
}

func (q stubQueryer) QueryRow(context.Context, string, ...any) pgx.Row {
	return stubRow{exists: q.exists, usageDays: q.usageDays}
}

func (q stubQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("Query is not used in settings validation")
}

type stubRow struct {
	exists    bool
	usageDays string
}

func (r stubRow) Scan(dest ...any) error {
	switch target := dest[0].(type) {
	case *bool:
		*target = r.exists
	case *string:
		if r.usageDays == "" {
			*target = "90"
		} else {
			*target = r.usageDays
		}
	}
	return nil
}

func testManagement() *Management {
	return &Management{Egress: &egress.Policy{}}
}

func newSink(format string, streams ...string) *Sink {
	return &Sink{Name: "sink", Type: "x", Destination: "https://example.test/hook",
		Format: format, Streams: streams}
}

func TestSinkFormatMatrix(t *testing.T) {
	m := testManagement()
	for _, tc := range []struct {
		name    string
		typ     string
		format  string
		streams []string
		wantErr string
	}{
		{"https json any", "https", "json", []string{"requests", "audit", "metering"}, ""},
		{"https json focus rejected", "https", "json", []string{"focus"}, "streams"},
		{"https cloudevents metering", "https", "cloudevents", []string{"metering"}, ""},
		{"https cloudevents requests rejected", "https", "cloudevents", []string{"metering", "requests"}, "streams"},
		{"https jsonl rejected", "https", "jsonl", []string{"requests"}, "format"},
		{"otlp audit", "otlp_logs", "otlp", []string{"audit", "metering"}, ""},
		{"otlp focus rejected", "otlp_logs", "otlp", []string{"focus"}, "streams"},
		{"otlp json rejected", "otlp_logs", "json", []string{"requests"}, "format"},
		{"s3 jsonl", "s3", "jsonl", []string{"requests", "metering"}, ""},
		{"s3 jsonl focus rejected", "s3", "jsonl", []string{"focus"}, "streams"},
		{"s3 csv focus", "s3", "csv_gzip", []string{"focus"}, ""},
		{"s3 csv requests rejected", "s3", "csv_gzip", []string{"requests"}, "streams"},
		{"s3 csv focus plus rejected", "s3", "csv_gzip", []string{"focus", "requests"}, "streams"},
		{"gcs csv focus", "gcs", "csv_gzip", []string{"focus"}, ""},
		{"azure csv focus", "azure_blob", "csv_gzip", []string{"focus"}, ""},
		{"azure json rejected", "azure_blob", "json", []string{"requests"}, "format"},
		{"stripe meter", "stripe_meter", "stripe", []string{"metering"}, ""},
		{"stripe requests rejected", "stripe_meter", "stripe", []string{"requests"}, "streams"},
		{"stripe json rejected", "stripe_meter", "json", []string{"metering"}, "format"},
		{"stripe empty streams", "stripe_meter", "stripe", []string{}, "streams"},
		{"unknown type", "ftp", "json", []string{"requests"}, "type"},
		{"duplicate streams", "https", "json", []string{"requests", "requests"}, "streams"},
		{"unknown stream", "https", "json", []string{"telemetry"}, "streams"},
	} {
		sink := newSink(tc.format, tc.streams...)
		sink.Type = tc.typ
		err := m.validateSink(sink)
		if tc.wantErr == "" {
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: accepted", tc.name)
		}
		if f := fieldOf(err); f != tc.wantErr {
			t.Fatalf("%s: field %q want %q (%v)", tc.name, f, tc.wantErr, err)
		}
	}
}

// fieldOf digs the field name out of an access.Invalid failure.
func fieldOf(err error) string {
	var problem *access.Problem
	if errors.As(err, &problem) {
		return problem.Field
	}
	return ""
}

func TestSinkStreamCardinality(t *testing.T) {
	m := testManagement()
	sink := newSink("json", "requests", "attempts", "usage_rollups", "guardrail_decisions", "audit", "metering")
	sink.Type = "https"
	if err := m.validateSink(sink); err != nil {
		t.Fatalf("six streams rejected: %v", err)
	}
	sink.Streams = append(sink.Streams, "attempts", "usage_rollups")
	if err := m.validateSink(sink); err == nil {
		t.Fatal("seven streams accepted")
	}
}

func TestFocusSinkRejectsOutcomeFilter(t *testing.T) {
	m := testManagement()
	sink := newSink("csv_gzip", "focus")
	sink.Type = "s3"
	sink.Filter = &Filter{Outcome: stringptr("success")}
	if err := m.validateSink(sink); err == nil {
		t.Fatal("focus sink accepted an outcome filter")
	}
	sink.Filter = &Filter{Route: stringptr("team-chat"), Project: stringptr(uuid.NewString())}
	// project and route filters stay legal — only the outcome is refused
	// before the destination check runs.
	if err := m.validateSink(sink); err != nil && fieldOf(err) == "filter.outcome" {
		t.Fatalf("route/project filters refused: %v", err)
	}
}

func stringptr(s string) *string { return &s }

func meteringSink(t *testing.T, raw string) *Sink {
	t.Helper()
	sink := newSink("stripe", "metering")
	sink.Type = "stripe_meter"
	sink.Settings = json.RawMessage(raw)
	return sink
}

func TestSettingsStrictDecode(t *testing.T) {
	m := testManagement()
	sink := meteringSink(t, `{"event_name":"tokens","customers":{},"surprise":1}`)
	if _, err := m.resolveSettings(t.Context(), stubQueryer{}, sink, nil); err == nil {
		t.Fatal("unknown settings field accepted")
	}
	sink = meteringSink(t, `{"event_name":"x"} {}`)
	if _, err := m.resolveSettings(t.Context(), stubQueryer{}, sink, nil); err == nil {
		t.Fatal("two documents accepted")
	}
	sink = meteringSink(t, `["array"]`)
	if _, err := m.resolveSettings(t.Context(), stubQueryer{}, sink, nil); err == nil {
		t.Fatal("non-object settings accepted")
	}
}

func TestSettingsDefaultsPersisted(t *testing.T) {
	m := testManagement()
	project := uuid.NewString()
	sink := meteringSink(t, `{"event_name":"olp.attempt","customers":{"`+project+`":"cus_123"}}`)
	canonical, err := m.resolveSettings(t.Context(), stubQueryer{exists: true}, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(canonical, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["value"] != "charge" || stored["customer_key"] != "stripe_customer_id" || stored["value_key"] != "value" {
		t.Fatalf("defaults not persisted: %s", canonical)
	}
	// Non-formatted sinks store an empty object.
	sink = newSink("json", "requests")
	sink.Type = "https"
	sink.Settings = nil
	canonical, err = m.resolveSettings(t.Context(), stubQueryer{}, sink, nil)
	if err != nil || string(canonical) != "{}" {
		t.Fatalf("empty settings: %s %v", canonical, err)
	}
	sink.Settings = json.RawMessage(`{"cost_basis":"cost"}`)
	if _, err := m.resolveSettings(t.Context(), stubQueryer{}, sink, nil); err == nil {
		t.Fatal("json sink accepted focus settings")
	}
}

func TestFocusSettings(t *testing.T) {
	m := testManagement()
	sink := newSink("csv_gzip", "focus")
	sink.Type = "s3"
	sink.Settings = json.RawMessage(`{}`)
	canonical, err := m.resolveSettings(t.Context(), stubQueryer{}, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	if err := json.Unmarshal(canonical, &stored); err != nil {
		t.Fatal(err)
	}
	if stored["cost_basis"] != "cost" {
		t.Fatalf("cost_basis default missing: %s", canonical)
	}
	today := time.Now().UTC().Format("2006-01-02")
	sink.Settings = json.RawMessage(`{"cost_basis":"charge","start_date":"` + today + `"}`)
	canonical, err = m.resolveSettings(t.Context(), stubQueryer{}, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"cost_basis":"list"}`,
		`{"start_date":"2020-01-01"}`,
		`{"start_date":"2999-01-01"}`,
		`{"start_date":"13-10-2026"}`,
		`{"source":"/x"}`,
		`{"event_name":"x"}`,
	} {
		sink.Settings = json.RawMessage(raw)
		if _, err := m.resolveSettings(t.Context(), stubQueryer{}, sink, nil); err == nil {
			t.Fatalf("%s accepted on a focus sink", raw)
		}
	}
}

func TestCloudEventsSettings(t *testing.T) {
	m := testManagement()
	sink := newSink("cloudevents", "metering")
	sink.Type = "https"
	for raw, ok := range map[string]bool{
		`{}`:                                            true,
		`{"source":"/olp/installations/x"}`:             true,
		`{"source":"https://example.test/e"}`:           true,
		`{"source":""}`:                                 false,
		`{"source":"with space"}`:                       false,
		`{"source":"tab\there"}`:                        false,
		`{"source":"` + strings.Repeat("x", 257) + `"}`: false,
		`{"event_name":"x"}`:                            false,
		`{"cost_basis":"cost"}`:                         false,
	} {
		sink.Settings = json.RawMessage(raw)
		_, err := m.resolveSettings(t.Context(), stubQueryer{}, sink, nil)
		if (err == nil) != ok {
			t.Fatalf("%s: err=%v", raw, err)
		}
	}
}

func TestStripeSettings(t *testing.T) {
	m := testManagement()
	project, other := uuid.NewString(), uuid.NewString()
	valid := `{"event_name":"olp.tokens","customers":{"` + project + `":"cus_ABC123"}}`
	sink := meteringSink(t, valid)
	if _, err := m.resolveSettings(t.Context(), stubQueryer{exists: true}, sink, nil); err != nil {
		t.Fatalf("valid stripe settings: %v", err)
	}
	for raw, field := range map[string]string{
		`{}`:                                  "settings.event_name",
		`{"event_name":"x y","customers":{}}`: "settings.event_name",
		`{"event_name":"x","customers":{}}`:   "settings.customers",
		`{"event_name":"x","customers":{"` + project + `":"sk_live_bad"}}`:                                    "settings.customers",
		`{"event_name":"x","customers":{"not-a-uuid":"cus_a"}}`:                                               "settings.customers",
		`{"event_name":"x","value":"exponent","customers":{"` + project + `":"cus_a"}}`:                       "settings.value",
		`{"event_name":"x","customers":{"` + project + `":"cus_a"},"customer_key":"bad-key!"}`:                "settings.customer_key",
		`{"event_name":"x","customers":{"` + project + `":"cus_a"},"customer_key":"same","value_key":"same"}`: "settings.value_key",
		`{"event_name":"x","customers":{"` + project + `":"cus_a"},"cost_basis":"cost"}`:                      "settings",
		`{"event_name":"x","customers":{"` + project + `":"cus_a"},"start_date":"2026-01-01"}`:                "settings",
	} {
		sink := meteringSink(t, raw)
		_, err := m.resolveSettings(t.Context(), stubQueryer{exists: true}, sink, nil)
		if err == nil {
			t.Fatalf("%s accepted", raw)
		}
		if got := fieldOf(err); got != field {
			t.Fatalf("%s: field %q want %q", raw, got, field)
		}
	}
	// A missing project is refused.
	sink = meteringSink(t, valid)
	if _, err := m.resolveSettings(t.Context(), stubQueryer{exists: false}, sink, nil); err == nil {
		t.Fatal("nonexistent project mapped")
	}
	// A project-scoped sink maps only itself.
	sink = meteringSink(t, `{"event_name":"x","customers":{"`+other+`":"cus_a"}}`)
	sink.ProjectID = &project
	if _, err := m.resolveSettings(t.Context(), stubQueryer{exists: true}, sink, nil); err == nil {
		t.Fatal("project sink mapped a foreign project")
	}
	sink.Settings = json.RawMessage(`{"event_name":"x","customers":{"` + project + `":"cus_a"}}`)
	if _, err := m.resolveSettings(t.Context(), stubQueryer{exists: true}, sink, nil); err != nil {
		t.Fatalf("own project refused: %v", err)
	}
	// Keys may be customised but must differ.
	sink.ProjectID = nil
	sink.Settings = json.RawMessage(`{"event_name":"x","customers":{"` + project + `":"cus_a"},"customer_key":"account","value_key":"amount"}`)
	canonical, err := m.resolveSettings(t.Context(), stubQueryer{exists: true}, sink, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stored map[string]any
	_ = json.Unmarshal(canonical, &stored)
	if stored["customer_key"] != "account" || stored["value_key"] != "amount" {
		t.Fatalf("keys not persisted: %s", canonical)
	}
}

func TestSinkCredentialRules(t *testing.T) {
	for _, tc := range []struct {
		name  string
		typ   string
		cred  string
		valid bool
	}{
		{"stripe api key", "stripe_meter", `{"api_key":"rk_test_123"}`, true},
		{"stripe missing key", "stripe_meter", `{"headers":{}}`, false},
		{"stripe bad prefix", "stripe_meter", `{"api_key":"pk_live_x"}`, false},
		{"stripe newline", "stripe_meter", `{"api_key":"sk_live_\nx"}`, false},
		{"stripe long", "stripe_meter", `{"api_key":"sk_live_` + strings.Repeat("x", 513) + `"}`, false},
		{"stripe extra secret", "stripe_meter", `{"api_key":"sk_live_x","secret":{"k":"v"}}`, false},
		{"stripe region", "stripe_meter", `{"api_key":"sk_live_x","region":"us-east-1"}`, false},
		{"https with api_key", "https", `{"api_key":"sk_live_x"}`, false},
		{"https signing secret", "https", `{"signing_secret":"0123456789abcdef0123456789abcdef"}`, true},
		{"s3 credential", "s3", `{"region":"us-east-1","auth_mode":"static","secret":{"access_key_id":"x","secret_access_key":"y"}}`, true},
		{"unknown field", "https", `{"surprise":1}`, false},
		{"no credential", "stripe_meter", ``, true},
	} {
		err := validateSinkCredential(tc.typ, json.RawMessage(tc.cred))
		if (err == nil) != tc.valid {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}

func TestMeteringValueParity(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		payload string
		want    string
		ok      bool
	}{
		{"charge", `{"charge":"0.003000000000"}`, "0.003", true},
		{"charge", `{"charge":null}`, "", false},
		{"charge", `{}`, "", false},
		{"charge", `{"charge":"0"}`, "0", true},
		{"cost", `{"cost":"1.5000"}`, "1.5", true},
		{"input_tokens", `{"input_tokens":1000}`, "1000", true},
		{"output_tokens", `{"output_tokens":"42"}`, "42", true},
		{"total_tokens", `{"input_tokens":1000,"output_tokens":250}`, "1250", true},
		{"total_tokens", `{"input_tokens":1000}`, "1000", true},
		{"total_tokens", `{"output_tokens":250}`, "250", true},
		{"total_tokens", `{"input_tokens":null,"output_tokens":null}`, "", false},
		{"total_tokens", `{}`, "", false},
		{"requests", `{}`, "1", true},
		{"charge", `{"charge":"not-a-number"}`, "", false},
		{"unknown", `{"charge":"1"}`, "", false},
	} {
		got, ok := meteringValue(tc.kind, json.RawMessage(tc.payload))
		if ok != tc.ok {
			t.Fatalf("%s %s: ok=%v want %v", tc.kind, tc.payload, ok, tc.ok)
		}
		if ok && plainDecimal(got) != tc.want {
			t.Fatalf("%s %s: %s want %s", tc.kind, tc.payload, plainDecimal(got), tc.want)
		}
	}
}

func TestMeteringValueSQLParity(t *testing.T) {
	// The Go mirror must derive the same amount as olp.metering_value; the SQL
	// side is covered by the integration tests over the same payloads.
	if got, _ := meteringValue("total_tokens", json.RawMessage(`{"input_tokens":7}`)); !got.Equal(decimal.NewFromInt(7)) {
		t.Fatal(got)
	}
}

func TestCloudEventGolden(t *testing.T) {
	var bodies [][]byte
	var eventID, contentType, signature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, body)
		eventID = r.Header.Get("X-OLP-Event-ID")
		contentType = r.Header.Get("Content-Type")
		signature = r.Header.Get("X-OLP-Signature")
		w.WriteHeader(200)
	}))
	defer server.Close()
	sender := NewSender(testPolicy())
	sender.Installation = "inst-1"
	credential, _ := json.Marshal(map[string]any{"signing_secret": "0123456789abcdef0123456789abcdef"})
	record := Record{
		ID:     "01900000-0000-7000-8000-0000000000aa",
		Stream: "metering",
		At:     time.Unix(1761000000, 0),
		Data:   json.RawMessage(`{"version":1,"event_id":"01900000-0000-7000-8000-0000000000aa","stream":"metering","source_id":"01900000-0000-7000-8000-0000000000bb","project_id":"01900000-0000-7000-8000-0000000000cc","route":"chat","outcome":"success","occurred_at":"2026-10-20T21:20:00.500000+00:00","data":{"attempt_id":"a","charge":"0.003000000000","input_tokens":1000}}`),
	}
	destination := Destination{ID: uuid.NewString(), Type: "https", URL: server.URL, Format: "cloudevents"}
	if err := sender.Send(t.Context(), destination, credential, record); err != nil {
		t.Fatal(err)
	}
	want := `{"specversion":"1.0","id":"01900000-0000-7000-8000-0000000000bb","source":"/olp/installations/inst-1","type":"dev.openllmproxy.attempt.priced","subject":"01900000-0000-7000-8000-0000000000cc","time":"2026-10-20T21:20:00.5Z","datacontenttype":"application/json","data":{"attempt_id":"a","charge":"0.003000000000","input_tokens":1000}}`
	if string(bodies[0]) != want {
		t.Fatalf("body:\n%s\nwant:\n%s", bodies[0], want)
	}
	if contentType != "application/cloudevents+json" || eventID != record.ID {
		t.Fatalf("headers: %q %q", contentType, eventID)
	}
	sum := hmac.New(sha256.New, []byte("0123456789abcdef0123456789abcdef"))
	sum.Write(bodies[0])
	if signature != "sha256="+hex.EncodeToString(sum.Sum(nil)) {
		t.Fatalf("signature %q does not cover the CloudEvent bytes", signature)
	}
	// A second send marshals identical bytes — redelivery is deduplicated.
	if err := sender.Send(t.Context(), destination, credential, record); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("redelivery differs:\n%s\n%s", bodies[0], bodies[1])
	}
}

func TestCloudEventSubjectAndSourceFallback(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(200)
	}))
	defer server.Close()
	sender := NewSender(testPolicy())
	sender.Installation = "inst-9"
	record := Record{ID: "r1", Stream: "metering", Data: json.RawMessage(`{"source_id":"01900000-0000-7000-8000-0000000000bb","project_id":null,"occurred_at":"2026-10-20T21:20:00+00:00","data":{}}`)}
	if err := sender.Send(t.Context(), Destination{ID: uuid.NewString(), Type: "https", URL: server.URL, Format: "cloudevents"}, nil, record); err != nil {
		t.Fatal(err)
	}
	if body["subject"] != "installation" || body["source"] != "/olp/installations/inst-9" {
		t.Fatalf("fallbacks: %v", body)
	}
	// A configured source wins.
	settings, _ := json.Marshal(map[string]any{"source": "https://billing.example.test/metering"})
	if err := sender.Send(t.Context(), Destination{ID: uuid.NewString(), Type: "https", URL: server.URL, Format: "cloudevents", Settings: settings}, nil, record); err != nil {
		t.Fatal(err)
	}
	if body["source"] != "https://billing.example.test/metering" {
		t.Fatalf("source: %v", body["source"])
	}
}

// fakeStripe records meter posts and answers like the API.
type fakeStripe struct {
	mu      sync.Mutex
	hits    []stripeHit
	seen    map[string]bool
	status  int
	loseAck map[string]bool
}

type stripeHit struct {
	path          string
	contentType   string
	auth          string
	idem          string
	stripeVersion string
	form          url.Values
	body          []byte
}

func (s *fakeStripe) handler() http.HandlerFunc {
	s.seen = map[string]bool{}
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(body))
		s.mu.Lock()
		s.hits = append(s.hits, stripeHit{r.URL.Path, r.Header.Get("Content-Type"),
			r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key"), r.Header.Get("Stripe-Version"), form, body})
		identifier := form.Get("identifier")
		duplicate := s.seen[identifier]
		s.seen[identifier] = true
		lose := s.loseAck != nil && s.loseAck[identifier]
		s.mu.Unlock()
		if s.status != 0 {
			w.WriteHeader(s.status)
			return
		}
		if duplicate {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"code":"duplicate_meter_event","message":"already reported"}}`))
			return
		}
		if lose {
			w.WriteHeader(502)
			return
		}
		w.WriteHeader(200)
	}
}

func (s *fakeStripe) received() []stripeHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stripeHit(nil), s.hits...)
}

var stripeIdentifier = regexp.MustCompile(`^olp-[0-9a-f]{32}-[0-9a-f]{32}$`)

func stripeDestination(server *httptest.Server, project string) (Destination, []byte) {
	settings, _ := json.Marshal(map[string]any{
		"event_name":   "olp.attempt",
		"value":        "charge",
		"customers":    map[string]string{project: "cus_ABC123"},
		"customer_key": "stripe_customer_id",
		"value_key":    "value",
	})
	credential, _ := json.Marshal(map[string]any{"api_key": "rk_test_0123456789"})
	return Destination{ID: uuid.NewString(), Type: "stripe_meter", URL: server.URL, Format: "stripe", Settings: settings}, credential
}

func meteringRecord(project, charge string) Record {
	return Record{ID: uuid.NewString(), Stream: "metering", At: time.Unix(1761000000, 0),
		Data: json.RawMessage(`{"source_id":"01900000-0000-7000-8000-0000000000bb","project_id":"` + project + `","occurred_at":"2026-10-20T21:20:00+00:00","data":{"charge":"` + charge + `","cost":"0.0015","input_tokens":1000,"output_tokens":250}}`)}
}

func TestStripeMeterRequest(t *testing.T) {
	project := uuid.NewString()
	fake := &fakeStripe{}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	sender := NewSender(testPolicy())
	destination, credential := stripeDestination(server, project)
	if err := sender.Send(t.Context(), destination, credential, meteringRecord(project, "0.003000000000")); err != nil {
		t.Fatal(err)
	}
	hits := fake.received()
	if len(hits) != 1 {
		t.Fatalf("hits %d", len(hits))
	}
	hit := hits[0]
	if hit.path != "/v1/billing/meter_events" || hit.contentType != "application/x-www-form-urlencoded" {
		t.Fatalf("request %s %s", hit.path, hit.contentType)
	}
	if hit.auth != "Bearer rk_test_0123456789" {
		t.Fatalf("auth %q", hit.auth)
	}
	form := hit.form
	identifier := form.Get("identifier")
	if !stripeIdentifier.MatchString(identifier) || len(identifier) != 69 {
		t.Fatalf("identifier %q", identifier)
	}
	if hit.idem != identifier {
		t.Fatalf("idempotency %q != %q", hit.idem, identifier)
	}
	if form.Get("event_name") != "olp.attempt" || form.Get("timestamp") != "1792531200" {
		t.Fatalf("form %v", form)
	}
	if form.Get("payload[stripe_customer_id]") != "cus_ABC123" || form.Get("payload[value]") != "0.003" {
		t.Fatalf("payload %v", form)
	}
	if hit.stripeVersion != "" {
		t.Fatalf("unexpected Stripe-Version header %q", hit.stripeVersion)
	}
	if strings.Contains(string(hit.body), "0.003000000000") {
		t.Fatalf("value carries trailing zeros: %s", hit.body)
	}
}

func TestStripeDuplicateIdentifierIsDelivered(t *testing.T) {
	project := uuid.NewString()
	fake := &fakeStripe{}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	sender := NewSender(testPolicy())
	destination, credential := stripeDestination(server, project)
	record := meteringRecord(project, "0.003")
	if err := sender.Send(t.Context(), destination, credential, record); err != nil {
		t.Fatal(err)
	}
	if err := sender.Send(t.Context(), destination, credential, record); err != nil {
		t.Fatalf("duplicate identifier: %v", err)
	}
	if len(fake.received()) != 2 {
		t.Fatalf("hits %d", len(fake.received()))
	}
}

func TestStripeStatusMapping(t *testing.T) {
	project := uuid.NewString()
	for status, code := range map[int]string{400: "status", 409: "status", 500: "status", 429: "status"} {
		fake := &fakeStripe{status: status}
		server := httptest.NewServer(fake.handler())
		sender := NewSender(testPolicy())
		destination, credential := stripeDestination(server, project)
		err := sender.Send(t.Context(), destination, credential, meteringRecord(project, "0.003"))
		if DeliveryErrorCode(err) != code {
			t.Fatalf("%d: %v", status, err)
		}
		server.Close()
	}
	// A non-duplicate 400 is still a status failure, and neither error carries
	// the API key or the response body.
	fake := &fakeStripe{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fake.mu.Lock()
		fake.hits = append(fake.hits, stripeHit{body: body})
		fake.mu.Unlock()
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"code":"other_failure","message":"reject rk_test_0123456789"}}`))
	}))
	defer server.Close()
	sender := NewSender(testPolicy())
	destination, credential := stripeDestination(server, project)
	err := sender.Send(t.Context(), destination, credential, meteringRecord(project, "0.003"))
	if DeliveryErrorCode(err) != "status" {
		t.Fatalf("other 400: %v", err)
	}
	if strings.Contains(err.Error(), "rk_test_0123456789") || strings.Contains(fmt.Sprint(err), "other_failure") {
		t.Fatalf("error leaked: %v", err)
	}
	var send *SendError
	if errors.As(err, &send) && send.Err != nil && strings.Contains(send.Err.Error(), "rk_test") {
		t.Fatalf("inner error leaked the key: %v", send.Err)
	}
}

func TestStripeUnmappedAndUnreportable(t *testing.T) {
	project := uuid.NewString()
	fake := &fakeStripe{}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	sender := NewSender(testPolicy())
	destination, credential := stripeDestination(server, project)
	for name, record := range map[string]Record{
		"unmapped project": meteringRecord(uuid.NewString(), "0.003"),
		"null value":       meteringRecord(project, ""),
		"zero value":       meteringRecord(project, "0"),
		"negative":         meteringRecord(project, "-1"),
	} {
		if name == "null value" {
			record.Data = json.RawMessage(`{"source_id":"01900000-0000-7000-8000-0000000000bb","project_id":"` + project + `","occurred_at":"2026-10-20T21:20:00+00:00","data":{"charge":null}}`)
		}
		err := sender.Send(t.Context(), destination, credential, record)
		if DeliveryErrorCode(err) != "unmapped" {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if len(fake.received()) != 0 {
		t.Fatal("unreportable records reached Stripe")
	}
}

func TestStripeValueKinds(t *testing.T) {
	project := uuid.NewString()
	fake := &fakeStripe{}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	sender := NewSender(testPolicy())
	for _, value := range []string{"cost", "input_tokens", "output_tokens", "total_tokens", "requests"} {
		settings, _ := json.Marshal(map[string]any{"event_name": "e", "value": value,
			"customers": map[string]string{project: "cus_x"}, "customer_key": "k", "value_key": "v"})
		credential, _ := json.Marshal(map[string]any{"api_key": "sk_test_x"})
		destination := Destination{ID: uuid.NewString(), Type: "stripe_meter", URL: server.URL, Format: "stripe", Settings: settings}
		if err := sender.Send(t.Context(), destination, credential, meteringRecord(project, "0.003")); err != nil {
			t.Fatalf("%s: %v", value, err)
		}
	}
	wants := map[string]string{"cost": "0.0015", "input_tokens": "1000", "output_tokens": "250", "total_tokens": "1250", "requests": "1"}
	hits := fake.received()
	if len(hits) != 5 {
		t.Fatalf("hits %d", len(hits))
	}
	got := map[string]int{}
	for _, hit := range hits {
		got[hit.form.Get("payload[v]")]++
		if hit.form.Get("payload[k]") != "cus_x" {
			t.Fatalf("customer key %v", hit.form)
		}
	}
	for value, want := range wants {
		if got[want] != 1 {
			t.Fatalf("%s: value %q deliveries %v", value, want, got)
		}
	}
}

func TestStripeCredentialRequired(t *testing.T) {
	project := uuid.NewString()
	fake := &fakeStripe{}
	server := httptest.NewServer(fake.handler())
	defer server.Close()
	sender := NewSender(testPolicy())
	destination, credential := stripeDestination(server, project)
	if err := sender.Send(t.Context(), destination, nil, meteringRecord(project, "0.003")); DeliveryErrorCode(err) != "credential" {
		t.Fatalf("missing credential: %v", err)
	}
	credential, _ = json.Marshal(map[string]any{"api_key": "pk_test_no"})
	if err := sender.Send(t.Context(), destination, credential, meteringRecord(project, "0.003")); DeliveryErrorCode(err) != "credential" {
		t.Fatalf("bad key: %v", err)
	}
	if len(fake.received()) != 0 {
		t.Fatal("request reached Stripe without a credential")
	}
}

func TestFocusStartDateRetentionBound(t *testing.T) {
	m := testManagement()
	sink := newSink("csv_gzip", "focus")
	sink.Type = "s3"
	old := time.Now().UTC().AddDate(0, 0, -200).Format("2006-01-02")
	// An unchanged start_date is re-accepted even once it has aged out.
	sink.Settings = json.RawMessage(`{"cost_basis":"cost","start_date":"` + old + `"}`)
	if _, err := m.resolveSettings(t.Context(), stubQueryer{}, sink, sink.Settings); err != nil {
		t.Fatalf("unchanged start_date refused: %v", err)
	}
	// Retention shortens the window: with 30-day usage retention the earliest
	// new start is today−29, inside the 90-day FOCUS window.
	sink.Settings = json.RawMessage(`{"start_date":"` + old + `"}`)
	if _, err := m.resolveSettings(t.Context(), stubQueryer{usageDays: "30"}, sink, nil); err == nil {
		t.Fatal("start_date older than retention accepted")
	}
	edge := time.Now().UTC().AddDate(0, 0, -29).Format("2006-01-02")
	sink.Settings = json.RawMessage(`{"start_date":"` + edge + `"}`)
	if _, err := m.resolveSettings(t.Context(), stubQueryer{usageDays: "30"}, sink, nil); err != nil {
		t.Fatalf("edge start_date refused: %v", err)
	}
	// A changed start_date is bounds-checked even when the stored one is old.
	if _, err := m.resolveSettings(t.Context(), stubQueryer{usageDays: "30"}, sink,
		json.RawMessage(`{"cost_basis":"cost","start_date":"`+old+`"}`)); err != nil {
		t.Fatalf("changed start_date refused: %v", err)
	}
}
