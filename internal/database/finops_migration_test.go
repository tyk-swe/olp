//go:build integration

package database

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// The 0067 pairing matrix: which stream sets each type/format pair may carry.
// Every row of the matrix is accepted exactly as specified; anything outside
// it fails the check.
func TestIntegrationExportSinkPairingMatrix(t *testing.T) {
	pool := scratchPool(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users
	    (id, email, display_name, role, etag)
	    VALUES ($1::uuid, 'matrix@example.test', 'Matrix', 'owner', $2::uuid)`,
		user, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	sequence := 0
	sink := func(sinkType, format string, streams ...string) error {
		sequence++
		list := "{"
		for index, stream := range streams {
			if index > 0 {
				list += ","
			}
			list += stream
		}
		_, err := pool.Exec(t.Context(), fmt.Sprintf(`INSERT INTO olp.export_sinks
		    (id, name, type, destination, streams, format, etag, created_by)
		    VALUES ($1::uuid, $2, $3, 'https://127.0.0.1:9', '%s'::text[], $4, $5::uuid, $6::uuid)`, list+"}"),
			uuid.NewString(), fmt.Sprintf("sink-%d", sequence), sinkType, format, uuid.NewString(), user)
		return err
	}
	pairing := func(err error) bool {
		var refused *pgconn.PgError
		return errors.As(err, &refused) && refused.Code == "23514" && refused.ConstraintName == "export_sinks_check"
	}

	for _, legal := range []struct {
		sinkType string
		format   string
		streams  []string
	}{
		{"https", "json", []string{"requests"}},
		{"https", "json", []string{"requests", "attempts", "usage_rollups", "guardrail_decisions", "audit", "metering"}},
		{"https", "cloudevents", []string{"metering"}},
		{"otlp_logs", "otlp", []string{"attempts", "metering"}},
		{"s3", "jsonl", []string{"metering", "audit"}},
		{"gcs", "jsonl", []string{"requests"}},
		{"azure_blob", "jsonl", []string{"usage_rollups"}},
		{"s3", "csv_gzip", []string{"focus"}},
		{"gcs", "csv_gzip", []string{"focus"}},
		{"azure_blob", "csv_gzip", []string{"focus"}},
		{"stripe_meter", "stripe", []string{"metering"}},
	} {
		if err := sink(legal.sinkType, legal.format, legal.streams...); err != nil {
			t.Fatalf("legal %s+%s %v refused: %v", legal.sinkType, legal.format, legal.streams, err)
		}
	}
	for _, illegal := range []struct {
		sinkType string
		format   string
		streams  []string
	}{
		{"https", "json", []string{"focus"}},
		{"https", "json", []string{"metering", "focus"}},
		{"https", "cloudevents", []string{"requests", "metering"}},
		{"https", "cloudevents", []string{"requests"}},
		{"otlp_logs", "otlp", []string{"focus"}},
		{"s3", "jsonl", []string{"focus"}},
		{"s3", "jsonl", []string{"requests", "focus"}},
		{"s3", "csv_gzip", []string{"focus", "attempts"}},
		{"s3", "csv_gzip", []string{"metering"}},
		{"stripe_meter", "stripe", []string{"metering", "requests"}},
		{"stripe_meter", "stripe", []string{"focus"}},
	} {
		if err := sink(illegal.sinkType, illegal.format, illegal.streams...); !pairing(err) {
			t.Fatalf("illegal %s+%s %v was not refused by the pairing check: %v",
				illegal.sinkType, illegal.format, illegal.streams, err)
		}
	}
	// The remaining axes refuse their own mismatches.
	for _, illegal := range []struct {
		sinkType string
		format   string
		streams  []string
	}{
		{"https", "jsonl", []string{"requests"}},
		{"s3", "json", []string{"requests"}},
		{"stripe_meter", "json", []string{"metering"}},
		{"https", "stripe", []string{"metering"}},
	} {
		if err := sink(illegal.sinkType, illegal.format, illegal.streams...); !pairing(err) {
			t.Fatalf("illegal %s+%s %v was not refused: %v",
				illegal.sinkType, illegal.format, illegal.streams, err)
		}
	}
	// Streams beyond the declared set, or more than six, fail the streams check.
	for _, streams := range [][]string{
		{"unknown_stream"},
		{"requests", "requests", "attempts", "usage_rollups", "guardrail_decisions", "audit", "metering"},
	} {
		var refused *pgconn.PgError
		err := sink("https", "json", streams...)
		if !errors.As(err, &refused) || refused.ConstraintName != "export_sinks_streams_check" {
			t.Fatalf("streams %v was not refused by the streams check: %v", streams, err)
		}
	}
	// Settings must be an object of at most 128 KiB.
	var refused *pgconn.PgError
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.export_sinks
	    (id, name, type, destination, streams, format, etag, created_by, settings)
	    VALUES ($1::uuid, 'big-settings', 'https', 'https://127.0.0.1:9', '{requests}', 'json',
	            $2::uuid, $3::uuid, jsonb_build_object('pad', repeat('x', 200000)))`,
		uuid.NewString(), uuid.NewString(), user); !errors.As(err, &refused) ||
		refused.ConstraintName != "export_sinks_settings_check" {
		t.Fatalf("oversized settings was not refused: %v", err)
	}
}

// The 0067 foundation grants the billing credential its seal purpose and the
// FinOps workers their health tasks, and creates the FOCUS and billing tables.
func TestIntegrationFinopsFoundation(t *testing.T) {
	pool := scratchPool(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user, provider := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users
	    (id, email, display_name, role, etag)
	    VALUES ($1::uuid, 'finops@example.test', 'FinOps', 'owner', $2::uuid)`,
		user, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers
	    (id, name, kind, state, configuration, etag, slots_etag, created_by)
	    VALUES ($1::uuid, 'billing-provider', 'openai', 'draft', '{}', $2::uuid, $3::uuid, $4::uuid)`,
		provider, uuid.NewString(), uuid.NewString(), user); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.secrets
	    (id, purpose, key_version, ciphertext) VALUES ($1::uuid, $2, 1, $3)`,
		uuid.NewString(), "billing_credential", []byte("sealed")); err != nil {
		t.Fatalf("billing_credential purpose refused: %v", err)
	}
	for _, task := range []string{"focus_export", "billing_import"} {
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.worker_task_health
		    (task, checked_at) VALUES ($1, now())`, task); err != nil {
			t.Fatalf("task %s refused: %v", task, err)
		}
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.focus_dirty_days (day, version)
	    VALUES (current_date, nextval('olp.focus_dirty_version'))`); err != nil {
		t.Fatalf("focus_dirty_days: %v", err)
	}
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.provider_billing_imports
		    (provider_id, source, credential_id, etag, created_by)
		 SELECT $1::uuid, 'openai_costs', s.id, $2::uuid, $3::uuid
		 FROM olp.secrets s WHERE s.purpose = 'billing_credential'`,
			[]any{provider, uuid.NewString(), user}},
		{`INSERT INTO olp.provider_invoiced_costs (provider_id, day, line_item, currency, amount, source)
		 VALUES ($1::uuid, current_date, 'tokens', 'USD', 1.5, 'openai_costs')`,
			[]any{provider}},
	} {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatalf("%s: %v", statement.sql, err)
		}
	}
	// A lowercase currency and an unknown source are refused.
	var refused *pgconn.PgError
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_invoiced_costs
	    (provider_id, day, line_item, currency, amount, source)
	    VALUES ($1::uuid, current_date, 'other', 'usd', 1, 'openai_costs')`, provider); err == nil {
		t.Fatal("lowercase currency accepted")
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_billing_imports
	    (provider_id, source, etag, created_by)
	    VALUES ($1::uuid, 'aws_cur', $2::uuid, $3::uuid)`, provider, uuid.NewString(), user); !errors.As(err, &refused) {
		t.Fatalf("unknown billing source accepted: %v", err)
	}
}

// The 0067 metering stream: olp.metering_value resolves the configured amount
// and olp.export_sink_accepts gates a Stripe sink to mapped consumers with a
// positive value while other sinks follow the plain scope rules.
func TestIntegrationMeteringAcceptance(t *testing.T) {
	pool := scratchPool(t)
	if err := Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users
	    (id, email, display_name, role, etag)
	    VALUES ($1::uuid, 'metering@example.test', 'Metering', 'owner', $2::uuid)`,
		user, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	project := uuid.NewString()
	for _, value := range []struct {
		settings string
		payload  string
		want     *string
	}{
		{`{}`, `{"charge":"0.003000000000"}`, ptr("0.003000000000")},
		{`{"value":"cost"}`, `{"cost":"1.5"}`, ptr("1.5")},
		{`{"value":"input_tokens"}`, `{"input_tokens":1000}`, ptr("1000")},
		{`{"value":"output_tokens"}`, `{"output_tokens":250}`, ptr("250")},
		{`{"value":"total_tokens"}`, `{"input_tokens":10,"output_tokens":5}`, ptr("15")},
		{`{"value":"total_tokens"}`, `{"output_tokens":5}`, ptr("5")},
		{`{"value":"total_tokens"}`, `{"input_tokens":null,"output_tokens":null}`, nil},
		{`{"value":"requests"}`, `{}`, ptr("1")},
		{`{}`, `{"charge":null}`, nil},
		{`{}`, `{"charge":"0"}`, ptr("0")},
	} {
		var got *string
		if err := pool.QueryRow(t.Context(),
			`SELECT olp.metering_value($1::jsonb,$2::jsonb)::text`, value.settings, value.payload).
			Scan(&got); err != nil {
			t.Fatalf("metering_value(%s,%s): %v", value.settings, value.payload, err)
		}
		if (got == nil) != (value.want == nil) || got != nil && *got != *value.want {
			t.Fatalf("metering_value(%s,%s) = %v, want %v", value.settings, value.payload, got, value.want)
		}
	}

	// enqueue: the Stripe sink takes only its mapped project with a positive
	// configured value; the plain HTTPS sink takes them all.
	stripe := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.export_sinks
	    (id, name, type, destination, streams, format, etag, created_by, settings)
	    VALUES ($1::uuid, 'stripe', 'stripe_meter', 'https://api.stripe.com', '{metering}', 'stripe',
	            $2::uuid, $3::uuid,
	            jsonb_build_object('event_name','olp.tokens','value','input_tokens',
	                'customers', jsonb_build_object($4::text,'cus_test')))`,
		stripe, uuid.NewString(), user, project); err != nil {
		t.Fatal(err)
	}
	https := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.export_sinks
	    (id, name, type, destination, streams, format, etag, created_by)
	    VALUES ($1::uuid, 'https', 'https', 'https://127.0.0.1:9', '{metering}', 'json',
	            $2::uuid, $3::uuid)`, https, uuid.NewString(), user); err != nil {
		t.Fatal(err)
	}
	pending := func(sink string) int {
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.export_pending WHERE sink_id=$1::uuid`,
			sink).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	enqueue := func(project *string, payload string) {
		if _, err := pool.Exec(t.Context(), `SELECT olp.enqueue_export('metering',$1::uuid,$2::uuid,'route','success',now(),$3::jsonb)`,
			uuid.NewString(), project, payload); err != nil {
			t.Fatal(err)
		}
	}
	enqueue(&project, `{"input_tokens":1000}`)                                              // mapped + positive
	enqueue(&project, `{"input_tokens":0}`)                                                 // zero: not reportable
	enqueue(&project, `{"input_tokens":null}`)                                              // null: not reportable
	enqueue(nil, `{"input_tokens":1000}`)                                                   // no consumer: unmapped
	enqueue(func() *string { p := uuid.NewString(); return &p }(), `{"input_tokens":1000}`) // unmapped project
	if got := pending(stripe); got != 1 {
		t.Fatalf("stripe pending = %d, want 1", got)
	}
	if got := pending(https); got != 5 {
		t.Fatalf("https pending = %d, want 5", got)
	}
}

func ptr(s string) *string { return &s }
