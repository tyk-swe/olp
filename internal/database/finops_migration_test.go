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

// A lowercase currency and an unknown source are refused.

// enqueue: the Stripe sink takes only its mapped project with a positive
// configured value; the plain HTTPS sink takes them all.

// mapped + positive
// zero: not reportable
// null: not reportable
// no consumer: unmapped
// unmapped project

func ptr(s string) *string { return &s }
