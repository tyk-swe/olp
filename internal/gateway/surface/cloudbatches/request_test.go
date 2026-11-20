package cloudbatches

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/batches/upstream"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/routing"
)

const role = "arn:aws:iam::123456789012:role/batch"

func storage() *connectors.BatchStorage {
	return &connectors.BatchStorage{InputURIPrefix: "s3://jobs/in/", OutputURIPrefix: "s3://jobs/out/", RoleARN: role}
}

func TestRequestsStayInsideTheProviderStoragePrefixesAndKnownFields(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		body    string
		adapter connectors.BatchAdapter
		storage *connectors.BatchStorage
		code    string
		param   string
	}{
		"accepted":                 {body: `{"input_uri":"s3://jobs/in/a.jsonl","output_uri_prefix":"s3://jobs/out/run/","display_name":"nightly_1","timeout_hours":24}`, adapter: connectors.BatchBedrock, storage: storage()},
		"input outside the prefix": {body: `{"input_uri":"s3://jobs/other/a.jsonl","output_uri_prefix":"s3://jobs/out/run/"}`, adapter: connectors.BatchBedrock, storage: storage(), code: "storage_scope"},
		"input in another bucket":  {body: `{"input_uri":"s3://theirs/in/a.jsonl","output_uri_prefix":"s3://jobs/out/run/"}`, adapter: connectors.BatchBedrock, storage: storage(), code: "storage_scope"},
		"output outside prefix":    {body: `{"input_uri":"s3://jobs/in/a.jsonl","output_uri_prefix":"s3://jobs/in/run/"}`, adapter: connectors.BatchBedrock, storage: storage(), code: "storage_scope"},
		"provider without storage": {body: `{"input_uri":"s3://jobs/in/a.jsonl","output_uri_prefix":"s3://jobs/out/run/"}`, adapter: connectors.BatchBedrock, code: "storage_scope"},
		"unknown field":            {body: `{"input_uri":"s3://jobs/in/a.jsonl","output_uri_prefix":"s3://jobs/out/run/","role_arn":"x"}`, adapter: connectors.BatchBedrock, storage: storage(), code: "unsupported_parameter", param: "role_arn"},
		"missing input":            {body: `{"output_uri_prefix":"s3://jobs/out/run/"}`, adapter: connectors.BatchBedrock, storage: storage(), code: "missing_required_parameter", param: "input_uri"},
		"missing output":           {body: `{"input_uri":"s3://jobs/in/a.jsonl"}`, adapter: connectors.BatchBedrock, storage: storage(), code: "missing_required_parameter", param: "output_uri_prefix"},
		"display name with a path": {body: `{"input_uri":"s3://jobs/in/a.jsonl","output_uri_prefix":"s3://jobs/out/run/","display_name":"a/b"}`, adapter: connectors.BatchBedrock, storage: storage(), code: "invalid_value", param: "display_name"},
		"timeout below the window": {body: `{"input_uri":"s3://jobs/in/a.jsonl","output_uri_prefix":"s3://jobs/out/run/","timeout_hours":23}`, adapter: connectors.BatchBedrock, storage: storage(), code: "invalid_value", param: "timeout_hours"},
		"timeout as text":          {body: `{"input_uri":"s3://jobs/in/a.jsonl","output_uri_prefix":"s3://jobs/out/run/","timeout_hours":"24"}`, adapter: connectors.BatchBedrock, storage: storage(), code: "invalid_value", param: "timeout_hours"},
		"timeout on Vertex":        {body: `{"input_uri":"s3://jobs/in/a.jsonl","output_uri_prefix":"s3://jobs/out/run/","timeout_hours":24}`, adapter: connectors.BatchVertex, storage: storage(), code: "unsupported_parameter", param: "timeout_hours"},
		"not an object":            {body: `[]`, adapter: connectors.BatchBedrock, storage: storage(), code: "invalid_json"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input, e := validate([]byte(c.body), c.adapter, c.storage)
			if c.code == "" {
				want := request{InputURI: "s3://jobs/in/a.jsonl", OutputURIPrefix: "s3://jobs/out/run/", DisplayName: "nightly_1", TimeoutHours: 24}
				if e != nil || input != want {
					t.Fatalf("validate = %+v, %v, want %+v", input, e, want)
				}
				return
			}
			if e == nil || e.Code != c.code {
				t.Fatalf("validate = %+v, %v, want code %s", input, e, c.code)
			}
			if c.param != "" && (e.Param == nil || *e.Param != c.param) {
				t.Fatalf("param = %v, want %s", e.Param, c.param)
			}
		})
	}
}

func decoded[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return value
}

func TestBedrockPlanNamesTheRoleModelAndOneIdempotencyTokenForTheJob(t *testing.T) {
	t.Parallel()
	pin := &kernel.Pin{Model: "provider-model", Provider: routing.Provider{Kind: connectors.KindBedrock, CloudRegion: "us-east-1", BatchStorage: storage()}}
	input := request{InputURI: "s3://jobs/in/a.jsonl", OutputURIPrefix: "s3://jobs/out/run/", DisplayName: "nightly", TimeoutHours: 48}
	plan, err := planJob(pin, connectors.BatchBedrock, input)
	if err != nil {
		t.Fatal(err)
	}
	job := decoded[bedrockJob](t, plan.body)
	if job.JobName != "nightly" || job.RoleARN != role || job.ModelID != "provider-model" || job.TimeoutHours != 48 ||
		job.InputDataConfig.S3InputDataConfig.S3URI != input.InputURI || job.OutputDataConfig.S3OutputDataConfig.S3URI != input.OutputURIPrefix {
		t.Fatalf("job = %s", plan.body)
	}
	submission, ok := plan.submission.(batches.BedrockSubmission)
	if !ok || job.ClientRequestToken != plan.id.String() || submission.ClientRequestToken != plan.id.String() || string(submission.CreateBody) != string(plan.body) {
		t.Fatalf("token %q, plan id %s, submission %+v", job.ClientRequestToken, plan.id, plan.submission)
	}
	if plan.window.Hours() != 48 || !strings.HasSuffix(plan.endpoint, "/model-invocation-job") {
		t.Fatalf("window %s, endpoint %s", plan.window, plan.endpoint)
	}
}

func TestVertexPlanLabelsTheJobWithItsSubmissionAndDefaultsTheName(t *testing.T) {
	t.Parallel()
	pin := &kernel.Pin{Model: "models/provider-model", Provider: routing.Provider{Kind: connectors.KindVertexAI, CloudRegion: "us-central1", CloudProject: "project-1"}}
	input := request{InputURI: "gs://jobs/in/a.jsonl", OutputURIPrefix: "gs://jobs/out/run/"}
	plan, err := planJob(pin, connectors.BatchVertex, input)
	if err != nil {
		t.Fatal(err)
	}
	job := decoded[vertexJob](t, plan.body)
	label := strings.ReplaceAll(plan.id.String(), "-", "")
	if job.DisplayName != "olp-"+label || job.Labels["olp_submission"] != label || plan.submission != (batches.VertexSubmission{Label: label}) {
		t.Fatalf("job = %s, submission %+v, want label %s", plan.body, plan.submission, label)
	}
	if job.Model != "publishers/google/models/provider-model" || len(job.InputConfig.GCSSource.URIs) != 1 || job.InputConfig.GCSSource.URIs[0] != input.InputURI ||
		job.OutputConfig.GCSDestination.OutputURIPrefix != input.OutputURIPrefix {
		t.Fatalf("job = %s", plan.body)
	}
	if plan.window != kernel.DefaultBatchCompletionWindow || !strings.HasSuffix(plan.endpoint, "/batchPredictionJobs") {
		t.Fatalf("window %s, endpoint %s", plan.window, plan.endpoint)
	}
	if _, err := planJob(&kernel.Pin{Model: "provider-model", Provider: routing.Provider{Kind: connectors.KindVertexAI}}, connectors.BatchVertex, input); err == nil {
		t.Fatal("a Vertex provider without a project and location produced a job address")
	}
}

func TestProjectedJobsCarryTheGatewayIdentifierAndNoProviderResourceName(t *testing.T) {
	t.Parallel()
	const arn, path = "arn:aws:bedrock:us-east-1:123456789012:model-invocation-job/abc", "projects/project-1/locations/us-central1/batchPredictionJobs/42"
	job := upstream.CloudJob{ID: arn, Status: upstream.CloudStatus("failed"), CreatedAt: 7, Error: &upstream.CloudError{
		Code: "ValidationException", Message: "job " + arn + " and " + path + " failed: bad record",
	}}
	record := upstream.CloudRecord{Dialect: connectors.BedrockBatchProfile, RouteModel: "route", InputURI: "s3://jobs/in/a.jsonl", OutputURIPrefix: "s3://jobs/out/run/"}
	raw := project(job, "batch_local", record)
	if strings.Contains(string(raw), "arn:") || strings.Contains(string(raw), "batchPredictionJobs") {
		t.Fatalf("projection names a provider resource: %s", raw)
	}
	got := decoded[object](t, raw)
	if got.ID != "batch_local" || got.Model != "route" || got.Dialect != connectors.BedrockBatchProfile || got.Error == nil ||
		got.Error.Code != "ValidationException" || !strings.HasSuffix(got.Error.Message, "failed: bad record") {
		t.Fatalf("projection = %s", raw)
	}
}
