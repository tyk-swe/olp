//go:build integration

package export

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/secrets"
)

// received lives here, not with fakeStripe, so the default test build does
// not carry a method only integration tests call.
func (s *fakeStripe) received() []stripeHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stripeHit(nil), s.hits...)
}

// meteringFixtures seeds the smallest priced-attempt chain: an owner, a
// project-owned API key, a provider and one 3-per-million USD price.
type meteringFixtures struct {
	owner     string
	project   string
	key       string
	provider  string
	effective time.Time
}

func seedMeteringFixtures(t *testing.T, pool *pgxpool.Pool) meteringFixtures {
	t.Helper()
	f := meteringFixtures{
		owner:    uuid.NewString(),
		project:  uuid.NewString(),
		key:      uuid.NewString(),
		provider: uuid.NewString(),
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1::uuid,$2,'Metering','owner',$3::uuid)`,
			[]any{f.owner, "metering-" + f.owner + "@example.test", uuid.NewString()}},
		{`INSERT INTO olp.organizations(id,name,etag,created_by) VALUES($1::uuid,$2,$3::uuid,$4::uuid)`,
			[]any{uuid.NewString(), "org-" + f.owner, uuid.NewString(), f.owner}},
		{`INSERT INTO olp.projects(id,organization_id,name,etag,created_by)
		    SELECT $1::uuid,o.id,$2,$3::uuid,$4::uuid FROM olp.organizations o WHERE o.created_by=$4::uuid`,
			[]any{f.project, "metering-project", uuid.NewString(), f.owner}},
		{`INSERT INTO olp.api_keys(id,lookup_id,digest,name,project_id,created_by,policy,etag)
		    VALUES($1::uuid,$2,$3,'metering',$4::uuid,$5::uuid,'{}'::jsonb,$6::uuid)`,
			[]any{f.key, "lookup-" + f.key, []byte("digest"), f.project, f.owner, uuid.NewString()}},
		{`INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,active_revision,active_revision_id,created_by)
		    VALUES($1::uuid,$2,'openai','active',$3::jsonb,$4::uuid,$5::uuid,1,$6::uuid,$7::uuid)`,
			[]any{f.provider, "metering-" + f.provider,
				`{"kind":"openai","auth_mode":"api_key","endpoint":"https://127.0.0.1:9","options":{"vendor_id":"openai"}}`,
				uuid.NewString(), uuid.NewString(), uuid.NewString(), f.owner}},
		{`INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,source_etag,activated_by)
		    SELECT $1::uuid,$2::uuid,1,'metering',p.configuration,'[]'::jsonb,'[]'::jsonb,$3::uuid,$4::uuid
		    FROM olp.providers p WHERE p.id=$2::uuid`,
			[]any{uuid.NewString(), f.provider, uuid.NewString(), f.owner}},
	} {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.sql, err)
		}
	}
	// The provider's active_revision_id must point at its own revision.
	if _, err := pool.Exec(t.Context(), `UPDATE olp.providers p SET active_revision_id=r.id
	    FROM olp.provider_revisions r WHERE r.provider_id=p.id AND p.id=$1::uuid`, f.provider); err != nil {
		t.Fatal(err)
	}
	revision := uuid.NewString()
	f.effective = time.Now().UTC().Add(-time.Hour)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.pricing_revisions(id,revision,effective_at,created_by)
	    VALUES($1::uuid,1,$2,$3::uuid)`, revision, f.effective, f.owner); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.prices
	    (pricing_revision_id,provider_kind,model,operation,input_per_million,currency)
	    VALUES($1::uuid,'openai','metering-model','generation',3::numeric,'USD')`, revision); err != nil {
		t.Fatal(err)
	}
	return f
}

func insertSink(t *testing.T, pool *pgxpool.Pool, name, typ, format, destination string, streams []string, settings string, credentialID *string, projectID *string) string {
	t.Helper()
	sink := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.export_sinks
	    (id,name,project_id,type,destination,streams,format,filter,settings,enabled,etag,created_by,credential_id)
	    VALUES($1::uuid,$2,$3::uuid,$4,$5,$6::text[],$7,'{}'::jsonb,$8::jsonb,true,$9::uuid,
	    (SELECT id FROM olp.users LIMIT 1),$10::uuid)`,
		sink, name, projectID, typ, destination, streams, format, settings, uuid.NewString(), credentialID); err != nil {
		t.Fatalf("sink %s: %v", name, err)
	}
	return sink
}

func enqueueMetering(t *testing.T, pool *pgxpool.Pool, source, project string, occurred time.Time, payload string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(),
		`SELECT olp.enqueue_export('metering',$1::uuid,$2::uuid,'metering-route','success',$3,$4::jsonb)`,
		source, project, occurred, payload); err != nil {
		t.Fatal(err)
	}
}

func pendingFor(t *testing.T, pool *pgxpool.Pool, sink string) int64 {
	t.Helper()
	var count int64
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM olp.export_pending WHERE sink_id=$1::uuid AND delivered_at IS NULL`, sink).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func runWorker(t *testing.T, pool *pgxpool.Pool, installation string, ring *secrets.KeyRing) *Worker {
	t.Helper()
	sender := NewSender(matrixPolicy())
	sender.Installation = installation
	w := &Worker{Pool: pool, Keys: ring, Installation: installation, Sender: sender,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := w.Pass(t.Context()); err != nil {
		t.Fatal(err)
	}
	return w
}

// A priced attempt produces one metering record carrying the whole
// accounting payload, scoped to the consumer project.

// A committed attempt reports cost 0 but is still a priced attempt.

// An unpriced attempt produces none.

// TestIntegrationStripeDelivery sends a metering record through the fake
// Stripe that loses the first acknowledgement; the retried delivery carries
// the same identifier and lands exactly once.
func TestIntegrationStripeDelivery(t *testing.T) {
	pool := matrixPool(t)
	ring := matrixRing(t)
	fixtures := seedMeteringFixtures(t, pool)
	const installation = "stripe-installation"
	fake := &fakeStripe{}
	stripeAPI := httptest.NewServer(fake.handler())
	t.Cleanup(stripeAPI.Close)
	httpsReceiver := newStubReceiver(t)

	credential, _ := json.Marshal(map[string]any{"api_key": "rk_test_0123456789"})
	secretID := uuid.NewString()
	sealed, err := ring.Seal(installation, secrets.SinkCredential, secretID, credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.secrets(id,purpose,key_version,ciphertext)
	    VALUES($1,'sink_credential',1,$2)`, secretID, sealed); err != nil {
		t.Fatal(err)
	}
	stripeSink := insertSink(t, pool, "stripe", "stripe_meter", "stripe", stripeAPI.URL,
		[]string{"metering"}, `{"event_name":"olp.tokens","value":"input_tokens","customers":{"`+
			fixtures.project+`":"cus_ABC123"}}`, &secretID, nil)
	httpsSink := insertSink(t, pool, "metering-copy", "https", "json", httpsReceiver.URL,
		[]string{"metering"}, `{}`, nil, nil)

	source := uuid.NewString()
	// input_tokens reports the token count, not the charge.
	enqueueMetering(t, pool, source, fixtures.project, time.Now().UTC().Add(-time.Minute),
		`{"charge":"0.003000000000","input_tokens":1000,"output_tokens":250}`)
	// An unmapped project is rejected for the Stripe sink only — the HTTPS
	// sink on the same stream still takes it.
	enqueueMetering(t, pool, uuid.NewString(), uuid.NewString(), time.Now().UTC(),
		`{"charge":"0.003000000000","input_tokens":1000}`)
	// A null/zero value never reaches a Stripe pending row either.
	enqueueMetering(t, pool, uuid.NewString(), fixtures.project, time.Now().UTC(),
		`{"charge":null,"input_tokens":null,"output_tokens":null}`)
	enqueueMetering(t, pool, uuid.NewString(), fixtures.project, time.Now().UTC(),
		`{"charge":"0","input_tokens":0,"output_tokens":0}`)

	if pendingFor(t, pool, stripeSink) != 1 || pendingFor(t, pool, httpsSink) != 4 {
		t.Fatalf("pending stripe=%d https=%d", pendingFor(t, pool, stripeSink), pendingFor(t, pool, httpsSink))
	}

	// First pass: the fake records the event but drops the acknowledgement.
	wantID := "olp-" + strings.ReplaceAll(stripeSink, "-", "") + "-" + strings.ReplaceAll(source, "-", "")
	fake.loseAck = map[string]bool{wantID: true}
	runWorker(t, pool, installation, ring)
	if pendingFor(t, pool, stripeSink) != 1 {
		t.Fatal("lost acknowledgement was delivered")
	}
	if hits := fake.received(); len(hits) != 1 {
		t.Fatalf("stripe hits %d", len(hits))
	}
	first := fake.received()[0]
	if first.form.Get("identifier") != wantID || first.idem != wantID {
		t.Fatalf("identifier %q idem %q want %q", first.form.Get("identifier"), first.idem, wantID)
	}
	if first.form.Get("payload[value]") != "1000" || first.form.Get("payload[stripe_customer_id]") != "cus_ABC123" ||
		first.form.Get("event_name") != "olp.tokens" {
		t.Fatalf("meter form %v", first.form)
	}
	if first.auth != "Bearer rk_test_0123456789" {
		t.Fatalf("authorization %q", first.auth)
	}

	// Redelivery is a duplicate under Stripe's own idempotency — it still
	// clears the pending row, and the fake holds exactly one event.
	if _, err := pool.Exec(t.Context(), `UPDATE olp.export_pending SET next_attempt_at=now() WHERE sink_id=$1::uuid`, stripeSink); err != nil {
		t.Fatal(err)
	}
	runWorker(t, pool, installation, ring)
	if pendingFor(t, pool, stripeSink) != 0 {
		t.Fatal("duplicate delivery stayed pending")
	}
	hits := fake.received()
	if len(hits) != 2 {
		t.Fatalf("stripe hits %d", len(hits))
	}
	if hits[1].form.Get("identifier") != wantID || hits[1].idem != wantID {
		t.Fatalf("redelivery identifier %q", hits[1].form)
	}
	identifiers := map[string]int{}
	for _, hit := range hits {
		identifiers[hit.form.Get("identifier")]++
	}
	if len(identifiers) != 1 {
		t.Fatalf("identifiers %v", identifiers)
	}
}

func newStubReceiver(t *testing.T) *httptest.Server {
	r := &exportStubReceiver{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		w.WriteHeader(204)
	}))
	t.Cleanup(server.Close)
	return server
}

type exportStubReceiver struct {
	mu     sync.Mutex
	bodies [][]byte
}

func (r *exportStubReceiver) received() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.bodies...)
}
