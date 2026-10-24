package resources

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

func retainedFixture(t *testing.T, name string) oif.Document {
	t.Helper()
	body, err := os.ReadFile("testdata/retained/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	document, err := oif.ParseJSON(body, oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestRetainedTerminalTrainingEvidence(t *testing.T) {
	now := time.Date(2026, 10, 23, 14, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		fixture, kind string
		tokens        int64
	}{
		{"openai-fine-tuning-failed", "fine_tuning_job", 9007199254740993},
		{"vertex-tuning-succeeded", "vertex_tuning_job", 1234567890123},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			r := &Resource{Kind: tc.kind, Metadata: json.RawMessage(`{"upstream_model":"training-model"}`), CreatedAt: now.Add(-2 * time.Hour)}
			e := retainedapi.Endpoint{Family: retainedapi.Family{Kind: tc.kind, Operation: "fine_tuning"}, Action: "get"}
			document := retainedFixture(t, tc.fixture)
			if err := EnrichRetained(r, e, document, now); err != nil {
				t.Fatal(err)
			}
			observed, err := ObserveRetained(r, e, document, now)
			if err != nil || !observed.RemoteTerminated || !observed.CoverageComplete || len(observed.Lines) != 1 || *observed.Lines[0].Usage.TrainingTokens != tc.tokens {
				t.Fatalf("observation=%+v err=%v", observed, err)
			}
			if _, err := NormalizeRetainedEvidence(observed.Lines[0]); err != nil {
				t.Fatal(err)
			}
			if !observed.ObservedAt.Equal(observed.Lines[0].End) {
				t.Fatal("closure differs from native coverage boundary")
			}
			if err := EnrichRetained(r, e, document, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			repeated, err := ObserveRetained(r, e, document, now.Add(time.Hour))
			if err != nil || !observed.Lines[0].End.Equal(repeated.Lines[0].End) {
				t.Fatal("polling extended already complete training usage")
			}
			missing, _ := oif.ParseJSON([]byte(`{"status":"failed"}`), oif.Limits{})
			if tc.kind == "vertex_tuning_job" {
				missing, _ = oif.ParseJSON([]byte(`{"state":"JOB_STATE_FAILED"}`), oif.Limits{})
			}
			unresolved, err := ObserveRetained(r, e, missing, now)
			if err != nil || !unresolved.RemoteTerminated || unresolved.CoverageComplete || len(unresolved.Lines) != 0 {
				t.Fatalf("missing paid usage cleared liability: %+v %v", unresolved, err)
			}
		})
	}
}

func TestRetainedEvalPerModelEvidence(t *testing.T) {
	now := time.Date(2026, 10, 23, 14, 0, 0, 0, time.UTC)
	r := &Resource{Kind: "eval_run", CreatedAt: now.Add(-2 * time.Hour), Metadata: json.RawMessage(`{"upstream_model":"sample-model","authorized_models":["sample-model","grader-model"]}`)}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Kind: r.Kind, Operation: "eval"}, Action: "get"}
	document := retainedFixture(t, "openai-eval-cancelled")
	if err := EnrichRetained(r, e, document, now); err != nil {
		t.Fatal(err)
	}
	observed, err := ObserveRetained(r, e, document, now)
	if err != nil || !observed.RemoteTerminated || !observed.CoverageComplete || len(observed.Lines) != 2 {
		t.Fatalf("cancelled paid work lost: %+v %v", observed, err)
	}
	for _, line := range observed.Lines {
		if _, err := NormalizeRetainedEvidence(line); err != nil {
			t.Fatal(err)
		}
	}
	bad, _ := oif.ParseJSON([]byte(strings.Replace(document.Raw(), `"grader-model"`, `"unapproved-model"`, 1)), oif.Limits{})
	if _, err := ObserveRetained(r, e, bad, now); err == nil {
		t.Fatal("unapproved grader gained billing authority")
	}
	missing, _ := oif.ParseJSON([]byte(`{"status":"failed","per_model_usage":null}`), oif.Limits{})
	if observation, err := ObserveRetained(r, e, missing, now); err != nil || observation.CoverageComplete || len(observation.Lines) != 0 {
		t.Fatal("omitted model usage interpreted as zero")
	}
	partial, _ := oif.ParseJSON([]byte(`{"status":"failed","per_model_usage":[{"model_name":"sample-model","invocation_count":0,"prompt_tokens":0,"completion_tokens":0,"cached_tokens":0,"total_tokens":0}]}`), oif.Limits{})
	if observation, err := ObserveRetained(r, e, partial, now); err != nil || observation.CoverageComplete || len(observation.Lines) != 1 {
		t.Fatal("unreported grader usage treated as zero")
	}
}

func TestRetainedCacheAuthoritativeExpiry(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 30, 0, 0, time.UTC)
	r := &Resource{Kind: "context_cache", Metadata: json.RawMessage(`{"upstream_model":"cache-model"}`)}
	e := retainedapi.Endpoint{Family: retainedapi.Family{Kind: r.Kind, Operation: "context_cache"}, Action: "get"}
	document := retainedFixture(t, "gemini-cache")
	if err := EnrichRetained(r, e, document, now); err != nil {
		t.Fatal(err)
	}
	if r.ExpiresAt == nil || !r.ExpiresAt.Equal(time.Date(2026, 10, 23, 13, 0, 0, 750000000, time.UTC)) {
		t.Fatalf("expiry=%v", r.ExpiresAt)
	}
	if observation, err := ObserveRetained(r, e, document, now); err != nil || len(observation.Lines) != 0 {
		t.Fatal("snapshot was billed as an interval")
	}
	if observation, err := ObserveRetainedExpiry(r, now, true); err != nil || observation.RemoteTerminated {
		t.Fatal("early disappearance treated as expiry")
	}
	if observation, err := ObserveRetainedExpiry(r, now.Add(time.Hour), false); err != nil || observation.RemoteTerminated {
		t.Fatal("unverified expiry settled")
	}
	observation, err := ObserveRetainedExpiry(r, now.Add(time.Hour), true)
	if err != nil || !observation.CoverageComplete || len(observation.Lines) != 1 || *observation.Lines[0].Usage.CacheTokenSeconds != "3604100.5" {
		t.Fatalf("exact expiry=%+v error=%v", observation, err)
	}
	for _, raw := range []string{strings.Replace(document.Raw(), `models/cache-model`, `models/other-model`, 1), strings.Replace(document.Raw(), `1001`, `1002`, 1), strings.Replace(document.Raw(), `12:00:00.25Z`, `12:00:00.26Z`, 1)} {
		changed, _ := oif.ParseJSON([]byte(raw), oif.Limits{})
		if err := EnrichRetained(r, e, changed, now); err == nil {
			t.Fatal("immutable cache authority changed")
		}
	}
	extended, _ := oif.ParseJSON([]byte(strings.Replace(document.Raw(), `13:00:00.75Z`, `14:00:00.75Z`, 1)), oif.Limits{})
	if err := EnrichRetained(r, e, extended, now); err != nil {
		t.Fatal(err)
	}
	if observation, err := ObserveRetainedExpiry(r, now.Add(time.Hour), true); err != nil || observation.RemoteTerminated {
		t.Fatal("old expiry survived TTL extension")
	}
}

func TestRetainedStorageAndContainerSnapshotsAreNotCharges(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct{ kind, body string }{{"vector_store", `{"usage_bytes":1024}`}, {"container", `{"memory_limit":"4g","status":"running"}`}} {
		r := &Resource{Kind: tc.kind, CreatedAt: now.Add(-48 * time.Hour), Metadata: json.RawMessage(`{"upstream_model":"m","memory_tier":"4g"}`)}
		document, _ := oif.ParseJSON([]byte(tc.body), oif.Limits{})
		e := retainedapi.Endpoint{Family: retainedapi.Family{Kind: r.Kind}, Action: "get"}
		if err := EnrichRetained(r, e, document, now); err != nil {
			t.Fatal(err)
		}
		if observation, err := ObserveRetained(r, e, document, now); err != nil || observation.CoverageComplete || len(observation.Lines) != 0 {
			t.Fatal("snapshot age became a charge")
		}
		deleted, _ := oif.ParseJSON([]byte(`{}`), oif.Limits{})
		e.Action = "delete"
		if observation, err := ObserveRetained(r, e, deleted, now); err != nil || !observation.RemoteTerminated || observation.CoverageComplete {
			t.Fatal("deletion lost unresolved usage liability")
		}
	}
}

func TestRetainedDeletionProofDoesNotInventAsyncTermination(t *testing.T) {
	source, _ := oif.ParseJSON([]byte(`{}`), oif.Limits{})
	for _, kind := range []string{"vector_store", "container", "context_cache", "fine_tuning_job", "eval_run"} {
		expected := kind == "vector_store" || kind == "container" || kind == "context_cache"
		observation, err := ObserveRetained(&Resource{Kind: kind}, retainedapi.Endpoint{Action: "delete"}, source, time.Now())
		if err != nil || observation.RemoteTerminated != expected || observation.CoverageComplete || len(observation.Lines) != 0 {
			t.Fatalf("delete %s invented usage/termination: %+v %v", kind, observation, err)
		}
	}
}

func TestRetainedNativeTerminalLifecycleNeverRegresses(t *testing.T) {
	now := time.Now().UTC()
	r := &Resource{Kind: "fine_tuning_job", State: "active", Metadata: json.RawMessage(`{"upstream_model":"m"}`)}
	terminal, _ := oif.ParseJSON([]byte(`{"status":"succeeded"}`), oif.Limits{})
	e := retainedapi.Endpoint{Action: "get"}
	if err := EnrichRetained(r, e, terminal, now); err != nil || r.State != "succeeded" {
		t.Fatalf("native terminal state lost: %s %v", r.State, err)
	}
	stale, _ := oif.ParseJSON([]byte(`{"status":"running"}`), oif.Limits{})
	if err := EnrichRetained(r, e, stale, now); err != nil || r.State != "succeeded" {
		t.Fatalf("terminal state reopened: %s %v", r.State, err)
	}
	r.State = StateDeleted
	if err := EnrichRetained(r, e, terminal, now); err != nil || r.State != StateDeleted {
		t.Fatal("native read resurrected tombstone")
	}
}

func TestRetainedCoarseTerminalClockUsesVerifiedCounterCoverage(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 100000000, time.UTC)
	r := &Resource{Kind: "fine_tuning_job", CreatedAt: now, State: "active", Metadata: json.RawMessage(`{"upstream_model":"m"}`)}
	source, _ := oif.ParseJSON([]byte(`{"status":"failed","created_at":1792800000,"finished_at":1792800000,"trained_tokens":0}`), oif.Limits{})
	e := retainedapi.Endpoint{Action: "get"}
	if err := EnrichRetained(r, e, source, now); err != nil {
		t.Fatal(err)
	}
	observation, err := ObserveRetained(r, e, source, now)
	if err != nil || !observation.RemoteTerminated || len(observation.Lines) != 1 || *observation.Lines[0].Usage.TrainingTokens != 0 || !observation.Lines[0].End.Equal(now) {
		t.Fatalf("coarse timestamp lost exact terminal usage: %+v %v", observation, err)
	}
}
