package resources

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

func mustParseOIF(t *testing.T, raw string) oif.Document {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(raw), oif.Limits{MaxBytes: 1 << 20})
	if err != nil {
		t.Fatalf("parse oif document: %v", err)
	}
	return doc
}

func TestEnrichRetainedAtomicOnError(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	origExp := time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		resource Resource
		endpoint retainedapi.Endpoint
		body     string
		wantErr  error
	}{
		{
			name: "context cache missing expireTime leaves state and expires_at intact",
			resource: Resource{
				Kind:      "context_cache",
				State:     "active",
				ExpiresAt: &origExp,
				Metadata:  json.RawMessage(`{"upstream_model":"models/cache-model"}`),
			},
			body: `{"status":"expired","model":"models/cache-model","usageMetadata":{"totalTokenCount":120}}`,
		},
		{
			name: "context cache immutable token count change leaves state intact",
			resource: Resource{
				Kind:      "context_cache",
				State:     "active",
				ExpiresAt: &origExp,
				Metadata:  json.RawMessage(`{"upstream_model":"cache-model","total_token_count":100}`),
			},
			body: `{"status":"expired","model":"cache-model","usageMetadata":{"totalTokenCount":200},"expireTime":"2026-05-01T14:00:00Z"}`,
		},
		{
			name: "vector store invalid expires_at leaves state intact",
			resource: Resource{
				Kind:      "vector_store",
				State:     "in_progress",
				ExpiresAt: &origExp,
				Metadata:  json.RawMessage(`{}`),
			},
			body:    `{"status":"completed","expires_at":"not-a-timestamp"}`,
			wantErr: ErrContract,
		},
		{
			name: "vector store metadata limit failure leaves state and expires_at intact",
			resource: Resource{
				Kind:      "vector_store",
				State:     "in_progress",
				ExpiresAt: &origExp,
				Metadata:  json.RawMessage(`{"pad":"` + strings.Repeat("a", metadataLimit-12) + `"}`),
			},
			body:    `{"status":"expired","expires_at":null}`,
			wantErr: ErrMetadataTooLarge,
		},
		{
			name: "container invalid memory tier leaves state intact",
			resource: Resource{
				Kind:     "container",
				State:    "running",
				Metadata: json.RawMessage(`{}`),
			},
			body:    `{"status":"expired","memory_limit":"128g"}`,
			wantErr: ErrContract,
		},
		{
			name: "corrupt native_status metadata returns error without mutation",
			resource: Resource{
				Kind:     "fine_tuning_job",
				State:    "running",
				Metadata: json.RawMessage(`{"native_status":123}`),
			},
			body:    `{"status":"succeeded"}`,
			wantErr: ErrContract,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := tc.resource
			origState := r.State
			origMeta := append([]byte(nil), r.Metadata...)
			origExpiresPtr := r.ExpiresAt
			var origExpiresVal time.Time
			if origExpiresPtr != nil {
				origExpiresVal = *origExpiresPtr
			}

			err := EnrichRetained(&r, tc.endpoint, mustParseOIF(t, tc.body), now)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("got error %v, want %v", err, tc.wantErr)
			}
			if r.State != origState {
				t.Fatalf("State mutated on error: got %q, want %q", r.State, origState)
			}
			if !bytes.Equal(r.Metadata, origMeta) {
				t.Fatalf("Metadata mutated on error: got %s, want %s", r.Metadata, origMeta)
			}
			if r.ExpiresAt != origExpiresPtr || (origExpiresPtr != nil && !r.ExpiresAt.Equal(origExpiresVal)) {
				t.Fatalf("ExpiresAt mutated on error: got %v, want %v", r.ExpiresAt, origExpiresPtr)
			}
		})
	}
}

func TestEnrichRetainedInvariants(t *testing.T) {
	t.Parallel()

	firstAt := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	laterAt := time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC)

	t.Run("terminal status does not regress on stale poll", func(t *testing.T) {
		t.Parallel()
		r := Resource{Kind: "fine_tuning_job", State: "running", Metadata: json.RawMessage(`{}`)}
		if err := EnrichRetained(&r, retainedapi.Endpoint{}, mustParseOIF(t, `{"status":"succeeded","created_at":1777629000}`), firstAt); err != nil {
			t.Fatalf("first enrich failed: %v", err)
		}
		if r.State != "succeeded" {
			t.Fatalf("got state %q, want succeeded", r.State)
		}
		if err := EnrichRetained(&r, retainedapi.Endpoint{}, mustParseOIF(t, `{"status":"running","created_at":1777629000}`), laterAt); err != nil {
			t.Fatalf("second enrich failed: %v", err)
		}
		if r.State != "succeeded" {
			t.Fatalf("terminal state regressed to %q", r.State)
		}
		var meta map[string]any
		if err := json.Unmarshal(r.Metadata, &meta); err != nil {
			t.Fatalf("unmarshal meta: %v", err)
		}
		if meta["native_status"] != "succeeded" {
			t.Fatalf("native_status regressed: %v", meta["native_status"])
		}
		if meta["terminal_observed_at"] != firstAt.Format(time.RFC3339Nano) {
			t.Fatalf("terminal_observed_at changed: got %v, want %v", meta["terminal_observed_at"], firstAt.Format(time.RFC3339Nano))
		}
	})

	t.Run("immutable creation timestamp rejects modification", func(t *testing.T) {
		t.Parallel()
		r := Resource{Kind: "fine_tuning_job", State: "running", Metadata: json.RawMessage(`{"created_at":"2026-05-01T09:00:00Z"}`)}
		err := EnrichRetained(&r, retainedapi.Endpoint{}, mustParseOIF(t, `{"created_at":"2026-05-01T09:05:00Z"}`), firstAt)
		if err == nil {
			t.Fatalf("expected error on immutable created_at change")
		}
	})

	t.Run("context cache matches serving_model ignoring models prefix and skips delete meter", func(t *testing.T) {
		t.Parallel()
		r := Resource{
			Kind:     "context_cache",
			State:    "active",
			Metadata: json.RawMessage(`{"serving_model":"models/my-model"}`),
		}
		err := EnrichRetained(&r, retainedapi.Endpoint{Action: "get"}, mustParseOIF(t, `{"model":"my-model","usageMetadata":{"totalTokenCount":42},"expireTime":"2026-05-01T12:00:00Z"}`), firstAt)
		if err != nil {
			t.Fatalf("enrich context_cache: %v", err)
		}
		if r.ExpiresAt == nil || !r.ExpiresAt.Equal(time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)) {
			t.Fatalf("unexpected ExpiresAt: %v", r.ExpiresAt)
		}
		if err := EnrichRetained(&r, retainedapi.Endpoint{Action: "delete"}, mustParseOIF(t, `{}`), laterAt); err != nil {
			t.Fatalf("delete action should skip context_cache meter check: %v", err)
		}
	})

	t.Run("file captures metadata fields without overwriting state", func(t *testing.T) {
		t.Parallel()
		r := Resource{Kind: KindFile, State: "active", Metadata: json.RawMessage(`{}`)}
		err := EnrichRetained(&r, retainedapi.Endpoint{}, mustParseOIF(t, `{"object":"file","purpose":"fine-tune","filename":"data.jsonl","bytes":512,"status":"processed","created_at":1777629000}`), firstAt)
		if err != nil {
			t.Fatalf("enrich file: %v", err)
		}
		if r.State != "active" {
			t.Fatalf("file state should not change: got %q", r.State)
		}
		var meta map[string]any
		if err := json.Unmarshal(r.Metadata, &meta); err != nil {
			t.Fatalf("unmarshal meta: %v", err)
		}
		if meta["filename"] != "data.jsonl" || meta["bytes"] != float64(512) {
			t.Fatalf("unexpected file metadata: %v", meta)
		}
	})
}

func TestObserveRetained(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	t.Run("delete terminates storage cache and container but not fine_tuning_job", func(t *testing.T) {
		t.Parallel()
		for _, kind := range []string{"context_cache", "vector_store", "container"} {
			obs, err := ObserveRetained(&Resource{Kind: kind}, retainedapi.Endpoint{Action: "delete"}, mustParseOIF(t, `{}`), now)
			if err != nil || !obs.RemoteTerminated {
				t.Fatalf("kind %s delete: obs=%+v err=%v", kind, obs, err)
			}
		}
		obs, err := ObserveRetained(&Resource{Kind: "fine_tuning_job"}, retainedapi.Endpoint{Action: "delete"}, mustParseOIF(t, `{}`), now)
		if err != nil || obs.RemoteTerminated {
			t.Fatalf("fine_tuning_job delete should not terminate: obs=%+v err=%v", obs, err)
		}
	})

	t.Run("fine_tuning_job same-second create and finish falls back to terminal_observed_at", func(t *testing.T) {
		t.Parallel()
		r := &Resource{
			Kind: "fine_tuning_job",
			Metadata: json.RawMessage(`{
				"upstream_model":"base-model",
				"created_at":"2026-05-01T10:00:00Z",
				"finished_at":"2026-05-01T10:00:00Z",
				"terminal_observed_at":"2026-05-01T10:00:05Z"
			}`),
		}
		obs, err := ObserveRetained(r, retainedapi.Endpoint{}, mustParseOIF(t, `{"status":"succeeded","trained_tokens":7500}`), now)
		if err != nil {
			t.Fatalf("ObserveRetained failed: %v", err)
		}
		if !obs.RemoteTerminated || !obs.CoverageComplete || len(obs.Lines) != 1 {
			t.Fatalf("unexpected observation: %+v", obs)
		}
		line := obs.Lines[0]
		wantEnd := time.Date(2026, 5, 1, 10, 0, 5, 0, time.UTC)
		if !line.End.Equal(wantEnd) || !obs.ObservedAt.Equal(wantEnd) {
			t.Fatalf("got end=%v observed=%v, want %v", line.End, obs.ObservedAt, wantEnd)
		}
		if line.Usage.TrainingTokens == nil || *line.Usage.TrainingTokens != 7500 {
			t.Fatalf("unexpected training tokens: %+v", line.Usage)
		}
	})

	t.Run("fine_tuning_job missing trained_tokens leaves coverage incomplete", func(t *testing.T) {
		t.Parallel()
		r := &Resource{
			Kind:     "fine_tuning_job",
			Metadata: json.RawMessage(`{"upstream_model":"base-model","created_at":"2026-05-01T10:00:00Z","finished_at":"2026-05-01T10:10:00Z"}`),
		}
		obs, err := ObserveRetained(r, retainedapi.Endpoint{}, mustParseOIF(t, `{"status":"failed","trained_tokens":null}`), now)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !obs.RemoteTerminated || obs.CoverageComplete || len(obs.Lines) != 0 {
			t.Fatalf("expected terminated with incomplete coverage: %+v", obs)
		}
	})

	t.Run("vertex_tuning_job extracts totalBillableTokenCount", func(t *testing.T) {
		t.Parallel()
		r := &Resource{
			Kind:     "vertex_tuning_job",
			Metadata: json.RawMessage(`{"upstream_model":"vertex-base","create_time":"2026-05-01T10:00:00Z","finished_at":"2026-05-01T10:30:00Z"}`),
		}
		doc := mustParseOIF(t, `{"state":"JOB_STATE_SUCCEEDED","tuningDataStats":{"supervisedTuningDataStats":{"totalBillableTokenCount":"12345"}}}`)
		obs, err := ObserveRetained(r, retainedapi.Endpoint{}, doc, now)
		if err != nil {
			t.Fatalf("ObserveRetained vertex failed: %v", err)
		}
		if !obs.RemoteTerminated || !obs.CoverageComplete || len(obs.Lines) != 1 || *obs.Lines[0].Usage.TrainingTokens != 12345 {
			t.Fatalf("unexpected vertex observation: %+v", obs)
		}
	})

	t.Run("eval_run reverse-maps serving_models and requires full model coverage", func(t *testing.T) {
		t.Parallel()
		r := &Resource{
			Kind: "eval_run",
			Metadata: json.RawMessage(`{
				"upstream_model":"logical-a",
				"authorized_models":["logical-a","logical-b"],
				"serving_models":{"logical-a":"native-a","logical-b":"native-b"},
				"created_at":"2026-05-01T10:00:00Z",
				"terminal_observed_at":"2026-05-01T10:05:00Z"
			}`),
		}
		partialDoc := mustParseOIF(t, `{
			"status":"completed",
			"per_model_usage":[
				{"model_name":"native-a","prompt_tokens":100,"completion_tokens":40,"cached_tokens":20,"total_tokens":140,"invocation_count":2}
			]
		}`)
		partial, err := ObserveRetained(r, retainedapi.Endpoint{}, partialDoc, now)
		if err != nil {
			t.Fatalf("partial eval observe failed: %v", err)
		}
		if !partial.RemoteTerminated || partial.CoverageComplete || len(partial.Lines) != 1 || partial.Lines[0].Model != "logical-a" {
			t.Fatalf("unexpected partial eval observation: %+v", partial)
		}

		fullDoc := mustParseOIF(t, `{
			"status":"completed",
			"per_model_usage":[
				{"model_name":"native-a","prompt_tokens":100,"completion_tokens":40,"cached_tokens":20,"total_tokens":140,"invocation_count":2},
				{"model_name":"native-b","prompt_tokens":0,"completion_tokens":0,"cached_tokens":0,"total_tokens":0,"invocation_count":0}
			]
		}`)
		full, err := ObserveRetained(r, retainedapi.Endpoint{}, fullDoc, now)
		if err != nil {
			t.Fatalf("full eval observe failed: %v", err)
		}
		if !full.CoverageComplete || len(full.Lines) != 2 {
			t.Fatalf("expected complete coverage with 2 lines: %+v", full)
		}
	})

	t.Run("eval_run rejects ambiguous native model binding and inconsistent counts", func(t *testing.T) {
		t.Parallel()
		ambiguous := &Resource{
			Kind: "eval_run",
			Metadata: json.RawMessage(`{
				"upstream_model":"logical-a",
				"authorized_models":["logical-a","logical-b"],
				"serving_models":{"logical-a":"shared-native","logical-b":"shared-native"},
				"created_at":"2026-05-01T10:00:00Z",
				"terminal_observed_at":"2026-05-01T10:05:00Z"
			}`),
		}
		doc := mustParseOIF(t, `{"status":"completed","per_model_usage":[{"model_name":"shared-native","prompt_tokens":10,"completion_tokens":5,"cached_tokens":0,"total_tokens":15,"invocation_count":1}]}`)
		if _, err := ObserveRetained(ambiguous, retainedapi.Endpoint{}, doc, now); err == nil {
			t.Fatalf("expected ambiguous native model error")
		}

		badTotals := &Resource{
			Kind: "eval_run",
			Metadata: json.RawMessage(`{
				"upstream_model":"logical-a",
				"authorized_models":["logical-a"],
				"created_at":"2026-05-01T10:00:00Z",
				"terminal_observed_at":"2026-05-01T10:05:00Z"
			}`),
		}
		badDoc := mustParseOIF(t, `{"status":"completed","per_model_usage":[{"model_name":"logical-a","prompt_tokens":10,"completion_tokens":5,"cached_tokens":11,"total_tokens":15,"invocation_count":1}]}`)
		if _, err := ObserveRetained(badTotals, retainedapi.Endpoint{}, badDoc, now); !errors.Is(err, ErrContract) {
			t.Fatalf("got %v, want ErrContract for cached > prompt", err)
		}
	})
}

func TestObserveRetainedExpiry(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	end := start.Add(125 * time.Millisecond)

	t.Run("exact fractional token-seconds and zero tokens", func(t *testing.T) {
		t.Parallel()
		r := &Resource{
			Kind:      "context_cache",
			ExpiresAt: &end,
			Metadata: json.RawMessage(`{
				"upstream_model":"cache-m",
				"total_token_count":3,
				"create_time":"` + start.Format(time.RFC3339Nano) + `",
				"expires_at":"` + end.Format(time.RFC3339Nano) + `"
			}`),
		}
		obs, err := ObserveRetainedExpiry(r, end.Add(time.Second), true)
		if err != nil {
			t.Fatalf("ObserveRetainedExpiry failed: %v", err)
		}
		if !obs.RemoteTerminated || !obs.CoverageComplete || len(obs.Lines) != 1 || !obs.ObservedAt.Equal(end) {
			t.Fatalf("unexpected expiry obs: %+v", obs)
		}
		if got := *obs.Lines[0].Usage.CacheTokenSeconds; got != "0.375" {
			t.Fatalf("got CacheTokenSeconds %q, want 0.375", got)
		}

		zeroRes := &Resource{
			Kind:      "context_cache",
			ExpiresAt: &end,
			Metadata: json.RawMessage(`{
				"upstream_model":"cache-m",
				"total_token_count":0,
				"create_time":"` + start.Format(time.RFC3339Nano) + `",
				"expires_at":"` + end.Format(time.RFC3339Nano) + `"
			}`),
		}
		zeroObs, err := ObserveRetainedExpiry(zeroRes, end, true)
		if err != nil || *zeroObs.Lines[0].Usage.CacheTokenSeconds != "0" {
			t.Fatalf("zero token cache expiry: obs=%+v err=%v", zeroObs, err)
		}
	})

	t.Run("does not terminate before expiry or without 404", func(t *testing.T) {
		t.Parallel()
		r := &Resource{Kind: "context_cache", ExpiresAt: &end, Metadata: json.RawMessage(`{}`)}
		if obs, err := ObserveRetainedExpiry(r, end.Add(-time.Millisecond), true); err != nil || obs.RemoteTerminated {
			t.Fatalf("before expiry should not terminate: obs=%+v err=%v", obs, err)
		}
		if obs, err := ObserveRetainedExpiry(r, end.Add(time.Second), false); err != nil || obs.RemoteTerminated {
			t.Fatalf("without 404 should not terminate: obs=%+v err=%v", obs, err)
		}
	})

	t.Run("rejects mismatched metadata expires_at", func(t *testing.T) {
		t.Parallel()
		other := end.Add(time.Minute)
		r := &Resource{
			Kind:      "context_cache",
			ExpiresAt: &end,
			Metadata: json.RawMessage(`{
				"upstream_model":"cache-m",
				"total_token_count":10,
				"create_time":"` + start.Format(time.RFC3339Nano) + `",
				"expires_at":"` + other.Format(time.RFC3339Nano) + `"
			}`),
		}
		if _, err := ObserveRetainedExpiry(r, other.Add(time.Second), true); !errors.Is(err, ErrContract) {
			t.Fatalf("got %v, want ErrContract", err)
		}
	})
}

func TestObserveRetainedJobTokenCounts(t *testing.T) {
	t.Parallel()
	job := &Resource{
		Kind: "fine_tuning_job",
		Metadata: json.RawMessage(`{
			"upstream_model":"base-model",
			"created_at":"2026-05-01T10:00:00Z",
			"terminal_observed_at":"2026-05-01T10:05:00Z"
		}`),
	}
	for name, tc := range map[string]struct{ count, want string }{
		"number":              {`7500`, "7500"},
		"numeric string":      {`"7500"`, "7500"},
		"above double range":  {`9007199254740993`, "9007199254740993"},
		"zero":                {`0`, "0"},
		"null is unobserved":  {`null`, "unobserved"},
		"negative":            {`-1`, "rejected"},
		"negative string":     {`"-1"`, "rejected"},
		"fraction":            {`7500.0`, "rejected"},
		"exponent":            {`75e2`, "rejected"},
		"beyond signed range": {`9223372036854775808`, "rejected"},
		"boolean":             {`true`, "rejected"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			body := mustParseOIF(t, `{"status":"succeeded","trained_tokens":`+tc.count+`}`)
			observed, err := ObserveRetained(job, retainedapi.Endpoint{}, body, time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC))
			if got := describeTraining(observed, err); got != tc.want {
				t.Fatalf("trained tokens = %s, want %s, %v", got, tc.want, err)
			}
		})
	}
}

func describeTraining(observed RetainedObservation, err error) string {
	if err != nil {
		return "rejected"
	}
	if len(observed.Lines) == 0 {
		return "unobserved"
	}
	return strconv.FormatInt(*observed.Lines[0].Usage.TrainingTokens, 10)
}
