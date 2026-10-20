package connectors

import "testing"

func TestBatchStorageValidationAndContainment(t *testing.T) {
	storage, err := NormalizeBatchStorage("vertex_ai", "vertex-batch-prediction", &BatchStorage{
		InputURIPrefix: "gs://vertex-data/input", OutputURIPrefix: "gs://vertex-data/output",
	})
	if err != nil || storage.InputURIPrefix != "gs://vertex-data/input/" || storage.OutputURIPrefix != "gs://vertex-data/output/" {
		t.Fatalf("normalized batch storage=%+v err=%v", storage, err)
	}
	if !storage.Contains(storage.InputURIPrefix, "gs://vertex-data/input/a/b.jsonl") ||
		storage.Contains(storage.InputURIPrefix, "gs://vertex-data/input-other/a.jsonl") ||
		storage.Contains(storage.InputURIPrefix, "gs://other-bucket/input/a.jsonl") {
		t.Fatal("batch storage URI containment is not bucket and segment scoped")
	}
	if _, err := NormalizeBatchStorage("bedrock", "bedrock-batch-inference", &BatchStorage{
		InputURIPrefix: "s3://batch-input/", OutputURIPrefix: "s3://batch-output/",
		RoleARN: "arn:aws:iam::123456789012:role/batch-role",
	}); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		kind, profile string
		storage       *BatchStorage
	}{
		{"vertex_ai", "vertex-batch-prediction", nil},
		{"bedrock", "bedrock-batch-inference", &BatchStorage{InputURIPrefix: "s3://a-bucket/in", OutputURIPrefix: "s3://a-bucket/out"}},
		{"openai", "", &BatchStorage{InputURIPrefix: "gs://a-bucket/in", OutputURIPrefix: "gs://a-bucket/out"}},
		{"vertex_ai", "vertex-batch-prediction", &BatchStorage{InputURIPrefix: "gs://bad..bucket/in", OutputURIPrefix: "gs://a-bucket/out"}},
		{"bedrock", "bedrock-batch-inference", &BatchStorage{InputURIPrefix: "s3://1.2.3.4/in", OutputURIPrefix: "s3://a-bucket/out", RoleARN: "arn:aws:iam::123456789012:role/batch"}},
	} {
		if _, err := NormalizeBatchStorage(test.kind, test.profile, test.storage); err == nil {
			t.Fatalf("accepted invalid batch storage %+v", test)
		}
	}
}

func TestBatchStorageConnectorCloneOwnsConfiguration(t *testing.T) {
	source := &BatchStorage{InputURIPrefix: "gs://batch/input/", OutputURIPrefix: "gs://batch/output/"}
	cloned := (Config{Kind: "vertex_ai", BatchStorage: source}).Clone()
	cloned.BatchStorage.InputURIPrefix = "gs://changed/input/"
	if source.InputURIPrefix != "gs://batch/input/" {
		t.Fatal("connector clone shares mutable batch storage")
	}
}

func TestCloudBatchControlPlaneURLHelpers(t *testing.T) {
	vertex, err := VertexBatchJobsURL("project-one", "us", "job-1", "cancel")
	if err != nil || vertex != "https://aiplatform.us.rep.googleapis.com/v1/projects/project-one/locations/us/batchPredictionJobs/job-1:cancel" {
		t.Fatalf("Vertex batch cancel URL=%q err=%v", vertex, err)
	}
	bedrock, err := BedrockBatchJobsURL("us-east-1", "arn:aws:bedrock:us-east-1:123456789012:model-invocation-job/job:1", "stop")
	if err != nil || bedrock != "https://bedrock.us-east-1.amazonaws.com/model-invocation-job/arn:aws:bedrock:us-east-1:123456789012:model-invocation-job%2Fjob:1/stop" {
		t.Fatalf("Bedrock stop URL=%q err=%v", bedrock, err)
	}
}
