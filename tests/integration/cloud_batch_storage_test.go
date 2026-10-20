//go:build integration

package integration_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestCloudBatchStorageConfigurationManagementRoundTrip(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	for _, test := range []struct {
		name, kind, profile, auth, region, project, scheme string
	}{
		{"Vertex", "vertex_ai", "vertex-batch-prediction", "adc", "us-central1", "fixture-project", "gs"},
		{"Bedrock", "bedrock", "bedrock-batch-inference", "default_chain", "us-east-1", "", "s3"},
	} {
		storage := map[string]any{
			"input_uri_prefix":  test.scheme + "://batch-fixture/input/",
			"output_uri_prefix": test.scheme + "://batch-fixture/output/",
		}
		if test.kind == "bedrock" {
			storage["role_arn"] = "arn:aws:iam::123456789012:role/olp-batch"
		}
		config := map[string]any{
			"kind": test.kind, "auth_mode": test.auth, "profile_id": test.profile, "profile_revision": "1",
			"cloud_region": test.region, "batch_storage": storage,
		}
		if test.project != "" {
			config["cloud_project"] = test.project
		}
		created := h.want(owner, http.MethodPost, "/api/v1/providers", map[string]any{
			"name": test.name, "configuration": config, "model": "fixture-model",
		}, idem(uuid.NewString()), http.StatusCreated)
		reloaded := h.want(owner, http.MethodGet, "/api/v1/providers/"+created["id"].(string), nil, nil, http.StatusOK)
		reloadedConfig := reloaded["configuration"].(map[string]any)
		if reloadedConfig["batch_storage"] == nil {
			t.Fatalf("%s batch_storage did not survive management read: %v", test.name, reloadedConfig)
		}
	}
	for _, test := range []struct {
		kind, profile, auth, region, project string
	}{
		{"vertex_ai", "vertex-batch-prediction", "adc", "us-central1", "fixture-project"},
		{"bedrock", "bedrock-batch-inference", "default_chain", "us-east-1", ""},
	} {
		config := map[string]any{
			"kind": test.kind, "auth_mode": test.auth, "profile_id": test.profile, "profile_revision": "1",
			"cloud_region": test.region,
		}
		if test.project != "" {
			config["cloud_project"] = test.project
		}
		h.want(owner, http.MethodPost, "/api/v1/providers", map[string]any{
			"name": "Missing storage " + test.kind, "configuration": config, "model": "fixture-model",
		}, idem(uuid.NewString()), http.StatusUnprocessableEntity)
	}
}
