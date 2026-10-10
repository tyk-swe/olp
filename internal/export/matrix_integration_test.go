//go:build integration

package export

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secrets"
)

const matrixDBEnv = "OLP_TEST_DATABASE_ADMIN_URL"

func matrixPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv(matrixDBEnv)
	if admin == "" {
		t.Fatalf("%s is required; run make integration", matrixDBEnv)
	}
	cfg, err := pgxpool.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	dbName := fmt.Sprintf("olp_export_matrix_%s", strings.ReplaceAll(uuid.NewString()[:13], "-", ""))
	adminPool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adminPool.Exec(t.Context(), "CREATE DATABASE "+dbName); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = dbName
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := adminPool.Exec(context.Background(), "DROP DATABASE "+dbName+" WITH (FORCE)"); err != nil {
			t.Logf("drop scratch database: %v", err)
		}
		adminPool.Close()
	})
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func matrixRing(t *testing.T) *secrets.KeyRing {
	t.Helper()
	ring, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` +
		strings.Repeat("ab", 32) + `"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

func matrixPolicy() *egress.Policy {
	return &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
}

type fakeCloudAuth struct{ t *testing.T }

func (f *fakeCloudAuth) Apply(_ context.Context, req *http.Request, cfg connectors.Config, secret, _ []byte) (egress.Sensitive, error) {
	if cfg.Kind != "vertex_ai" || cfg.AuthMode != "service_account" {
		return egress.Sensitive{}, fmt.Errorf("unexpected Apply config %+v", cfg)
	}
	var v struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal(secret, &v); err != nil || v.ClientEmail == "" || v.PrivateKey == "" {
		f.t.Error("gcs service_account secret must carry client_email and private_key")
		return egress.Sensitive{}, connectors.ErrCredentialRejected
	}
	req.Header.Set("Authorization", "Bearer fakeGoogle")
	return egress.Sensitive{}, nil
}

func (f *fakeCloudAuth) ApplyAzureStorage(_ context.Context, req *http.Request, mode string, secret []byte) (egress.Sensitive, error) {
	if mode != "azure_client_secret" {
		return egress.Sensitive{}, fmt.Errorf("unexpected azure mode %q", mode)
	}
	var v struct {
		TenantID     string `json:"tenant_id"`
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.Unmarshal(secret, &v); err != nil || v.ClientSecret == "" {
		f.t.Error("azure_blob secret must carry the client_secret credential")
		return egress.Sensitive{}, connectors.ErrCredentialRejected
	}
	req.Header.Set("Authorization", "Bearer fakeAzure")
	return egress.Sensitive{}, nil
}

type objectHit struct {
	path        string
	auth        string
	blobType    string
	contentType string
	msVersion   string
	msDate      string
	body        []byte
}

type objectStore struct {
	mu   sync.Mutex
	hits []objectHit
}

func (s *objectStore) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.hits = append(s.hits, objectHit{
			path:        r.URL.EscapedPath(),
			auth:        r.Header.Get("Authorization"),
			blobType:    r.Header.Get("X-Ms-Blob-Type"),
			contentType: r.Header.Get("Content-Type"),
			msVersion:   r.Header.Get("X-Ms-Version"),
			msDate:      r.Header.Get("X-Ms-Date"),
			body:        body,
		})
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}
}

func (s *objectStore) received() []objectHit {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]objectHit, len(s.hits))
	copy(out, s.hits)
	return out
}

var matrixStreams = []string{"requests", "attempts", "usage_rollups", "guardrail_decisions", "audit"}

func TestIntegrationExportSinkMatrix(t *testing.T) {
	ctx := t.Context()
	pool := matrixPool(t)
	ring := matrixRing(t)
	const installation = "matrix-installation"

	owner := uuid.NewString()
	if _, err := pool.Exec(ctx, `INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,$2,'Owner','owner',$3)`, owner, owner+"@example.test", uuid.NewString()); err != nil {
		t.Fatal(err)
	}

	type envelope struct {
		eventID   string
		signature string
		raw       []byte
		body      map[string]any
	}
	var mu sync.Mutex
	var httpsHits, otlpHits []envelope
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Content-Type") == "application/x-protobuf" {
			request := collogspb.ExportLogsServiceRequest{}
			if err := proto.Unmarshal(body, &request); err != nil {
				t.Errorf("otlp payload is not a valid ExportLogsServiceRequest: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			otlpHits = append(otlpHits, envelope{eventID: r.Header.Get("X-OLP-Event-ID"), raw: body, body: map[string]any{"proto": true, "request": &request}})
			w.WriteHeader(http.StatusOK)
			return
		}
		var env map[string]any
		if err := json.Unmarshal(body, &env); err != nil {
			t.Errorf("https envelope: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		httpsHits = append(httpsHits, envelope{eventID: r.Header.Get("X-OLP-Event-ID"), signature: r.Header.Get("X-OLP-Signature"), raw: body, body: env})
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(receiver.Close)

	stores := map[string]*objectStore{"s3": {}, "gcs": {}, "azure_blob": {}}
	servers := map[string]*httptest.Server{}
	for name, store := range stores {
		servers[name] = httptest.NewServer(store.handler())
		t.Cleanup(servers[name].Close)
	}

	type fixture struct {
		typ        string
		dest       string
		credential map[string]any
		sender     *Sender
	}
	s3Sender := NewSender(matrixPolicy())
	cloudSender := NewSender(matrixPolicy())
	cloudSender.auth = &fakeCloudAuth{t}
	fixtures := []fixture{
		{typ: "https", dest: receiver.URL + "/json", credential: map[string]any{"signing_secret": "0123456789abcdef0123456789abcdef"}, sender: cloudSender},
		{typ: "otlp_logs", dest: receiver.URL + "/otlp", credential: map[string]any{}, sender: cloudSender},
		{typ: "s3", dest: servers["s3"].URL + "/bucket", credential: map[string]any{"auth_mode": "static", "region": "us-east-1",
			"secret": map[string]any{"access_key_id": "AKIAIOSFODNN7EXAMPLE", "secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}}, sender: s3Sender},
		{typ: "gcs", dest: servers["gcs"].URL + "/bucket", credential: map[string]any{"auth_mode": "service_account",
			"secret": map[string]any{"client_email": "fixture@example.iam", "private_key": "-----BEGIN PRIVATE KEY-----\\nfixture\\n-----END PRIVATE KEY-----\\n"}}, sender: cloudSender},
		{typ: "azure_blob", dest: servers["azure_blob"].URL + "/account/container", credential: map[string]any{"auth_mode": "azure_client_secret",
			"secret": map[string]any{"tenant_id": uuid.NewString(), "client_id": uuid.NewString(), "client_secret": "azure-secret-0123456"}}, sender: cloudSender},
	}

	recordID := map[string]map[string]string{}
	queuedAt := map[string]map[string]time.Time{}
	for _, fx := range fixtures {
		secretID, sinkID := uuid.NewString(), uuid.NewString()
		credential, err := json.Marshal(fx.credential)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := ring.Seal(installation, secrets.SinkCredential, secretID, credential)
		if err != nil {
			t.Fatal(err)
		}
		format := map[string]string{"https": "json", "otlp_logs": "otlp", "s3": "jsonl", "gcs": "jsonl", "azure_blob": "jsonl"}[fx.typ]
		if _, err := pool.Exec(ctx, `INSERT INTO olp.secrets(id,purpose,key_version,ciphertext) VALUES($1,'sink_credential',1,$2)`, secretID, sealed); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO olp.export_sinks(id,name,type,destination,credential_id,streams,format,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			sinkID, fx.typ+"-sink", fx.typ, fx.dest, secretID, matrixStreams, format, uuid.NewString(), owner); err != nil {
			t.Fatal(err)
		}
	}
	for _, stream := range matrixStreams {
		source := uuid.NewString()
		payload, _ := json.Marshal(map[string]any{"stream": stream, "marker": "matrix-" + stream})
		if _, err := pool.Exec(ctx, `SELECT olp.enqueue_export($1,$2::uuid,NULL,$3,'success',now(),$4::jsonb)`, stream, source, "matrix", payload); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT s.type,r.stream,r.id::text,r.queued_at FROM olp.export_pending p JOIN olp.export_records r ON r.id=p.record_id JOIN olp.export_sinks s ON s.id=p.sink_id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ, stream, id string
		var at time.Time
		if err := rows.Scan(&typ, &stream, &id, &at); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if recordID[typ] == nil {
			recordID[typ] = map[string]string{}
			queuedAt[typ] = map[string]time.Time{}
		}
		recordID[typ][stream] = id
		queuedAt[typ][stream] = at
	}
	rows.Close()

	for _, fx := range fixtures {
		if _, err := pool.Exec(ctx, `UPDATE olp.export_sinks SET enabled=(type=$1)`, fx.typ); err != nil {
			t.Fatal(err)
		}
		w := &Worker{Pool: pool, Keys: ring, Installation: installation, Sender: fx.sender, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if err := w.Pass(ctx); err != nil {
			t.Fatalf("%s: %v", fx.typ, err)
		}
		var pending int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM olp.export_pending p JOIN olp.export_sinks s ON s.id=p.sink_id WHERE s.type=$1 AND p.delivered_at IS NULL`, fx.typ).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending != 0 {
			t.Fatalf("%s: %d records stayed pending", fx.typ, pending)
		}
		var delivered int64
		if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(delivered_total),0) FROM olp.export_cursors c JOIN olp.export_sinks s ON s.id=c.sink_id WHERE s.type=$1`, fx.typ).Scan(&delivered); err != nil {
			t.Fatal(err)
		}
		if delivered != 5 {
			t.Fatalf("%s delivered=%d want 5", fx.typ, delivered)
		}
	}

	mu.Lock()
	https := httpsHits
	otlp := otlpHits
	mu.Unlock()

	if len(https) != 5 {
		t.Fatalf("https envelopes %d", len(https))
	}
	seenStreams := map[string]bool{}
	for _, hit := range https {
		env := hit.body
		stream, _ := env["stream"].(string)
		id, _ := env["event_id"].(string)
		seenStreams[stream] = true
		mac := hmac.New(sha256.New, []byte("0123456789abcdef0123456789abcdef"))
		mac.Write(hit.raw)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if hit.eventID == "" || hit.eventID != id || hit.signature != want {
			t.Errorf("https envelope event=%q id=%q signature=%q want=%s", hit.eventID, id, hit.signature, want)
		}
		if id != recordID["https"][stream] {
			t.Errorf("https %s id %q want %q", stream, id, recordID["https"][stream])
		}
	}
	for _, stream := range matrixStreams {
		if !seenStreams[stream] {
			t.Errorf("https missing stream %s", stream)
		}
	}

	if len(otlp) != 5 {
		t.Fatalf("otlp deliveries %d", len(otlp))
	}
	otlpStreams := map[string]int{}
	for _, hit := range otlp {
		req := hit.body["request"].(*collogspb.ExportLogsServiceRequest)
		for _, rl := range req.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, lr := range sl.LogRecords {
					var eventID, stream string
					for _, kv := range lr.Attributes {
						if kv.Key == "olp.event_id" {
							eventID = kv.Value.GetStringValue()
						}
						if kv.Key == "olp.stream" {
							stream = kv.Value.GetStringValue()
						}
					}
					otlpStreams[stream]++
					if eventID != recordID["otlp_logs"][stream] {
						t.Errorf("otlp %s event_id %q want %q", stream, eventID, recordID["otlp_logs"][stream])
					}
					if lr.TimeUnixNano != uint64(queuedAt["otlp_logs"][stream].UnixNano()) {
						t.Errorf("otlp %s time %d want %d", stream, lr.TimeUnixNano, queuedAt["otlp_logs"][stream].UnixNano())
					}
					var env map[string]any
					if err := json.Unmarshal([]byte(lr.Body.GetStringValue()), &env); err != nil {
						t.Fatalf("otlp body is not the JSON envelope: %v", err)
					}
					if env["event_id"] != recordID["otlp_logs"][stream] || env["stream"] != stream {
						t.Errorf("otlp body identity %v/%v", env["event_id"], env["stream"])
					}
				}
			}
		}
	}
	for _, stream := range matrixStreams {
		if otlpStreams[stream] != 1 {
			t.Errorf("otlp stream %s delivered %d times", stream, otlpStreams[stream])
		}
	}

	for name, store := range stores {
		hits := store.received()
		if len(hits) != 5 {
			t.Fatalf("%s puts %d", name, len(hits))
		}
		gotStreams := map[string]int{}
		for _, hit := range hits {
			if !strings.HasSuffix(string(hit.body), "\n") || !json.Valid(hit.body[:len(hit.body)-1]) {
				t.Fatalf("%s body is not jsonl: %q", name, hit.body)
			}
			if hit.contentType != "application/x-ndjson" {
				t.Errorf("%s content-type %q", name, hit.contentType)
			}
			var env map[string]any
			if err := json.Unmarshal(hit.body[:len(hit.body)-1], &env); err != nil {
				t.Fatalf("%s envelope: %v", name, err)
			}
			stream, _ := env["stream"].(string)
			id, _ := env["event_id"].(string)
			gotStreams[stream]++
			if id != recordID[name][stream] {
				t.Errorf("%s %s event_id %q want %q", name, stream, id, recordID[name][stream])
			}
			wantKey := "/" + objectKey(Record{ID: id, Stream: stream, At: queuedAt[name][stream]})
			if !strings.HasSuffix(hit.path, wantKey) {
				t.Errorf("%s key %q does not end with %q", name, hit.path, wantKey)
			}
		}
		for _, stream := range matrixStreams {
			if gotStreams[stream] != 1 {
				t.Errorf("%s stream %s delivered %d times", name, stream, gotStreams[stream])
			}
		}
	}
	for _, hit := range stores["s3"].received() {
		if !strings.Contains(hit.auth, "AWS4-HMAC-SHA256") || !strings.Contains(hit.auth, "Credential=AKIAIOSFODNN7EXAMPLE") {
			t.Errorf("s3 authorization %q", hit.auth)
		}
	}
	for _, hit := range stores["gcs"].received() {
		if hit.auth != "Bearer fakeGoogle" {
			t.Errorf("gcs authorization %q", hit.auth)
		}
	}
	for _, hit := range stores["azure_blob"].received() {
		if hit.auth != "Bearer fakeAzure" || hit.blobType != "BlockBlob" || hit.msVersion != "2023-11-03" {
			t.Errorf("azure headers auth=%q blob=%q version=%q", hit.auth, hit.blobType, hit.msVersion)
		}
	}

	if _, err := pool.Exec(ctx, `UPDATE olp.export_sinks SET enabled=true`); err != nil {
		t.Fatal(err)
	}
	w := &Worker{Pool: pool, Keys: ring, Installation: installation, Sender: cloudSender, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := w.Pass(ctx); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(httpsHits) != 5 || len(otlpHits) != 5 {
		t.Fatalf("redelivery after checkpoint: https=%d otlp=%d", len(httpsHits), len(otlpHits))
	}
	for name, store := range stores {
		if got := len(store.received()); got != 5 {
			t.Fatalf("%s redelivered: %d puts", name, got)
		}
	}
}
