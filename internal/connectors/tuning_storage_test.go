package connectors

import (
	"reflect"
	"testing"
)

func TestTuningDatasetAuthority(t *testing.T) {
	prefixes, err := NormalizeTuningDatasetPrefixes("vertex_ai", []string{"gs://training-bucket/data", "gs://training-bucket/data/"})
	if err != nil || !reflect.DeepEqual(prefixes, []string{"gs://training-bucket/data/"}) {
		t.Fatalf("prefixes=%v %v", prefixes, err)
	}
	config := Config{TuningDatasetURIPrefixes: prefixes}
	for uri, want := range map[string]bool{"gs://training-bucket/data/train.jsonl": true, "gs://training-bucket/data2/train.jsonl": false, "gs://other-bucket/data/train.jsonl": false, "gs://training-bucket/data/../secret": false, "gs://training-bucket/data/%2e%2e/secret": false, "https://training-bucket/data/train.jsonl": false} {
		if got := config.AllowsTuningDataset(uri); got != want {
			t.Errorf("%q authority=%v want=%v", uri, got, want)
		}
	}
	if _, err := NormalizeTuningDatasetPrefixes("openai", prefixes); err == nil {
		t.Fatal("GCS authority added to another connector")
	}
	clone := config.Clone()
	clone.TuningDatasetURIPrefixes[0] = "gs://other-bucket/"
	if config.TuningDatasetURIPrefixes[0] != "gs://training-bucket/data/" {
		t.Fatal("mutable tuning authority escaped revision clone")
	}
	if (Config{}).AllowsTuningDataset("gs://training-bucket/data/train.jsonl") {
		t.Fatal("missing operator grant authorized native dataset")
	}
}
