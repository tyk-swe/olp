package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/platform/egress"
)

const vendorFixtures = "../../../tests/fixtures/vendors/"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(vendorFixtures + name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

type fixtureStage int

const (
	running fixtureStage = iota
	finished
	finishedUncounted
)

func TestVendorBatchFixturesDecode(t *testing.T) {
	gemini := func(body []byte) (Snapshot, error) {
		snapshot, err := decodeGeminiSnapshot(body)
		return snapshot, err
	}
	cloud := func(adapter connectors.BatchAdapter) func([]byte) (Snapshot, error) {
		return func(body []byte) (Snapshot, error) {
			job, err := DecodeCloudJob(adapter, body)
			return job.snapshot(body), err
		}
	}
	managed := func(adapter connectors.BatchAdapter) func([]byte) (Snapshot, error) {
		return func(body []byte) (Snapshot, error) { return decodeManagedSnapshot(adapter, body) }
	}
	snapshots := []struct {
		fixture string
		decode  func([]byte) (Snapshot, error)
		stage   fixtureStage
	}{
		{"anthropic/batch.in_progress.json", decodeAnthropicBatch, running},
		{"anthropic/batch.ended.json", decodeAnthropicBatch, finished},
		{"gemini/batch.running.json", gemini, running},
		{"gemini/batch.succeeded.file.json", gemini, finished},
		{"gemini/batch.succeeded.inline.json", gemini, finished},
		{"vertex/job.running.json", cloud(connectors.BatchVertex), running},
		{"vertex/job.succeeded.json", cloud(connectors.BatchVertex), finished},
		{"bedrock/job.running.json", cloud(connectors.BatchBedrock), running},
		{"bedrock/job.succeeded.json", cloud(connectors.BatchBedrock), finished},
		{"together/batch.job.json", managed(connectors.BatchTogether), finishedUncounted},
		{"mistral/batch.job.json", managed(connectors.BatchMistral), finished},
	}
	for _, c := range snapshots {
		snapshot, err := c.decode(fixture(t, c.fixture))
		if err != nil {
			t.Errorf("%s: %v", c.fixture, err)
			continue
		}
		terminal := c.stage != running
		if snapshot.ID == "" || snapshot.Terminal != terminal || terminal && (snapshot.OutputFileID == "" || (snapshot.Expected != nil) != (c.stage == finished)) {
			t.Errorf("%s: id %q terminal %v output %q expected %v", c.fixture, snapshot.ID, snapshot.Terminal, snapshot.OutputFileID, snapshot.Expected)
		}
	}

	const model = "fixture-model"
	results := []struct {
		fixture string
		parse   LineParser
	}{
		{"anthropic/batch.results.jsonl", func(line []byte, _ int64) (batches.Result, error) { return parseAnthropicLine(line, model) }},
		{"gemini/batch.results.jsonl", func(line []byte, _ int64) (batches.Result, error) { return parseGeminiLine(line, model) }},
		{"vertex/predictions.jsonl", func(line []byte, number int64) (batches.Result, error) {
			return parseVertexLine(line, gcsObject{Name: "out/predictions.jsonl", Generation: "1"}, number, model)
		}},
		{"together/batch.output.jsonl", func(line []byte, _ int64) (batches.Result, error) {
			return ParseLine(line, "/v1/chat/completions", false)
		}},
		{"together/batch.error.jsonl", func(line []byte, _ int64) (batches.Result, error) {
			return ParseLine(line, "/v1/chat/completions", true)
		}},
		{"mistral/batch.output.jsonl", func(line []byte, _ int64) (batches.Result, error) {
			return ParseLine(line, "/v1/chat/completions", false)
		}},
		{"mistral/batch.error.jsonl", func(line []byte, _ int64) (batches.Result, error) {
			return ParseLine(line, "/v1/chat/completions", true)
		}},
	}
	for _, c := range results {
		seen := map[string]bool{}
		for i, line := range bytes.Split(bytes.TrimSpace(fixture(t, c.fixture)), []byte("\n")) {
			result, err := c.parse(line, int64(i+1))
			if err != nil {
				t.Errorf("%s line %d: %v", c.fixture, i+1, err)
				continue
			}
			if result.CustomID == "" || seen[result.CustomID] || !settles(result) {
				t.Errorf("%s line %d: %+v", c.fixture, i+1, result)
			}
			seen[result.CustomID] = true
		}
	}

	manifest, err := parseBedrockManifest(fixture(t, "bedrock/manifest.json.out"))
	if err != nil || manifest.Kind != batches.ResultAggregate || !manifest.Usage.Batch || manifest.Usage.InputTokens == nil {
		t.Errorf("bedrock manifest: %+v, err %v", manifest, err)
	}
}

func settles(result batches.Result) bool {
	switch result.Kind {
	case batches.ResultSucceeded:
		return result.Usage.Evidence.Observed() && result.Usage.InputTokens != nil && *result.Usage.InputTokens > 0
	case batches.ResultErrored, batches.ResultCancelled, batches.ResultExpired:
		return result.ErrorClass != ""
	default:
		return false
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestVertexReconcilePagination(t *testing.T) {
	t.Parallel()
	const label = "submission-1"
	const token = "next+/=&page"
	job := func(id, submission string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"name":"projects/test-project/locations/us-central1/batchPredictionJobs/%s","state":"JOB_STATE_RUNNING","labels":{"olp_submission":%q}}`, id, submission))
	}
	type page struct {
		Jobs []json.RawMessage `json:"batchPredictionJobs,omitempty"`
		Next string            `json:"nextPageToken,omitempty"`
	}
	match := job("job-1", label)
	other := job("other", "submission-2")
	capped := make([]page, maxListPages)
	for i := range capped {
		capped[i] = page{Next: fmt.Sprintf("page-%d", i+2)}
	}
	capped[0].Jobs = []json.RawMessage{match}
	tests := []struct {
		name  string
		pages []page
		ids   []string
		err   error
	}{
		{"match after empty page", []page{{Next: token}, {Jobs: []json.RawMessage{other, match}}}, []string{"job-1"}, nil},
		{"two matches across pages", []page{{Jobs: []json.RawMessage{match}, Next: token}, {Jobs: []json.RawMessage{job("job-2", label)}}}, []string{"job-1", "job-2"}, nil},
		{"exhausted without a match", []page{{Next: token}, {Jobs: []json.RawMessage{other}}}, nil, nil},
		{"stalled cursor", []page{{Next: token}, {Next: token}}, nil, errListCursor},
		{"page limit with a partial match", capped, nil, errListPageLimit},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			policy := &egress.Policy{}
			reader, _ := New(connectors.NewAuth(policy, func(string) string { return "" }), policy).For(connectors.BatchVertex)
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if calls >= len(tc.pages) {
					t.Fatalf("unexpected list request %d", calls+1)
				}
				query := r.URL.Query()
				wantToken := ""
				if calls > 0 {
					wantToken = tc.pages[calls-1].Next
				}
				if query.Get("pageToken") != wantToken || query.Has("pageToken") != (calls > 0) ||
					query.Get("filter") != `labels.olp_submission="`+label+`"` || query.Get("pageSize") != fmt.Sprint(listPageSize) {
					t.Fatalf("list request %d: unexpected query %s", calls+1, query.Encode())
				}
				body, err := json.Marshal(tc.pages[calls])
				if err != nil {
					t.Fatal(err)
				}
				calls++
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(body))}, nil
			})}
			config := connectors.Config{Kind: connectors.KindVertexAI, AuthMode: connectors.AuthNone, CloudProject: "test-project", CloudRegion: "us-central1"}
			settlement := batches.Settlement{Intent: batches.Intent{Submission: batches.VertexSubmission{Label: label}}}
			matches, err := reader.Reconcile(t.Context(), settlement, Target{Client: client, Config: config}, nil)
			var ids []string
			for _, match := range matches {
				ids = append(ids, match.ID)
			}
			if !errors.Is(err, tc.err) || !slices.Equal(ids, tc.ids) || calls != len(tc.pages) {
				t.Fatalf("got jobs %v, error %v after %d requests; want jobs %v, error %v after %d requests", ids, err, calls, tc.ids, tc.err, len(tc.pages))
			}
		})
	}
}
