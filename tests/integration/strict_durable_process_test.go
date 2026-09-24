//go:build integration

package integration_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/testutil"
)

// A new gateway process reconstructs the owner, encrypted native source and
// output/error file mappings from the database. It never resubmits a batch.
func TestStrictBatchSurvivesGatewayRestartWithPartialFiles(t *testing.T) {
	binary := required(t, "OLP_TEST_BINARY")
	fixture := newOpenAIFixture(t, "")
	fixture.batchCreateRaw.Store(`{"id":"batch-up-1","object":"batch","status":"validating","input_file_id":"file-up-1"}`)
	fixture.batchFetchRaw.Store(`{"id":"batch-up-1","object":"batch","status":"completed","input_file_id":"file-up-1","output_file_id":"file-up-out","error_file_id":"file-up-err","request_counts":{"total":2,"completed":1,"failed":1}}`)
	fixture.files["file-up-out"] = map[string]any{"id": "file-up-out", "object": "file", "status": "processed"}
	fixture.files["file-up-err"] = map[string]any{"id": "file-up-err", "object": "file", "status": "processed"}
	fixture.contentByID.Store("file-up-out", "success-1\n")
	fixture.contentByID.Store("file-up-err", "failure-2\n")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}}, []string{"batch", "embeddings"},
		map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-chat", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	item := `{"custom_id":"one","method":"POST","url":"/v1/embeddings","body":{"model":"` + vendorModel + `","input":"first"}}` + "\n" +
		`{"custom_id":"two","method":"POST","url":"/v1/embeddings","body":{"model":"` + vendorModel + `","input":"second"}}` + "\n"
	status, uploaded := h.uploadTestFile(slug, key, item)
	if status != http.StatusOK {
		t.Fatalf("upload: %d %v", status, uploaded)
	}
	status, body, _ := h.gatewayRaw(http.MethodPost, "/v1/batches", key,
		strings.NewReader(`{"input_file_id":"`+uploaded["id"].(string)+`","endpoint":"/v1/embeddings","completion_window":"24h"}`),
		map[string]string{"Content-Type": "application/json"})
	if status != http.StatusOK || fixture.batchCreates.Load() != 1 {
		t.Fatalf("accepted batch: %d %s", status, body)
	}
	batchID, ok := jsonStringField(body, "id")
	if !ok {
		t.Fatalf("local batch ID missing: %s", body)
	}
	h.HTTP.Close()
	env := continuationProcessEnvironment(t, h)
	first := testutil.StartProcess(t, binary, "gateway", env)
	call := func(origin, method, path string) (int, []byte) {
		resp := processContinuationRequest(t, origin, key, method, path, nil, nil)
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, raw
	}
	status, body = call(first.PublicOrigin, http.MethodPost, "/v1/batches/"+batchID+"/cancel")
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"status":"cancelling"`)) || fixture.batchCreates.Load() != 1 {
		t.Fatalf("restart cancellation: %d %s", status, body)
	}
	status, body = call(first.PublicOrigin, http.MethodGet, "/v1/batches/"+batchID)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"failed":1`)) || bytes.Contains(body, []byte("file-up-err")) || fixture.batchCreates.Load() != 1 {
		t.Fatalf("restart partial retrieval: %d %s", status, body)
	}
	outputID, outOK := jsonStringField(body, "output_file_id")
	errorID, errOK := jsonStringField(body, "error_file_id")
	if !outOK || !errOK {
		t.Fatalf("partial file mappings absent: %s", body)
	}
	if err := first.Kill(); err != nil {
		t.Fatal(err)
	}
	second := testutil.StartProcess(t, binary, "gateway", env)
	for _, item := range []struct{ id, want string }{{outputID, "success-1\n"}, {errorID, "failure-2\n"}} {
		status, body = call(second.PublicOrigin, http.MethodGet, "/v1/files/"+item.id+"/content")
		if status != http.StatusOK || string(body) != item.want {
			t.Fatalf("second restart file: %d %s", status, body)
		}
	}
	if fixture.batchCreates.Load() != 1 {
		t.Fatalf("restart re-dispatched batch %d times", fixture.batchCreates.Load())
	}
}
