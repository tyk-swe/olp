//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
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
	owner, provider, slug, plain := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}}, []string{"batch", "embeddings"},
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
	var ownerID, batchUUID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT api_key_id::text,id::text FROM olp_go.provider_resources WHERE kind='strict_batch' AND upstream_id='batch-up-1'`).Scan(&ownerID, &batchUUID); err != nil {
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
	// Eight first polls race to discover the two native output-file IDs. The
	// unique mapping must converge on one owner-local identity per file.
	type reply struct {
		status int
		body   []byte
		err    error
	}
	replies := make(chan reply, 8)
	for range 8 {
		go func() {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, h.HTTP.URL+"/v1/batches/"+batchID, nil)
			if err != nil {
				replies <- reply{err: err}
				return
			}
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				replies <- reply{err: err}
				return
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			replies <- reply{status: resp.StatusCode, body: body, err: err}
		}()
	}
	var first []byte
	for range 8 {
		got := <-replies
		if got.err != nil || got.status != http.StatusOK || !bytes.Contains(got.body, []byte(`"native_integer":9007199254740993`)) || !bytes.Contains(got.body, []byte(`"native_zero":-0`)) || !bytes.Contains(got.body, []byte(`"failed":1`)) || bytes.Contains(got.body, []byte("file-up-out")) {
			t.Fatalf("concurrent partial result projection: status=%d err=%v body=%s", got.status, got.err, got.body)
		}
		if first == nil {
			first = got.body
		} else if !bytes.Equal(first, got.body) {
			t.Fatalf("concurrent mapping changed local file identity:\n%s\n%s", first, got.body)
		}
	}
	raw = first
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
	before = fixture.dials.Load()
	status, raw, _ = h.gatewayRaw(http.MethodPost, "/v1/batches/"+batchID+"/cancel", key, nil, nil)
	if status != http.StatusConflict || !bytes.Contains(raw, []byte(`"code":"resource_transition"`)) || fixture.dials.Load() != before {
		t.Fatalf("terminal strict batch was cancelled again: %d %s", status, raw)
	}
	providerPath := "/api/v3/providers/" + provider["id"].(string)
	slots := h.want(owner, "GET", providerPath+"/credential-slots", nil, nil, http.StatusOK)
	credentialID := slots["items"].([]any)[0].(map[string]any)["credential_version_id"].(string)
	provider = h.want(owner, "GET", providerPath, nil, nil, http.StatusOK)
	h.want(owner, "POST", providerPath+"/credentials/"+credentialID+"/revoke", nil, withMatch(provider, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	h.refresh()
	before = fixture.dials.Load()
	status, raw, _ = h.gatewayRaw(http.MethodGet, "/v1/batches/"+batchID, key, nil, nil)
	if status != http.StatusConflict || !bytes.Contains(raw, []byte(`"code":"provider_resource_credential_unavailable"`)) || fixture.dials.Load() != before {
		t.Fatalf("revoked credential reached accepted strict batch: %d %s", status, raw)
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
	var retained int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.secrets WHERE id=$1::uuid`, batchUUID).Scan(&retained); err != nil || retained != 0 {
		t.Fatalf("expired strict batch ciphertext remains: count=%d err=%v", retained, err)
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
				[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}}, []string{"batch", "embeddings"},
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

func TestAcceptedStrictBatchMappingSurvivesClientDisconnect(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	fixture.batchCreateRaw.Store(`{"id":"batch-up-1","object":"batch","status":"validating","input_file_id":"file-up-1"}`)
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}}, []string{"batch", "embeddings"},
		map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-chat", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	input := `{"custom_id":"one","method":"POST","url":"/v1/embeddings","body":{"model":"` + vendorModel + `","input":"alpha"}}` + "\n"
	status, uploaded := h.uploadTestFile(slug, key, input)
	if status != http.StatusOK {
		t.Fatalf("upload: %d %v", status, uploaded)
	}
	before := fixture.batchCreates.Load()
	const lockKey int64 = 21416016
	locker, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			_, _ = locker.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
		}
		locker.Release()
	})
	if _, err := locker.Exec(t.Context(), "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `CREATE FUNCTION olp_go.wait_strict_batch_mapping() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind='strict_batch' THEN PERFORM pg_advisory_xact_lock(21416016); END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER wait_strict_batch_mapping BEFORE INSERT ON olp_go.provider_resources
FOR EACH ROW EXECUTE FUNCTION olp_go.wait_strict_batch_mapping()`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.HTTP.URL+"/v1/batches",
		strings.NewReader(`{"input_file_id":"`+uploaded["id"].(string)+`","endpoint":"/v1/embeddings","completion_window":"24h"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	finished := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		finished <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(
SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
AND wait_event_type='Lock' AND wait_event='advisory')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("strict accepted batch never reached its encrypted mapping; submissions=%d", fixture.batchCreates.Load()-before)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if _, err := locker.Exec(t.Context(), "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	locked = false
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled strict client did not release its HTTP request")
	}
	deadline = time.Now().Add(3 * time.Second)
	var ownerID, localID string
	for {
		err := h.Pool.QueryRow(t.Context(), `SELECT api_key_id::text,id::text FROM olp_go.provider_resources
WHERE kind='strict_batch' AND upstream_id='batch-up-1'`).Scan(&ownerID, &localID)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("accepted strict batch lost its encrypted owner mapping after client disconnect", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	local := "strict_batch_" + strings.ReplaceAll(localID, "-", "")
	if _, payload, err := h.Gateway.Resources.ReadDurableContract(t.Context(), resources.KindStrictBatch, ownerID, local); err != nil || len(payload) == 0 {
		t.Fatalf("accepted mapping is not decryptable: %v", err)
	}
	if fixture.batchCreates.Load() != before+1 {
		t.Fatalf("client disconnect caused %d batch submissions", fixture.batchCreates.Load()-before)
	}
}

func TestStrictBatchRejectsUncertifiedPerItemOperationBeforeUpload(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}}, []string{"batch"},
		map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-chat", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	before := fixture.dials.Load()
	input := `{"custom_id":"one","method":"POST","url":"/v1/embeddings","body":{"model":"` + vendorModel + `","input":"alpha"}}` + "\n"
	status, _ := h.uploadTestFile(slug, key, input)
	if status != http.StatusBadRequest || fixture.dials.Load() != before {
		t.Fatalf("uncertified embeddings item reached batch provider: %d calls=%d", status, fixture.dials.Load()-before)
	}
}

func TestStrictFileEarlyProviderAcceptanceNeverLooksComplete(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	sink := &captureSink{}
	h.Gateway.Sink = sink
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}}, []string{"batch", "embeddings"},
		map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-chat", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	var source strings.Builder
	for i := range 10000 {
		source.WriteString(`{"custom_id":"item-`)
		source.WriteString(strconv.Itoa(i))
		source.WriteString(`","method":"POST","url":"/v1/embeddings","body":{"model":"`)
		source.WriteString(vendorModel)
		source.WriteString(`","input":"`)
		source.WriteString(strings.Repeat("x", 350))
		source.WriteString(`"}}` + "\n")
	}
	fixture.earlyFileReply.Store(true)
	before := fixture.dials.Load()
	status, result := h.uploadTestFile(slug, key, source.String())
	if status < 400 || fixture.dials.Load() != before+1 {
		t.Fatalf("early provider reply exposed a completed file: status=%d result=%v calls=%d", status, result, fixture.dials.Load()-before)
	}
	var mapped int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.provider_resources WHERE kind='strict_file'`).Scan(&mapped); err != nil || mapped != 0 {
		t.Fatalf("partial upload published file mapping: count=%d err=%v", mapped, err)
	}
	fact := sink.last().Attempts[0]
	if fact.Interaction == nil || fact.Interaction.UpstreamState != usage.UpstreamUnknown || fact.Interaction.ClientState != usage.ClientUnobserved || !fact.BillingUncertain {
		t.Fatalf("partial accepted upload was not recorded as ambiguous: %+v", fact)
	}
}

func TestAcceptedStrictBatchCancellationSurvivesClientDisconnect(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	fixture.batchCreateRaw.Store(`{"id":"batch-up-1","object":"batch","status":"validating","input_file_id":"file-up-1"}`)
	h := newAccessHarness(t)
	owner, _, slug, _ := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}, map[string]any{"operation": "embeddings", "surface": "openai", "mode": "unary"}}, []string{"batch", "embeddings"},
		map[string]any{"fidelity": map[string]any{"mode": "strict"}},
		map[string]any{"profile_id": "azure-legacy-chat", "profile_revision": "1"})
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	input := `{"custom_id":"one","method":"POST","url":"/v1/embeddings","body":{"model":"` + vendorModel + `","input":"alpha"}}` + "\n"
	status, uploaded := h.uploadTestFile(slug, key, input)
	if status != http.StatusOK {
		t.Fatalf("upload: %d %v", status, uploaded)
	}
	status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/batches", key,
		strings.NewReader(`{"input_file_id":"`+uploaded["id"].(string)+`","endpoint":"/v1/embeddings","completion_window":"24h"}`),
		map[string]string{"Content-Type": "application/json"})
	if status != http.StatusOK {
		t.Fatalf("batch create: %d %s", status, raw)
	}
	batchID, _ := jsonStringField(raw, "id")
	before := fixture.dials.Load()
	const lockKey int64 = 21416017
	locker, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			_, _ = locker.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
		}
		locker.Release()
	})
	if _, err := locker.Exec(t.Context(), "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `CREATE FUNCTION olp_go.wait_strict_batch_cancel() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind='strict_batch' AND NEW.state='cancelling' THEN PERFORM pg_advisory_xact_lock(21416017); END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER wait_strict_batch_cancel BEFORE UPDATE ON olp_go.provider_resources
FOR EACH ROW EXECUTE FUNCTION olp_go.wait_strict_batch_cancel()`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.HTTP.URL+"/v1/batches/"+batchID+"/cancel", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	finished := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		finished <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(
SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
AND wait_event_type='Lock' AND wait_event='advisory')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("accepted cancellation never reached state commit; provider calls=%d", fixture.dials.Load()-before)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if _, err := locker.Exec(t.Context(), "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	locked = false
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled client did not release its HTTP request")
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		var state string
		err := h.Pool.QueryRow(t.Context(), `SELECT state FROM olp_go.provider_resources WHERE kind='strict_batch' AND upstream_id='batch-up-1'`).Scan(&state)
		if err == nil && state == "cancelling" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("provider accepted cancellation but mapping stayed %q: %v", state, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fixture.dials.Load() != before+1 || fixture.batchCreates.Load() != 1 {
		t.Fatalf("cancellation replayed work: provider calls=%d batch submissions=%d", fixture.dials.Load()-before, fixture.batchCreates.Load())
	}
	// The mock's next GET is intentionally stale (`validating`). The durable
	// resource must keep the accepted cancellation until a terminal result.
	status, raw, _ = h.gatewayRaw(http.MethodGet, "/v1/batches/"+batchID, key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"status":"cancelling"`)) {
		t.Fatalf("stale provider poll regressed accepted cancellation: %d %s", status, raw)
	}
	before = fixture.dials.Load()
	status, raw, _ = h.gatewayRaw(http.MethodPost, "/v1/batches/"+batchID+"/cancel", key, nil, nil)
	if status != http.StatusOK || !bytes.Contains(raw, []byte(`"status":"cancelling"`)) || fixture.dials.Load() != before {
		t.Fatalf("accepted cancellation retry called provider again: %d %s", status, raw)
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
