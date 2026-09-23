//go:build integration

package integration_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/resources"
)

func TestStrictBatchSourcePartialFilesAndLifecycle(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	fixture.fileCreateRaw.Store(`{"id":"file-up-1","object":"file","status":"processed","bytes":128,"native_integer":9007199254740993}`)
	fixture.fileFetchRaw.Store(`{"id":"file-up-1","object":"file","status":"processed","bytes":128,"native_integer":9007199254740993}`)
	fixture.batchCreateRaw.Store(`{"id":"batch-up-1","object":"batch","status":"validating","input_file_id":"file-up-1","native_integer":9007199254740993,"request_counts":{"total":2,"completed":0,"failed":0}}`)
	fixture.batchFetchRaw.Store(`{"id":"batch-up-1","object":"batch","status":"completed","input_file_id":"file-up-1","output_file_id":"file-up-out","error_file_id":"file-up-err","request_counts":{"total":2,"completed":1,"failed":1},"native_integer":9007199254740993,"native_zero":-0}`)
	fixture.files["file-up-out"] = map[string]any{"id": "file-up-out", "object": "file", "status": "processed"}
	fixture.files["file-up-err"] = map[string]any{"id": "file-up-err", "object": "file", "status": "processed"}
	success := `{"id":"batch_req_1","custom_id":"first","response":{"status_code":200,"request_id":"req-1","body":{"data":[{"embedding":[1,2]}]}}}` + "\n"
	failure := `{"id":"batch_req_2","custom_id":"second","response":null,"error":{"code":"model_error","message":"fixture failure"}}` + "\n"
	fixture.contentByID.Store("file-up-out", success)
	fixture.contentByID.Store("file-up-err", failure)

	h := newAccessHarness(t)
	owner, _, slug, plain := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}}, []string{"batch"},
		map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-chat", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	other := stateKey(t, h, owner, slug, true)
	h.refresh()
	input := `{"custom_id":"first","method":"POST","url":"/v1/embeddings","body":{"model":"` + vendorModel + `","input":"alpha"}}` + "\n" +
		`{"custom_id":"second","method":"POST","url":"/v1/embeddings","body":{"model":"` + vendorModel + `","input":"beta"}}` + "\n"
	before := fixture.dials.Load()
	if status, _ := h.uploadTestFile(slug, plain, input); status != http.StatusForbidden || fixture.dials.Load() != before {
		t.Fatal("strict durable upload without state authority reached the provider")
	}
	for _, invalid := range []string{
		strings.Replace(input, `"custom_id":"second"`, `"custom_id":"first"`, 1),
		strings.Replace(input, `"model":"`+vendorModel+`"`, `"model":"other-model"`, 1),
		strings.Replace(input, `"url":"/v1/embeddings"`, `"url":"/v1/unsupported"`, 1),
	} {
		status, _ := h.uploadTestFile(slug, key, invalid)
		if status < 400 || fixture.dials.Load() != before {
			t.Fatalf("unqualified batch items uploaded: %d", status)
		}
	}
	status, uploaded := h.uploadTestFile(slug, key, input)
	if status != http.StatusOK {
		t.Fatalf("strict upload: %d %v", status, uploaded)
	}
	fileID, _ := uploaded["id"].(string)
	if !strings.HasPrefix(fileID, "strict_file_") {
		t.Fatalf("strict file identity/result: %v", uploaded)
	}
	status, raw, _ := h.gatewayRaw(http.MethodGet, "/v1/files/"+fileID, key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"native_integer":9007199254740993`)) {
		t.Fatalf("native file precision: %d %s", status, raw)
	}
	if status, _, _ := h.gatewayRaw(http.MethodGet, "/v1/files/"+fileID, other, nil, nil); status != http.StatusNotFound {
		t.Fatalf("other key read strict file: %d", status)
	}
	var count int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.provider_resources WHERE kind='strict_file' AND api_key_id=(SELECT api_key_id FROM olp_go.provider_resources WHERE upstream_id='file-up-1' AND kind='strict_file')`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("strict file owner mapping: count=%d err=%v", count, err)
	}
	var metadata, ciphertext string
	if err := h.Pool.QueryRow(t.Context(), `SELECT r.metadata::text,encode(s.ciphertext,'hex') FROM olp_go.provider_resources r JOIN olp_go.secrets s ON s.id=r.id WHERE r.kind='strict_file' AND r.upstream_id='file-up-1'`).Scan(&metadata, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(metadata, "first") || strings.Contains(metadata, "9007199254740993") || strings.Contains(ciphertext, hex.EncodeToString([]byte("first"))) {
		t.Fatal("strict file source/result leaked into ordinary metadata or ciphertext")
	}

	batchSource := `{"completion_window":"24h", "custom_native":{"large":9007199254740993,"zero":-0},"input_file_id":"` + fileID + `","endpoint":"/v1/embeddings"}`
	before = fixture.dials.Load()
	for _, invalid := range []string{
		`{"input_file_id":"` + fileID + `","input_file_id":"` + fileID + `","endpoint":"/v1/embeddings","completion_window":"24h"}`,
		`{"input_file_id":"` + fileID + `","endpoint":"/v1/embeddings","completion_window":"24h","output_file_id":"caller"}`,
		`{"input_file_id":"` + fileID + `","endpoint":"/v1/unsupported","completion_window":"24h"}`,
	} {
		status, _, _ := h.gatewayRaw(http.MethodPost, "/v1/batches", key, strings.NewReader(invalid), map[string]string{"Content-Type": "application/json"})
		if status < 400 || fixture.dials.Load() != before {
			t.Fatalf("invalid strict batch dispatched: %d %s", status, invalid)
		}
	}
	status, raw, _ = h.gatewayRaw(http.MethodPost, "/v1/batches", key, strings.NewReader(batchSource), map[string]string{"Content-Type": "application/json"})
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"native_integer":9007199254740993`)) {
		t.Fatalf("strict create: %d %s", status, raw)
	}
	batchID, ok := jsonStringField(raw, "id")
	if !ok || !strings.HasPrefix(batchID, "strict_batch_") || bytes.Contains(raw, []byte("file-up-1")) {
		t.Fatalf("batch identity leaked upstream: %s", raw)
	}
	wantBound := `{"completion_window":"24h","custom_native":{"large":9007199254740993,"zero":-0},"input_file_id":"file-up-1","endpoint":"/v1/embeddings","model":"` + vendorModel + `"}`
	if got := fixture.lastBatchRaw.Load().(string); got != wantBound {
		t.Fatalf("source/overlay changed native JSON:\n got %s\nwant %s", got, wantBound)
	}
	var ownerID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT api_key_id::text FROM olp_go.provider_resources WHERE kind='strict_batch' AND upstream_id='batch-up-1'`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	_, encrypted, err := h.Gateway.Resources.ReadDurableContract(t.Context(), resources.KindStrictBatch, ownerID, batchID)
	if err != nil {
		t.Fatal(err)
	}
	var native struct{ Source, Effective, Result []byte }
	if err := json.Unmarshal(encrypted, &native); err != nil || !bytes.Equal(native.Source, []byte(batchSource)) || !bytes.Equal(native.Effective, []byte(wantBound)) || !bytes.Equal(native.Result, []byte(fixture.batchCreateRaw.Load().(string))) {
		t.Fatalf("encrypted native source/effective/result changed: err=%v", err)
	}
	validResult := fixture.batchFetchRaw.Load().(string)
	fixture.batchFetchRaw.Store(`{"id":"batch-up-1","status":"completed","input_file_id":"file-up-other"}`)
	status, raw, _ = h.gatewayRaw(http.MethodGet, "/v1/batches/"+batchID, key, nil, nil)
	if status != http.StatusBadGateway || !bytes.Contains(raw, []byte(`"code":"fidelity_protocol_violation"`)) {
		t.Fatalf("provider changed accepted batch input identity: %d %s", status, raw)
	}
	fixture.batchFetchRaw.Store(validResult)
	status, raw, _ = h.gatewayRaw(http.MethodPost, "/v1/batches/"+batchID+"/cancel", key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"status":"cancelling"`)) {
		t.Fatalf("cancel: %d %s", status, raw)
	}
	status, raw, _ = h.gatewayRaw(http.MethodGet, "/v1/batches/"+batchID, key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"native_integer":9007199254740993`)) || !bytes.Contains(raw, []byte(`"native_zero":-0`)) || !bytes.Contains(raw, []byte(`"failed":1`)) || bytes.Contains(raw, []byte("file-up-out")) {
		t.Fatalf("partial result projection: %d %s", status, raw)
	}
	outputID, ok := jsonStringField(raw, "output_file_id")
	if !ok || !strings.HasPrefix(outputID, "strict_file_") {
		t.Fatalf("output file identity: %s", raw)
	}
	errorID, ok := jsonStringField(raw, "error_file_id")
	if !ok || !strings.HasPrefix(errorID, "strict_file_") {
		t.Fatalf("error file identity: %s", raw)
	}
	for _, row := range []struct{ id, want string }{{outputID, success}, {errorID, failure}} {
		status, body, headers := h.gatewayRaw(http.MethodGet, "/v1/files/"+row.id+"/content", key, nil, nil)
		sum := sha256.Sum256([]byte(row.want))
		if status != http.StatusOK || string(body) != row.want || headers.Get("X-OLP-Content-SHA256") != hex.EncodeToString(sum[:]) {
			t.Fatalf("partial item bytes/digest: %d %s", status, body)
		}
	}
	status, raw, _ = h.gatewayRaw(http.MethodGet, "/v1/batches", key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(raw, []byte(batchID)) || bytes.Contains(raw, []byte("batch-up-1")) {
		t.Fatalf("batch list: %d %s", status, raw)
	}
	status, raw, _ = h.gatewayRaw(http.MethodGet, "/v1/files", key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(raw, []byte(outputID)) || !bytes.Contains(raw, []byte(errorID)) {
		t.Fatalf("file list: %d %s", status, raw)
	}
	if status, _, _ := h.gatewayRaw(http.MethodGet, "/v1/batches/"+batchID, other, nil, nil); status != http.StatusNotFound {
		t.Fatalf("other key read strict batch: %d", status)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.provider_resources SET expires_at=now()-interval '1 second' WHERE kind='strict_batch' AND upstream_id='batch-up-1'`); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := h.gatewayRaw(http.MethodGet, "/v1/batches/"+batchID, key, nil, nil); status != http.StatusNotFound {
		t.Fatalf("expired batch remained retrievable: %d", status)
	}
	if _, err := h.Gateway.Resources.CleanupExpired(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestStrictUnaryBackgroundResponseRetainsOneAcceptedWork(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}, []string{"generation"},
		map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-responses", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	fixture.resps["resp-up-1"]["status"] = "in_progress"
	fixture.resps["resp-up-1"]["usage"] = nil
	before := fixture.dials.Load()
	for _, body := range []string{
		`{"model":"` + slug + `","input":"queued","background":true,"store":false}`,
		`{"model":"` + slug + `","input":"queued","background":true,"store":true,"stream":true}`,
	} {
		status, _, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
		if status < 400 || fixture.dials.Load() != before {
			t.Fatalf("unqualified background contract dispatched: %d %s", status, body)
		}
	}
	status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/responses", key,
		strings.NewReader(`{"model":"`+slug+`","input":"queued","background":true,"store":true}`),
		map[string]string{"Content-Type": "application/json"})
	if status != http.StatusOK {
		t.Fatalf("background create: %d %s", status, raw)
	}
	local, ok := jsonStringField(raw, "id")
	if !ok || !strings.HasPrefix(local, "strict_response_") || fixture.dials.Load() != before+1 {
		t.Fatalf("background accepted work identity: %d %s", status, raw)
	}
	fixture.resps["resp-up-1"]["status"] = "completed"
	fixture.resps["resp-up-1"]["usage"] = map[string]any{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10}
	for range 2 {
		status, raw, _ = h.gatewayRaw(http.MethodGet, "/v1/responses/"+local, key, nil, nil)
		if status != http.StatusOK || !bytes.Contains(raw, []byte(`"status":"completed"`)) || bytes.Contains(raw, []byte("resp-up-1")) {
			t.Fatalf("background retrieve: %d %s", status, raw)
		}
	}
	var attempts, inputTokens, outputTokens int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*),coalesce(sum(input_tokens),0),coalesce(sum(output_tokens),0)
 FROM olp_go.attempt_usage_facts WHERE upstream_model=$1 AND operation='generation'`, vendorModel).Scan(&attempts, &inputTokens, &outputTokens); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || inputTokens != 4 || outputTokens != 6 {
		t.Fatalf("background accounting duplicated or lost: attempts=%d input=%d output=%d", attempts, inputTokens, outputTokens)
	}
}

func TestStrictBatchPinnedOpenAISDKs(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "tests/sdk-smoke/node_modules/openai")); err != nil {
		t.Fatal("pinned OpenAI JavaScript SDK is missing; run pnpm install --frozen-lockfile")
	}
	for _, test := range []struct {
		name, binary string
		args         []string
	}{
		{"javascript", "node", []string{"tests/sdk-smoke/durable-batch.mjs"}},
		{"python", "uv", []string{"run", "--project", "tests/sdk-smoke-python", "--frozen", "python", "tests/sdk-smoke-python/durable_batch.py"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newOpenAIFixture(t, "")
			fixture.batchCreateRaw.Store(`{"id":"batch-up-1","object":"batch","status":"validating","input_file_id":"file-up-1"}`)
			fixture.batchFetchRaw.Store(`{"id":"batch-up-1","object":"batch","status":"completed","input_file_id":"file-up-1","output_file_id":"file-up-out","error_file_id":"file-up-err","request_counts":{"total":2,"completed":1,"failed":1}}`)
			fixture.files["file-up-out"] = map[string]any{"id": "file-up-out", "object": "file", "status": "processed"}
			fixture.files["file-up-err"] = map[string]any{"id": "file-up-err", "object": "file", "status": "processed"}
			fixture.contentByID.Store("file-up-out", `{"custom_id":"first","response":{"status_code":200}}`+"\n")
			fixture.contentByID.Store("file-up-err", `{"custom_id":"second","error":{"code":"fixture"}}`+"\n")
			h := newAccessHarness(t)
			owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
				[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}}, []string{"batch"},
				map[string]any{"fidelity": map[string]any{"mode": "strict"}},
				map[string]any{"profile_id": "azure-legacy-chat", "profile_revision": "1"})
			key := stateKey(t, h, owner, slug, true)
			h.refresh()
			before := fixture.batchCreates.Load()
			cmd := exec.CommandContext(t.Context(), test.binary, test.args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "OLP_DURABLE_BASE="+h.HTTP.URL, "OLP_DURABLE_ROUTE="+slug,
				"OLP_DURABLE_MODEL="+vendorModel, "OLP_DURABLE_KEY="+key)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("pinned %s durable SDK: %v\n%s", test.name, err, output)
			}
			if fixture.batchCreates.Load() != before+1 {
				t.Fatalf("pinned %s SDK submitted %d batches; %s", test.name, fixture.batchCreates.Load()-before, output)
			}
		})
	}
}

func jsonStringField(raw []byte, name string) (string, bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return "", false
	}
	var value string
	if json.Unmarshal(obj[name], &value) != nil || value == "" {
		return "", false
	}
	return value, true
}
