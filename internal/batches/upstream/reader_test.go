package upstream

import (
	"bytes"
	"os"
	"testing"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
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
		_, snapshot, err := decodeGeminiSnapshot(body)
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
