package batches

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestBatchLineUsageParsing(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
	}{
		{name: "chat.jsonl", endpoint: "/v1/chat/completions"},
		{name: "responses.jsonl", endpoint: "/v1/responses"},
		{name: "completions.jsonl", endpoint: "/v1/completions"},
		{name: "embeddings.jsonl", endpoint: "/v1/embeddings"},
	}
	for _, test := range tests {
		line := bytes.TrimSpace(readFixture(t, test.name))
		result, err := ParseLine(line, test.endpoint, false)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if result.Kind != ResultSucceeded || !result.Usage.Observed || !result.Usage.Complete {
			t.Fatalf("%s result: %+v", test.name, result)
		}
		if len(result.Evidence) == 0 || !jsonValid(result.Evidence) {
			t.Fatalf("%s evidence is not canonical JSON", test.name)
		}
		switch test.name {
		case "chat.jsonl":
			if got := result.Usage.InputTokens; got == nil || *got != 4 {
				t.Fatalf("chat input tokens = %v", got)
			}
			if got := result.Usage.OutputTokens; got == nil || *got != 6 {
				t.Fatalf("chat output tokens = %v", got)
			}
			if got := result.Usage.CachedInputTokens; got == nil || *got != 2 {
				t.Fatalf("chat cached input tokens = %v", got)
			}
		case "responses.jsonl":
			if got := result.Usage.InputTokens; got == nil || *got != 8 {
				t.Fatalf("responses input tokens = %v", got)
			}
			if got := result.Usage.OutputTokens; got == nil || *got != 3 {
				t.Fatalf("responses output tokens = %v", got)
			}
			if got := result.Usage.CachedInputTokens; got == nil || *got != 2 {
				t.Fatalf("responses cached input tokens = %v", got)
			}
		case "completions.jsonl":
			if got := result.Usage.InputTokens; got == nil || *got != 5 {
				t.Fatalf("completion input tokens = %v", got)
			}
			if got := result.Usage.OutputTokens; got == nil || *got != 2 {
				t.Fatalf("completion output tokens = %v", got)
			}
		case "embeddings.jsonl":
			if got := result.Usage.InputTokens; got == nil || *got != 2 {
				t.Fatalf("embedding input tokens = %v", got)
			}
			if result.Usage.OutputTokens != nil {
				t.Fatalf("embedding output tokens = %v, want absent", *result.Usage.OutputTokens)
			}
		}
	}
}

func TestBatchLineErrorsAndMissingUsage(t *testing.T) {
	lines := bytes.Split(bytes.TrimSpace(readFixture(t, "errors.jsonl")), []byte{'\n'})
	want := []ResultKind{ResultExpired, ResultCancelled, ResultErrored}
	for index, line := range lines {
		result, err := ParseLine(line, "/v1/chat/completions", true)
		if err != nil {
			t.Fatal(err)
		}
		if result.Kind != want[index] || result.ErrorClass == "" {
			t.Fatalf("error line %d: %+v", index, result)
		}
	}
	result, err := ParseLine(bytes.TrimSpace(readFixture(t, "missing_usage.jsonl")), "/v1/chat/completions", false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Kind != ResultSucceeded || result.Usage.Observed || !result.Usage.BillingUncertain {
		t.Fatalf("missing usage was treated as zero usage: %+v", result)
	}
}

func TestBatchCloseDecision(t *testing.T) {
	expected := int64(4)
	tests := []struct {
		name     string
		counts   ResultCounts
		conflict bool
		terminal bool
		unknown  bool
		now      time.Time
		want     Decision
	}{
		{name: "open", counts: ResultCounts{ResultSucceeded: 2}, want: Decision{State: StateActive}},
		{name: "all observed", counts: ResultCounts{ResultSucceeded: 2, ResultErrored: 2}, want: Decision{State: StateSettled}},
		{name: "terminal gap", counts: ResultCounts{ResultSucceeded: 1}, terminal: true, want: Decision{State: StateUncertain, Gap: 3}},
		{name: "unknown expected", counts: ResultCounts{ResultSucceeded: 2}, terminal: true, unknown: true, want: Decision{State: StateUncertain, Gap: 1}},
		{name: "conflict remains open", counts: ResultCounts{ResultSucceeded: 1}, conflict: true, want: Decision{State: StateConflict}},
		{name: "conflict closes uncertain", counts: ResultCounts{ResultSucceeded: 1}, conflict: true, now: time.Unix(200, 0), want: Decision{State: StateUncertain, Gap: 3}},
		{name: "close after gap", counts: ResultCounts{ResultSucceeded: 1}, now: time.Unix(200, 0), want: Decision{State: StateUncertain, Gap: 3}},
	}
	now := time.Unix(100, 0)
	closeAfter := time.Unix(150, 0)
	for _, test := range tests {
		at := test.now
		if at.IsZero() {
			at = now
		}
		expectedCount := &expected
		if test.unknown {
			expectedCount = nil
		}
		got := CloseDecision(expectedCount, test.counts, test.conflict, test.terminal, at, closeAfter)
		if got != test.want {
			t.Errorf("%s decision = %+v, want %+v", test.name, got, test.want)
		}
	}
}

func TestBatchResultEventIDsAndValidation(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	keyID, providerID, runtimeID, createID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	model, route, revision := "provider-model", "batch-route", "provider-revision"
	template := &usage.Event{
		Version: usage.WireVersion, EventID: createID, RequestID: createID,
		RuntimeGenerationID: runtimeID, APIKeyID: keyID, Origin: usage.OriginBatchSettlement,
		ParentRequestID: &createID, RouteSlug: route, Operation: "generation", Surface: "openai",
		RequestStartedAt: now, RequestCompletedAt: now, ObservedAt: now,
		ProviderID: &providerID, UpstreamModel: &model, Committed: true,
		Attempts: []usage.Attempt{{
			ID: uuid.NewString(), Ordinal: 1, ProviderID: providerID, UpstreamModel: model,
			StartedAt: now, CompletedAt: now, Committed: true,
			Usage:   &usage.AttemptUsage{Observed: false, BillingUncertain: true},
			Routing: &usage.Routing{ProviderRevisionID: revision},
		}},
	}
	resourceID := uuid.New()
	digest := bytes.Repeat([]byte{0x7a}, 32)
	status := 200
	input, output := int64(8), int64(5)
	result := Result{CustomID: "line-1", Kind: ResultSucceeded, StatusCode: &status,
		Usage: usage.AttemptUsage{Observed: true, Complete: true, InputTokens: &input, OutputTokens: &output}}
	first, err := ResultEvent(template, resourceID, digest, result, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResultEvent(template, resourceID, digest, result, now.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if first.EventID != first.RequestID || first.EventID != second.EventID ||
		first.Attempts[0].ID != second.Attempts[0].ID || first.ParentRequestID == nil ||
		*first.ParentRequestID != createID || !first.Batch || !first.Attempts[0].Usage.Batch {
		t.Fatalf("batch event identity or provenance is invalid: %+v", first)
	}
	if !second.Attempts[0].StartedAt.Equal(now) || !second.ObservedAt.Equal(now.Add(24*time.Hour)) {
		t.Fatal("delayed settlement changed the submission pricing timestamp")
	}
	if _, err = usage.Validate(first); err != nil {
		t.Fatalf("batch result event failed usage validation: %v", err)
	}
	gap, err := ResultEvent(template, resourceID, digest, Result{
		CustomID: "*", Kind: ResultGap,
		Usage: usage.AttemptUsage{Observed: false, BillingUncertain: true},
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if gap.Attempts[0].Usage.Observed || !gap.Attempts[0].Usage.BillingUncertain ||
		!gap.Batch || !gap.Attempts[0].Usage.Batch {
		t.Fatalf("gap event did not preserve uncertain batch usage: %+v", gap)
	}
	for _, uncertain := range []bool{false, true} {
		aggregateTemplate := *template
		aggregateTemplate.Attempts = append([]usage.Attempt(nil), template.Attempts...)
		routingInfo := *template.Attempts[0].Routing
		policy := json.RawMessage(`{"batch_aggregate_pricing_uncertain":false}`)
		if uncertain {
			policy = json.RawMessage(`{"batch_aggregate_pricing_uncertain":true}`)
		}
		routingInfo.Policy = &policy
		aggregateTemplate.Attempts[0].Routing = &routingInfo
		aggregate := result
		aggregate.Kind = ResultAggregate
		event, err := ResultEvent(&aggregateTemplate, resourceID, digest, aggregate, now)
		if err != nil || event.UsageComplete == uncertain || event.Attempts[0].Usage.BillingUncertain != uncertain {
			t.Fatalf("aggregate tariff uncertainty was not retained: event=%+v err=%v", event, err)
		}
	}

	for _, fixture := range []struct {
		name  string
		body  []byte
		parse ResultLineParser
	}{
		{"anthropic", anthropicFixture(t, "batch.results.jsonl"), func(line []byte) (Result, error) { return parseAnthropicResultLine(line, model) }},
		{"gemini", geminiBatchFixture(t, "batch.results.jsonl"), func(line []byte) (Result, error) { return parseGeminiResult(line, model) }},
		{"vertex", cloudBatchFixture(t, "vertex", "predictions.jsonl"), func(line []byte) (Result, error) { return parseVertexLine(line, "object", "1", 1, model) }},
		{"bedrock", cloudBatchFixture(t, "bedrock", "manifest.json.out"), parseBedrockManifest},
		{"vertex_private_status", []byte(`{"status":"private-prompt-marker","response":{}}`), func(line []byte) (Result, error) { return parseVertexLine(line, "object", "1", 1, model) }},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			for _, raw := range bytes.Split(bytes.TrimSpace(fixture.body), []byte{'\n'}) {
				result, err := fixture.parse(raw)
				if err != nil {
					t.Fatal(err)
				}
				event, err := ResultEvent(template, resourceID, digest, result, now)
				if err != nil {
					t.Fatalf("native result cannot be persisted: %+v err=%v", result, err)
				}
				encoded, encodeErr := usage.Encode(event)
				if encodeErr != nil || bytes.Contains(encoded, []byte("private-prompt-marker")) {
					t.Fatalf("provider status content leaked into ordinary accounting: err=%v", encodeErr)
				}
				if result.Kind == ResultSucceeded || result.Kind == ResultAggregate {
					if event.StatusCode == nil || *event.StatusCode != 200 || event.ErrorClass != nil || event.Attempts[0].ErrorClass != nil {
						t.Fatalf("native success has invalid accounting outcome: %+v", event)
					}
				} else if event.Attempts[0].Usage.Observed || !event.Attempts[0].Usage.Complete {
					t.Fatalf("native error has invalid non-billable evidence: %+v", event.Attempts[0].Usage)
				}
			}
		})
	}
}

func TestBatchCursorPrefixDigestSkipAndMismatch(t *testing.T) {
	body := readFixture(t, "cursor.jsonl")
	firstEnd := bytes.IndexByte(body, '\n') + 1
	prefix := sha256.Sum256(body[:firstEnd])
	cursor := Cursor{Phase: "output", File: "file-1", Line: 1, PrefixSHA256: hex.EncodeToString(prefix[:])}
	var got []Result
	next, read, _, err := ReadJSONLPages(bytes.NewReader(body), cursor, "file-1", "output",
		func(line []byte) (Result, error) {
			return Result{CustomID: string(line), Kind: ResultSucceeded, Evidence: append([]byte(nil), line...)}, nil
		},
		func(results []Result, _ Cursor) error { got = append(got, results...); return nil })
	if err != nil || read != int64(len(body)) || next.Line != 2 || len(got) != 1 || !bytes.Contains([]byte(got[0].CustomID), []byte("cursor-2")) {
		t.Fatalf("cursor resume: next=%+v read=%d got=%+v err=%v", next, read, got, err)
	}
	cursor.PrefixSHA256 = strings.Repeat("0", 64)
	_, _, _, err = ReadJSONLPages(bytes.NewReader(body), cursor, "file-1", "output",
		func(line []byte) (Result, error) { return Result{}, nil },
		func([]Result, Cursor) error { return nil })
	if !errors.Is(err, ErrCursorConflict) {
		t.Fatalf("changed prefix error = %v, want %v", err, ErrCursorConflict)
	}
}

func TestBatchReconciliationMatchDecision(t *testing.T) {
	after := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	candidates := []BatchSnapshot{
		{ID: "old", InputFileID: "owned-file", CreatedAt: after.Add(-6 * time.Minute)},
		{ID: "other-input", InputFileID: "other-file", CreatedAt: after},
		{ID: "one", InputFileID: "owned-file", CreatedAt: after},
		{ID: "two", InputFileID: "owned-file", CreatedAt: after.Add(time.Second)},
	}
	if matches := MatchSubmission(candidates[:2], "owned-file", after); len(matches) != 0 {
		t.Fatalf("unmatched submission: %+v", matches)
	}
	if matches := MatchSubmission(candidates[:3], "owned-file", after); len(matches) != 1 || matches[0].ID != "one" {
		t.Fatalf("single submission match: %+v", matches)
	}
	if matches := MatchSubmission(candidates, "owned-file", after); len(matches) != 2 {
		t.Fatalf("ambiguous submission matches: %+v", matches)
	}
}

func TestManagedBatchSnapshotsAndLists(t *testing.T) {
	together, err := parseManagedSnapshot([]byte(`{"job":{"id":"t-job","status":"IN_PROGRESS","endpoint":"/v1/chat/completions","input_file_id":"t-file","created_at":"2026-10-20T12:00:00Z","output_file_id":"t-out"}}`), "together")
	if err != nil || together.Status != "in_progress" || together.InputFileID != "t-file" || together.OutputFileID != "t-out" {
		t.Fatalf("Together snapshot = %+v, %v", together, err)
	}
	mistral, err := parseManagedSnapshot([]byte(`{"id":"m-job","status":"SUCCESS","endpoint":"/v1/embeddings","input_files":["m-file"],"created_at":1792497600,"total_requests":"5"}`), "mistral")
	if err != nil || mistral.Status != "completed" || mistral.InputFileID != "m-file" || mistral.Expected == nil || *mistral.Expected != 5 {
		t.Fatalf("Mistral snapshot = %+v, %v", mistral, err)
	}
	if _, err = parseManagedSnapshot([]byte(`{"id":"m-job","status":"UNKNOWN","endpoint":"/v1/chat/completions"}`), "mistral"); err == nil {
		t.Fatalf("unknown Mistral status error = %v", err)
	}
	items, more, err := managedBatchList([]byte(`[{"id":"t-job","status":"CANCELLED","endpoint":"/v1/chat/completions","input_file_id":"t-file","created_at":"2026-10-20T12:00:00Z"}]`), "together", 0)
	if err != nil || more || len(items) != 1 || items[0].Status != "cancelled" {
		t.Fatalf("Together batch list = %+v, %v, %v", items, more, err)
	}
	for adapter, statuses := range managedBatchStatuses {
		for upstreamStatus, projectedStatus := range statuses {
			job := map[string]any{"id": "status-job", "status": upstreamStatus, "endpoint": "/v1/chat/completions", "created_at": 1792497600}
			if adapter == "together" {
				job["input_file_id"] = "input"
			} else {
				job["input_files"] = []string{"input"}
			}
			body, _ := json.Marshal(job)
			snapshot, err := parseManagedSnapshot(body, adapter)
			if err != nil || snapshot.Status != projectedStatus {
				t.Fatalf("%s %s => %s, %v", adapter, upstreamStatus, snapshot.Status, err)
			}
		}
	}
}

func TestBatchLineDigestUsesItsPurpose(t *testing.T) {
	auth := secrets.NewAuthKey(bytes.Repeat([]byte{0x5a}, 32), uuid.NewString())
	got := CustomIDDigest(auth, "custom-line")
	want := auth.Digest(secrets.BatchLineDigest, "custom-line")
	if !bytes.Equal(got, want) {
		t.Fatalf("custom ID digest = %x, want %x", got, want)
	}
}

func jsonValid(body []byte) bool {
	var value any
	return json.Unmarshal(body, &value) == nil
}

func TestBatchEmptyJobWaitsForTerminal(t *testing.T) {
	now := time.Unix(100, 0)
	expected := int64(0)
	for _, terminal := range []bool{false, true} {
		want := StateActive
		if terminal {
			want = StateSettled
		}
		if got := CloseDecision(&expected, ResultCounts{}, false, terminal, now, now.Add(time.Hour)); got.State != want {
			t.Fatalf("terminal=%v decision=%+v want=%s", terminal, got, want)
		}
	}
}

func TestBatchCheckpointRetainsSubmissionAndOutputEvidence(t *testing.T) {
	previous := []byte(`{"submission":{"input_file_id":"upstream","local_input_file_id":"local","endpoint":"/v1/chat/completions"},"phase":"output","line":100}`)
	next := Cursor{Phase: "error", File: "errors", OutputPrefixSHA256: "output-digest"}
	encoded, err := checkpointCursor(previous, next)
	if err != nil {
		t.Fatal(err)
	}
	input, local, endpoint, err := submissionFiles(encoded)
	var progress Cursor
	if err != nil || input != "upstream" || local != "local" || endpoint != "/v1/chat/completions" || json.Unmarshal(encoded, &progress) != nil || progress.Line != 0 {
		t.Fatalf("checkpoint lost submission identity or retained stale progress: %s err=%v", encoded, err)
	}
	lines := readFixture(t, "cursor.jsonl")
	var committed Cursor
	_, _, _, err = ReadJSONLPages(bytes.NewReader(lines), progress, "errors", "error",
		func(line []byte) (Result, error) { return ParseLine(line, endpoint, false) },
		func(_ []Result, cursor Cursor) error { committed = cursor; return nil })
	if err != nil || committed.OutputPrefixSHA256 != "output-digest" {
		t.Fatalf("error checkpoint lost output evidence: %+v err=%v", committed, err)
	}
}

func TestBatchResultPagesBoundBytesBeforeCount(t *testing.T) {
	line := strings.Repeat("x", MaxPageBytes/20+17) + "\n"
	commits, total := 0, 0
	_, _, _, err := ReadJSONLPages(strings.NewReader(strings.Repeat(line, 40)), Cursor{}, "output", "output",
		func(raw []byte) (Result, error) { return Result{CustomID: "line", Evidence: raw}, nil },
		func(page []Result, _ Cursor) error { commits++; total += len(page); return nil })
	if err != nil || commits < 2 || total != 40 {
		t.Fatalf("large result pages were bounded only by count: commits=%d total=%d err=%v", commits, total, err)
	}
}
