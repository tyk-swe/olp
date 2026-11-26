//go:build integration

package integration_test

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/retainedapi"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/tests/harness"
)

func openAIReplies(_ string, nth int) map[string]string {
	const file = `{"id":"file-provider-owned","object":"file","bytes":7,"created_at":1700000000,"filename":"batch.jsonl","purpose":"batch"}`
	batch := func(status string) string {
		return `{"id":"batch_provider-owned","object":"batch","endpoint":"/v1/chat/completions","input_file_id":"file-provider-owned","completion_window":"24h","status":"` + status + `","created_at":1700000000}`
	}
	response := func(id, previous string) string {
		return `{"id":"` + id + `","object":"response","created_at":1700000000,"expires_at":4102444800,` + previous + `"status":"completed","model":"provider-model","output":[{"id":"msg_2","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[]}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
	}
	created := response(fmt.Sprintf("resp_provider-owned-%d", nth), "")
	if nth == 2 {
		created = response("resp_provider-owned-2", `"previous_response_id":"resp_provider-owned-1",`)
	}
	replies := map[string]string{
		"POST /files":                                      file,
		"GET /files/file-provider-owned":                   file,
		"GET /files/file-provider-owned/content":           "CONTENT",
		"DELETE /files/file-provider-owned":                `{"id":"file-provider-owned","object":"file","deleted":true}`,
		"POST /batches":                                    batch("validating"),
		"GET /batches/batch_provider-owned":                batch("in_progress"),
		"POST /batches/batch_provider-owned/cancel":        batch("cancelling"),
		"POST /responses":                                  created,
		"POST /v1/responses":                               created,
		"GET /responses/resp_provider-owned-1":             response("resp_provider-owned-1", ""),
		"GET /responses/resp_provider-owned-1/input_items": `{"object":"list","data":[{"id":"msg_1","type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"first_id":"msg_1","last_id":"msg_1","has_more":false}`,
		"DELETE /responses/resp_provider-owned-1":          `{"id":"resp_provider-owned-1","object":"response.deleted","deleted":true}`,
	}
	for _, request := range []string{"GET /responses/resp_provider-owned-1", "GET /responses/resp_provider-owned-1/input_items", "DELETE /responses/resp_provider-owned-1"} {
		method, path, _ := strings.Cut(request, " ")
		replies[method+" /v1"+path] = replies[request]
	}
	return replies
}

func TestABatchUploadToAVendorThatRefusesBatchesAnswersWithTheVendorsOwnReason(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const form = "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"batch.jsonl\"\r\nContent-Type: application/jsonl\r\n\r\n{}\n\r\n--b--\r\n"
	for _, vendor := range vendors.All() {
		if vendor.BatchRefusal == "" {
			continue
		}
		upstream := &retainedUpstream{replies: openAIReplies}
		g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
			slug: "refused-" + vendor.ID, model: "batch-model", kind: vendor.Connector, vendor: vendor.ID, operations: []string{"generation", "batch"}, upstream: upstream,
		})
		g.key(t, "owner")
		_, raw := g.send(t, "POST", "/v1/files", map[string]string{"Authorization": "Bearer owner", "Content-Type": "multipart/form-data; boundary=b"}, form, 422)
		var refusal struct {
			Error struct{ Code, Message string }
		}
		if err := json.Unmarshal(raw, &refusal); err != nil || refusal.Error.Code != "target_capability" || refusal.Error.Message != vendor.BatchRefusal {
			t.Fatalf("%s refused a batch upload with %s, want its contract's reason %q", vendor.ID, raw, vendor.BatchRefusal)
		}
		if sent := upstream.sent("POST /files"); len(sent) != 0 {
			t.Fatalf("%s received %d uploads, want none", vendor.ID, len(sent))
		}
	}
}

func TestFilesAndBatchesHideProviderIdentifiersIsolateKeysAndSpendTheProviderCredential(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "stored-batch", "batch-model"
	const output = "file-" + providerOwned + "-output"
	upstream := &retainedUpstream{replies: func(request string, nth int) map[string]string {
		replies := openAIReplies(request, nth)
		const read = "GET /batches/batch_provider-owned"
		if request == read && nth > 1 {
			batch := strings.Replace(replies[read], "in_progress", "completed", 1)
			replies[read] = strings.TrimSuffix(batch, "}") + `,"output_file_id":"` + output + `"}`
		}
		if request == "POST /files" && nth == 3 {
			replies[request] = strings.Replace(replies[request], "file-"+providerOwned, output, 1)
		}
		return replies
	}}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	line := `{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"` + slug + `","messages":[{"role":"user","content":"hi"}]}}`
	form := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"batch.jsonl\"\r\nContent-Type: application/jsonl\r\n\r\n" + line + "\n\r\n--b--\r\n"
	file := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	if file.ID == "" {
		t.Fatal("upload returned no file identifier")
	}
	uploads := upstream.sent("POST /files")
	if len(uploads) != 1 || uploads[0].key != "listed-secret" || uploads[0].authorization != "" {
		t.Fatalf("upload reached the provider as %+v, want one call carrying only the provider credential", uploads)
	}
	if !strings.Contains(uploads[0].body, `"model":"`+model+`"`) || strings.Contains(uploads[0].body, slug) {
		t.Fatalf("uploaded lines must name the provider model instead of the route: %s", uploads[0].body)
	}
	if got := g.call(t, "owner", "GET", "/v1/files/"+file.ID, "", "", 200); got.ID != file.ID {
		t.Fatalf("file read returned %q, want %q", got.ID, file.ID)
	}
	if got := g.call(t, "owner", "GET", "/v1/files", "", "", 200).identifiers(); !slices.Equal(got, []string{file.ID}) {
		t.Fatalf("owner lists files %v, want only %s", got, file.ID)
	}
	g.call(t, "owner", "GET", "/v1/files/"+file.ID+"/content", "", "", 200)

	g.call(t, "stranger", "GET", "/v1/files/"+file.ID, "", "", 404)
	if got := g.call(t, "stranger", "GET", "/v1/files", "", "", 200).identifiers(); len(got) != 0 {
		t.Fatalf("another key lists files %v", got)
	}
	submission := `{"input_file_id":"` + file.ID + `","endpoint":"/v1/chat/completions","completion_window":"24h"}`
	g.call(t, "stranger", "POST", "/v1/batches", "application/json", submission, 404)
	if sent := upstream.sent("POST /batches"); len(sent) != 0 {
		t.Fatalf("a batch over another key's file reached the provider: %+v", sent)
	}

	batch := g.call(t, "owner", "POST", "/v1/batches", "application/json", submission, 200)
	if batch.ID == "" || batch.InputFileID != file.ID || batch.Status != "validating" {
		t.Fatalf("batch = %+v, want a gateway identifier over file %s", batch, file.ID)
	}
	submissions := upstream.sent("POST /batches")
	if len(submissions) != 1 || !strings.Contains(submissions[0].body, `"input_file_id":"file-provider-owned"`) || submissions[0].key != "listed-secret" {
		t.Fatalf("batch reached the provider as %+v, want the provider's file identifier and credential", submissions)
	}
	if got := g.call(t, "owner", "GET", "/v1/batches/"+batch.ID, "", "", 200); got.ID != batch.ID || got.InputFileID != file.ID || got.Status != "in_progress" {
		t.Fatalf("batch read = %+v", got)
	}
	if got := g.call(t, "owner", "GET", "/v1/batches", "", "", 200).identifiers(); !slices.Equal(got, []string{batch.ID}) {
		t.Fatalf("owner lists batches %v, want only %s", got, batch.ID)
	}
	g.call(t, "stranger", "GET", "/v1/batches/"+batch.ID, "", "", 404)
	g.call(t, "stranger", "POST", "/v1/batches/"+batch.ID+"/cancel", "", "", 404)
	if got := g.call(t, "owner", "POST", "/v1/batches/"+batch.ID+"/cancel", "", "", 200); got.ID != batch.ID || got.Status != "cancelling" {
		t.Fatalf("batch cancel = %+v", got)
	}
	if sent := upstream.sent("POST /batches/batch_provider-owned/cancel"); len(sent) != 1 {
		t.Fatalf("the provider saw %d cancels, want only the owner's", len(sent))
	}

	g.call(t, "stranger", "DELETE", "/v1/files/"+file.ID, "", "", 404)
	if got := g.call(t, "owner", "DELETE", "/v1/files/"+file.ID, "", "", 200); !got.Deleted || got.ID != file.ID {
		t.Fatalf("file delete = %+v", got)
	}
	if sent := upstream.sent("DELETE /files/file-provider-owned"); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes, want only the owner's", len(sent))
	}
	replacement := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	if replacement.ID == "" || replacement.ID == file.ID {
		t.Fatalf("upload with a reused provider ID returned %q, want a new gateway ID", replacement.ID)
	}
	g.call(t, "owner", "GET", "/v1/files/"+file.ID, "", "", 404)
	if got := g.call(t, "owner", "GET", "/v1/files", "", "", 200).identifiers(); !slices.Equal(got, []string{replacement.ID}) {
		t.Fatalf("owner lists files %v, want only %s", got, replacement.ID)
	}

	batchPath := "/v1/batches/" + batch.ID
	finished := g.call(t, "owner", "GET", batchPath, "", "", 200)
	if finished.OutputFileID == "" {
		t.Fatal("completed batch returned no output file identifier")
	}
	if err := g.store.Tombstone(t.Context(), finished.OutputFileID); err != nil {
		t.Fatal(err)
	}
	g.call(t, "owner", "GET", "/v1/batches", "", "", 200)
	if got := g.call(t, "owner", "GET", "/v1/files", "", "", 200).identifiers(); !slices.Equal(got, []string{replacement.ID}) {
		t.Fatalf("reading batch history recreated its deleted output, listed files %v", got)
	}
	newUpload := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	if newUpload.ID == "" || newUpload.ID == finished.OutputFileID {
		t.Fatalf("upload with the batch output's old provider ID returned %q, want a new gateway ID", newUpload.ID)
	}
	if got := g.call(t, "owner", "GET", batchPath, "", "", 200); got.OutputFileID != finished.OutputFileID {
		t.Fatalf("batch output changed to %q, want the original gateway ID %q", got.OutputFileID, finished.OutputFileID)
	}
}

func TestAStoredResponseKeepsItsCitedFileWhenTheProviderReusesTheFileID(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug = "cited-files"
	upstream := &retainedUpstream{replies: func(request string, nth int) map[string]string {
		replies := openAIReplies(request, 1)
		first := strings.Replace(replies["POST /responses"], `"annotations":[]`,
			`"annotations":[{"type":"file_citation","index":0,"file_id":"file-`+providerOwned+`","filename":"batch.jsonl"}]`, 1)
		later := strings.ReplaceAll(first, "resp_provider-owned-1", "resp_provider-owned-2")
		replies["GET /responses/resp_provider-owned-1"], replies["GET /responses/resp_provider-owned-2"] = first, later
		replies["POST /responses"] = first
		if nth > 1 {
			replies["POST /responses"] = later
		}
		return replies
	}}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: "citing-model", kind: "azure_openai", operations: []string{"generation", "batch"}, upstream: upstream,
	})
	g.key(t, "owner")
	ask := func(method, path, body string) (string, string) {
		t.Helper()
		_, raw := g.send(t, method, path, map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}, body, 200)
		var response struct {
			ID     string
			Output []struct {
				Content []struct {
					Annotations []struct {
						FileID string `json:"file_id"`
					}
				}
			}
		}
		if err := json.Unmarshal(raw, &response); err != nil || len(response.Output) != 1 || len(response.Output[0].Content) != 1 || len(response.Output[0].Content[0].Annotations) != 1 {
			t.Fatalf("%s %s returned %s, want one cited file: %v", method, path, raw, err)
		}
		return response.ID, response.Output[0].Content[0].Annotations[0].FileID
	}
	form := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"batch.jsonl\"\r\nContent-Type: application/jsonl\r\n\r\n" +
		`{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"` + slug + `","messages":[{"role":"user","content":"hi"}]}}` + "\n\r\n--b--\r\n"
	create := `{"model":"` + slug + `","input":"hi","store":true}`

	file := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	first, cited := ask("POST", "/v1/responses", create)
	if file.ID == "" || cited != file.ID {
		t.Fatalf("the first response cites %q, want the uploaded file %q", cited, file.ID)
	}
	g.call(t, "owner", "DELETE", "/v1/files/"+file.ID, "", "", 200)
	replacement := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	if replacement.ID == "" || replacement.ID == file.ID {
		t.Fatalf("upload with a reused provider ID returned %q, want a new gateway ID", replacement.ID)
	}

	if _, cited := ask("GET", "/v1/responses/"+first, ""); cited != file.ID {
		t.Fatalf("the first response now cites %q, want the file it was created with %q", cited, file.ID)
	}
	second, cited := ask("POST", "/v1/responses", create)
	if cited != replacement.ID {
		t.Fatalf("a response created after the reuse cites %q, want the replacement %q", cited, replacement.ID)
	}
	if _, cited := ask("GET", "/v1/responses/"+second, ""); cited != replacement.ID {
		t.Fatalf("the later response reads back citing %q, want the replacement %q", cited, replacement.ID)
	}
}

func TestAFineTuningJobKeepsItsFilesWhenTheProviderReusesTheFileID(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		field      string
		uploadedBy bool
	}{
		"training file": {field: `"training_file":"file-` + providerOwned + `"`, uploadedBy: true},
		"result file":   {field: `"result_files":["file-` + providerOwned + `"]`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pool, _ := harness.Database(t)
			const (
				slug  = "tuned-files"
				model = "tuning-model"
			)
			upstream := &retainedUpstream{replies: func(request string, _ int) map[string]string {
				replies := openAIReplies(request, 1)
				replies["GET /fine_tuning/jobs/ftjob-reused"] = `{"id":"ftjob-reused","object":"fine_tuning.job","created_at":1,"status":"succeeded","model":"` + model + `",` + tc.field + `}`
				return replies
			}}
			g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
				slug: slug, model: model, kind: "azure_openai", operations: []string{"batch", "fine_tuning"}, upstream: upstream,
			})
			keyID := g.key(t, "owner")
			form := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"batch.jsonl\"\r\nContent-Type: application/jsonl\r\n\r\n" +
				`{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"` + slug + `","messages":[{"role":"user","content":"hi"}]}}` + "\n\r\n--b--\r\n"

			uploaded := func() string {
				if !tc.uploadedBy {
					return ""
				}
				return g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200).ID
			}()
			seeded := g.fixture.resource("fine_tuning_job", keyID, "ftjob-reused")
			seeded.RouteSlug, seeded.ProviderRevisionID, seeded.RouteRevisionID = slug, g.providerRevisionID, g.routeRevisionID
			seeded.SlotID, seeded.TargetID, seeded.CredentialID = g.slotID, g.targetID, &g.credentialID
			seeded.State, seeded.ContractVersion = "running", new(retainedapi.Revision)
			seeded.Metadata = json.RawMessage(`{"upstream_model":"` + model + `"}`)
			job, err := g.store.Put(t.Context(), seeded)
			if err != nil {
				t.Fatal(err)
			}
			read := func() string {
				t.Helper()
				_, raw := g.send(t, "GET", "/v1/fine_tuning/jobs/"+job.ID, map[string]string{"Authorization": "Bearer owner"}, "", 200)
				var decoded struct {
					TrainingFile string   `json:"training_file"`
					ResultFiles  []string `json:"result_files"`
				}
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatalf("job read returned %s: %v", raw, err)
				}
				return decoded.TrainingFile + strings.Join(decoded.ResultFiles, ",")
			}
			original := read()
			if original == "" || uploaded != "" && original != uploaded {
				t.Fatalf("the job names file %q, want the uploaded file %q", original, uploaded)
			}
			g.call(t, "owner", "DELETE", "/v1/files/"+original, "", "", 200)
			replacement := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
			if replacement.ID == "" || replacement.ID == original {
				t.Fatalf("upload with a reused provider ID returned %q, want a new gateway ID", replacement.ID)
			}
			if got := read(); got != original {
				t.Fatalf("the job now names file %q, want the file it named before the reuse %q", got, original)
			}
		})
	}
}

func TestStrictBatchKeepsItsOriginalInputWhenTheProviderReusesAFileID(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "strict-batch", "batch-model"
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "azure_openai", profile: "azure-v1-chat", version: "/openai/v1", fidelity: routing.FidelityStrict,
		operations: []string{"generation", "batch"}, upstream: http.StripPrefix("/openai", &retainedUpstream{replies: openAIReplies}),
	})
	g.key(t, "owner")
	line := `{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}}`
	form := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nbatch\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"batch.jsonl\"\r\nContent-Type: application/jsonl\r\n\r\n" + line + "\n\r\n--b--\r\n"
	file := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	submission := `{"input_file_id":"` + file.ID + `","endpoint":"/v1/chat/completions","completion_window":"24h"}`
	batch := g.call(t, "owner", "POST", "/v1/batches", "application/json", submission, 200)
	g.call(t, "owner", "DELETE", "/v1/files/"+file.ID, "", "", 200)
	replacement := g.call(t, "owner", "POST", "/v1/files", "multipart/form-data; boundary=b", form, 200)
	if replacement.ID == "" || replacement.ID == file.ID {
		t.Fatalf("replacement ID = %q, want a new gateway ID", replacement.ID)
	}
	if got := g.call(t, "owner", "GET", "/v1/batches/"+batch.ID, "", "", 200); got.InputFileID != file.ID {
		t.Fatalf("batch input changed to %q, want the original gateway ID %q", got.InputFileID, file.ID)
	}
}

func TestStoredResponsesHideProviderIdentifiersContinueOnTheirProviderAndIsolateKeys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "stored-responses", "responses-model"
	upstream := &retainedUpstream{replies: openAIReplies}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation"}, upstream: upstream,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	const json = "application/json"

	first := g.call(t, "owner", "POST", "/v1/responses", json, `{"model":"`+slug+`","input":"hi","store":true}`, 200)
	if first.ID == "" {
		t.Fatal("stored response returned no identifier")
	}
	if first.Model != slug {
		t.Fatalf("response creation model = %q, want route %q", first.Model, slug)
	}
	providerExpiry := time.Unix(4102444800, 0)
	if kept := storedResponses(t, pool, g, "owner"); len(kept) != 1 || kept[0] != (storedResponse{"response", "completed", model, providerExpiry}) {
		t.Fatalf("the gateway kept %+v, want the response with the provider's state, model and expiry", kept)
	}
	created := upstream.sent("POST /responses")
	if len(created) != 1 || created[0].key != "listed-secret" || created[0].authorization != "" || !strings.Contains(created[0].body, `"model":"`+model+`"`) {
		t.Fatalf("response reached the provider as %+v, want the provider model and credential only", created)
	}
	if got := g.call(t, "owner", "GET", "/v1/responses/"+first.ID, "", "", 200); got.ID != first.ID || got.Model != first.Model {
		t.Fatalf("response read returned %q of model %q, want %q of model %q", got.ID, got.Model, first.ID, first.Model)
	}
	if got := g.call(t, "owner", "GET", "/v1/responses/"+first.ID+"/input_items", "", "", 200); len(got.Data) != 1 {
		t.Fatalf("input items = %+v, want the provider's one item", got)
	}

	continuation := `{"model":"` + slug + `","input":"again","previous_response_id":"` + first.ID + `","store":true}`
	g.call(t, "stranger", "GET", "/v1/responses/"+first.ID, "", "", 404)
	g.call(t, "stranger", "GET", "/v1/responses/"+first.ID+"/input_items", "", "", 404)
	g.call(t, "stranger", "POST", "/v1/responses", json, continuation, 400)
	g.call(t, "stranger", "DELETE", "/v1/responses/"+first.ID, "", "", 404)
	if sent := upstream.sent("POST /responses"); len(sent) != 1 {
		t.Fatalf("another key's continuation reached the provider: %+v", sent)
	}

	second := g.call(t, "owner", "POST", "/v1/responses", json, continuation, 200)
	if second.ID == "" || second.ID == first.ID || second.PreviousResponseID != first.ID {
		t.Fatalf("continuation returned %q after %q naming %q, want a new identifier that names the first by its gateway identifier", second.ID, first.ID, second.PreviousResponseID)
	}
	if sent := upstream.sent("POST /responses"); len(sent) != 2 || !strings.Contains(sent[1].body, `"previous_response_id":"resp_provider-owned-1"`) {
		t.Fatalf("continuation reached the provider as %+v, want the provider's identifier of the first response", sent)
	}
	unkept, err := http.NewRequestWithContext(t.Context(), "POST", g.server.URL+"/v1/responses", strings.NewReader(`{"model":"`+slug+`","input":"once more","previous_response_id":"`+first.ID+`","store":false}`))
	if err != nil {
		t.Fatal(err)
	}
	unkept.Header.Set("Authorization", "Bearer owner")
	unkept.Header.Set("Content-Type", json)
	answered, err := g.server.Client().Do(unkept)
	if err != nil {
		t.Fatal(err)
	}
	_ = answered.Body.Close()
	if kept := storedResponses(t, pool, g, "owner"); answered.StatusCode != 200 || len(kept) != 2 {
		t.Fatalf("a response the caller asked not to store answered %d and left %d stored, want 200 and the two stored before", answered.StatusCode, len(kept))
	}

	provider := g.snapshot.Providers[g.fixture.providerID]
	provider.ProjectID = &g.fixture.projectID
	provider.SharedTokenQuotas = []routing.SharedTokenQuota{{Model: model, Group: "responses", TokensPerMinute: 100000}}
	g.snapshot.Providers[g.fixture.providerID] = provider
	g.republish(t, func(*routing.Route) {})
	_, refused := g.send(t, "POST", "/v1/responses", map[string]string{"Authorization": "Bearer owner", "Content-Type": json}, continuation, 400)
	if !strings.Contains(string(refused), `"unbounded_work_budget"`) || len(upstream.sent("POST /responses")) != 3 {
		t.Fatalf("continuation with a shared quota answered %s, want a budget refusal before the provider", refused)
	}
	t.Logf("POST /v1/responses with previous_response_id and a shared token quota: HTTP 400 %s", refused)

	if got := g.call(t, "owner", "DELETE", "/v1/responses/"+first.ID, "", "", 200); !got.Deleted || got.ID != first.ID {
		t.Fatalf("response delete = %+v", got)
	}
	if sent := upstream.sent("DELETE /responses/resp_provider-owned-1"); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes, want only the owner's", len(sent))
	}
	g.call(t, "owner", "GET", "/v1/responses/"+first.ID, "", "", 404)

	native := &retainedUpstream{replies: openAIReplies}
	strict := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "strict-responses", model: model, kind: "azure_openai", profile: "azure-v1-responses", fidelity: routing.FidelityStrict,
		operations: []string{"generation"}, upstream: native,
	})
	strict.key(t, "strict-owner")
	strict.key(t, "strict-stranger")
	asked := time.Now()
	bound := strict.call(t, "strict-owner", "POST", "/v1/responses", json, `{"model":"strict-responses","input":"hi","store":true}`, 200)
	kept := storedResponses(t, pool, strict, "strict-owner")
	if !strings.HasPrefix(bound.ID, "strict_response_") || len(kept) != 1 || kept[0].kind != "strict_response" || kept[0].state != "completed" || kept[0].model != model {
		t.Fatalf("a strict route answered %q and kept %+v, want a strict response with the provider's state and model", bound.ID, kept)
	}
	if expiry := kept[0].expires; !expiry.After(asked) || expiry.After(asked.Add(resources.ContinuationLifetime)) {
		t.Fatalf("a strict response expires at %s, want within the gateway's continuation lifetime rather than the provider's %s", expiry, providerExpiry)
	}
	if got := strict.call(t, "strict-owner", "GET", "/v1/responses/"+bound.ID, "", "", 200); got.ID != bound.ID || got.Model != "strict-responses" {
		t.Fatalf("a strict response read returned %q of model %q, want %q of the route", got.ID, got.Model, bound.ID)
	}
	_, unstreamed := strict.send(t, "GET", "/v1/responses/"+bound.ID+"?stream=true", map[string]string{"Authorization": "Bearer strict-owner"}, "", 400)
	if !strings.Contains(string(unstreamed), `"target_capability"`) || len(native.sent("GET /v1/responses/resp_provider-owned-1")) != 1 {
		t.Fatalf("a strict response whose provider has no stream contract answered a streamed read with %s, want a capability refusal before the provider", unstreamed)
	}
	if got := strict.call(t, "strict-owner", "GET", "/v1/responses/"+bound.ID+"/input_items", "", "", 200); len(got.Data) != 1 {
		t.Fatalf("a strict response's input items = %+v, want the provider's one item", got)
	}
	following := `{"model":"strict-responses","input":"again","previous_response_id":"` + bound.ID + `","store":true}`
	strict.call(t, "strict-stranger", "GET", "/v1/responses/"+bound.ID, "", "", 404)
	strict.call(t, "strict-stranger", "POST", "/v1/responses", json, following, 400)
	strict.call(t, "strict-stranger", "DELETE", "/v1/responses/"+bound.ID, "", "", 404)
	next := strict.call(t, "strict-owner", "POST", "/v1/responses", json, following, 200)
	sent := native.sent("POST /v1/responses")
	if !strings.HasPrefix(next.ID, "strict_response_") || next.ID == bound.ID || next.PreviousResponseID != bound.ID || len(sent) != 2 || !strings.Contains(sent[1].body, `"previous_response_id":"resp_provider-owned-1"`) {
		t.Fatalf("a strict continuation returned %q after %q and reached the provider as %+v, want a new strict response that names the first by its gateway identifier and one provider request naming the provider's", next.ID, next.PreviousResponseID, sent)
	}
	if got := strict.call(t, "strict-owner", "DELETE", "/v1/responses/"+bound.ID, "", "", 200); !got.Deleted || got.ID != bound.ID {
		t.Fatalf("a strict response delete = %+v", got)
	}
	if sent := native.sent("DELETE /v1/responses/resp_provider-owned-1"); len(sent) != 1 {
		t.Fatalf("the provider saw %d deletes of the strict response, want only the owner's", len(sent))
	}
	strict.call(t, "strict-owner", "GET", "/v1/responses/"+bound.ID, "", "", 404)
}

func TestMissingStoredResponseReconcilesBackgroundUsage(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug = "missing-responses"
	var providerStatus atomic.Int32
	upstream := &retainedUpstream{
		status: func(request string) int {
			if request == "POST /responses" {
				return http.StatusOK
			}
			return int(providerStatus.Load())
		},
		replies: func(request string, nth int) map[string]string {
			if request == "POST /responses" {
				return map[string]string{request: fmt.Sprintf(`{"id":"resp_%s-%d","object":"response","background":true,"status":"queued","model":"provider-model","output":[]}`, providerOwned, nth)}
			}
			return map[string]string{request: `{"error":{"type":"server_error","message":"provider error"}}`}
		},
	}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: "responses-model", kind: "azure_openai", operations: []string{"generation"}, upstream: upstream,
	})
	keyID := g.key(t, "owner")
	headers := map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"}
	for i, op := range []struct{ name, method, suffix string }{
		{"read", http.MethodGet, ""},
		{"cancel", http.MethodPost, "/cancel"},
		{"input items", http.MethodGet, "/input_items"},
	} {
		t.Run(op.name, func(t *testing.T) {
			header, raw := g.send(t, http.MethodPost, "/v1/responses", headers, `{"model":"`+slug+`","input":"hi","background":true,"store":true}`, http.StatusOK)
			var created retainedObject
			if err := json.Unmarshal(raw, &created); err != nil || created.ID == "" || created.Status != "queued" {
				t.Fatalf("background response creation: %s, %v", raw, err)
			}
			assertUsage := func(want int) {
				t.Helper()
				var count, uncertain int
				err := pool.QueryRow(t.Context(), `SELECT count(*), count(*) FILTER (WHERE charge_status = 'billing_uncertain' AND NOT usage_observed AND NOT usage_complete)
					FROM olp.attempt_usage_facts WHERE request_id=$1 AND api_key_id=$2`, header.Get("X-Request-Id"), keyID).Scan(&count, &uncertain)
				if err != nil || count != want || uncertain != want {
					t.Fatalf("creation usage: %d facts, %d unobserved and uncertain, want %d of each (error: %v)", count, uncertain, want, err)
				}
			}

			path := "/v1/responses/" + created.ID + op.suffix
			native := fmt.Sprintf("%s /responses/resp_%s-%d%s", op.method, providerOwned, i+1, op.suffix)
			providerStatus.Store(http.StatusInternalServerError)
			g.send(t, op.method, path, headers, "", http.StatusInternalServerError)
			assertUsage(0)

			providerStatus.Store(http.StatusNotFound)
			for range 2 {
				_, raw := g.send(t, op.method, path, headers, "", http.StatusNotFound)
				var missing struct{ Error struct{ Code string } }
				if err := json.Unmarshal(raw, &missing); err != nil || missing.Error.Code != "not_found" {
					t.Fatalf("missing response: %s, %v", raw, err)
				}
				if got := len(upstream.sent(native)); got != 2 {
					t.Errorf("provider calls after missing response: got %d, want 2", got)
				}
				assertUsage(1)
			}
		})
	}
}

type storedResponse struct {
	kind, state, model string
	expires            time.Time
}

func storedResponses(t *testing.T, pool *pgxpool.Pool, g *storedGateway, key string) []storedResponse {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT kind, state, metadata->>'upstream_model', expires_at FROM olp.provider_resources WHERE api_key_id=$1 ORDER BY created_at`, g.runtime.keys[key].ID)
	if err != nil {
		t.Fatal(err)
	}
	kept, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storedResponse, error) {
		var r storedResponse
		err := row.Scan(&r.kind, &r.state, &r.model, &r.expires)
		r.expires = r.expires.Local()
		return r, err
	})
	if err != nil {
		t.Fatal(err)
	}
	return kept
}

func TestFreeFileControlsPassAnExhaustedSharedTokenQuota(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "shared-quota-files", "files-model"
	fixture := newUsageFixture(t, pool)
	g := newStoredGateway(t, fixture, storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "vector_store"},
		upstream: &retainedUpstream{replies: openAIReplies},
	})
	g.key(t, "owner")
	upload := "--b\r\nContent-Disposition: form-data; name=\"purpose\"\r\n\r\nassistants\r\n--b\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.txt\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--b--\r\n"
	_, raw := g.send(t, "POST", "/v1/files", map[string]string{"Authorization": "Bearer owner", "Content-Type": "multipart/form-data; boundary=b", "Idempotency-Key": uuid.NewString()}, upload, 200)
	var file retainedObject
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatal(err)
	}

	shared := []routing.SharedTokenQuota{{Model: model, Group: "files", TokensPerMinute: 1}}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.provider_revisions SET configuration=jsonb_set(configuration::jsonb,'{options,shared_token_quotas}',$2::jsonb)::json WHERE id=$1`, g.providerRevisionID, encoded(t, shared)); err != nil {
		t.Fatal(err)
	}
	before := g.runtime.release.Load().Snapshot()
	provider := g.snapshot.Providers[fixture.providerID]
	provider.ProjectID, provider.SharedTokenQuotas = &fixture.projectID, shared
	g.snapshot.Providers[fixture.providerID] = provider
	g.republish(t, func(*routing.Route) {})
	if err := quota.SyncSharedQuotas(t.Context(), g.limiter, before, g.runtime.release.Load().Snapshot()); err != nil {
		t.Fatal(err)
	}
	g.send(t, "POST", "/v1/chat/completions", map[string]string{"Authorization": "Bearer owner", "Content-Type": "application/json"},
		`{"model":"`+slug+`","messages":[{"role":"user","content":"hi"}]}`, 429)
	g.call(t, "owner", "GET", "/v1/files/"+file.ID, "", "", 200)
	g.call(t, "owner", "DELETE", "/v1/files/"+file.ID, "", "", 200)
}

func TestABatchFileUploadIsCheckedBeforeTheProviderKeptOnlyWhenTheProviderNamesItAndRefusedOffABatchRoute(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model = "upload-route", "batch-model"
	provider := &storeProvider{issues: "1", scripted: map[string]string{}, statuses: map[string]int{}, objects: func(issues string) map[string]string {
		return map[string]string{"POST /files": `{"id":"file-` + providerOwned + issues + `","object":"file","bytes":7,"created_at":1700000000,"filename":"batch.jsonl","purpose":"batch","status":"processed"}`}
	}}
	upstream := &retainedUpstream{replies: provider.replies, status: provider.status}
	sink := &terminals{}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "azure_openai", operations: []string{"generation", "batch"}, upstream: upstream, sink: sink,
	})
	owner := g.key(t, "owner")
	g.key(t, "elsewhere")
	elsewhere := g.runtime.keys["elsewhere"]
	elsewhere.Policy.AllowedRoutes = []string{"another"}
	g.runtime.keys["elsewhere"] = elsewhere
	part := func(name, filename, content string) string {
		disposition := `form-data; name="` + name + `"`
		if filename != "" {
			disposition += `; filename="` + filename + "\"\r\nContent-Type: application/jsonl"
		}
		return "--b\r\nContent-Disposition: " + disposition + "\r\n\r\n" + content + "\r\n"
	}
	line := func(model string) string {
		return `{"custom_id":"one","method":"POST","url":"/v1/chat/completions","body":{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}}` + "\n"
	}
	kept := func() []string {
		t.Helper()
		rows, err := pool.Query(t.Context(), `SELECT upstream_id FROM olp.provider_resources WHERE api_key_id=$1 ORDER BY upstream_id`, owner)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}
	upload := func(name, key, route, form string, status int, code string, classes ...string) retainedObject {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), "POST", g.server.URL+"/v1/files", strings.NewReader(form+"--b--\r\n"))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "multipart/form-data; boundary=b")
		if route != "" {
			request.Header.Set("X-OLP-Route", route)
		}
		recorded, sent, stored := len(sink.all()), len(upstream.sent("POST /files")), kept()
		response, err := g.server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		raw, _ := io.ReadAll(response.Body)
		var reply struct {
			retainedObject
			Error struct{ Code string }
		}
		if err := json.Unmarshal(raw, &reply); err != nil || response.StatusCode != status || reply.Error.Code != code {
			t.Fatalf("%s answered %d %s (%v), want %d with code %q", name, response.StatusCode, raw, err, status, code)
		}
		if strings.Contains(string(raw)+fmt.Sprint(response.Header), providerOwned) {
			t.Fatalf("%s exposed a provider identifier: %v %s", name, response.Header, raw)
		}
		seen := sink.all()
		if len(seen) != recorded+1 {
			t.Fatalf("%s recorded %d requests, want one", name, len(seen)-recorded)
		}
		attempted := make([]string, len(seen[recorded].Attempts))
		for i, attempt := range seen[recorded].Attempts {
			attempted[i] = attempt.Class
		}
		if !slices.Equal(attempted, classes) {
			t.Fatalf("%s recorded attempts %v, want %v", name, attempted, classes)
		}
		if asked := len(upstream.sent("POST /files")) - sent; asked != len(classes) {
			t.Fatalf("%s reached the provider %d times, want %d", name, asked, len(classes))
		}
		if status != http.StatusOK && !slices.Equal(kept(), stored) {
			t.Fatalf("%s failed but changed the kept files from %v to %v", name, stored, kept())
		}
		return reply.retainedObject
	}
	batch := part("purpose", "", "batch") + part("file", "batch.jsonl", line(slug))
	for _, refused := range []struct {
		name, key, route, form string
		status                 int
		code                   string
	}{
		{"an upload naming no route", "owner", "", batch, http.StatusBadRequest, "missing_required_parameter"},
		{"an upload naming an unknown route", "owner", "nowhere", batch, http.StatusNotFound, "route_not_found"},
		{"an upload by a key without the route", "elsewhere", slug, batch, http.StatusForbidden, "route_forbidden"},
		{"an upload without a file", "owner", slug, part("purpose", "", "batch"), http.StatusBadRequest, "missing_required_parameter"},
		{"an upload without a purpose", "owner", slug, part("file", "batch.jsonl", line(slug)), http.StatusBadRequest, "invalid_request"},
		{"an upload of two files", "owner", slug, batch + part("file", "more.jsonl", line(slug)), http.StatusBadRequest, "invalid_request"},
		{"a batch file that is not JSON lines", "owner", slug, part("purpose", "", "batch") + part("file", "batch.jsonl", "not json\n"), http.StatusBadRequest, "unsupported_parameter"},
		{"a batch line for another model", "owner", slug, part("purpose", "", "batch") + part("file", "batch.jsonl", line("another")), http.StatusBadRequest, "unsupported_parameter"},
	} {
		upload(refused.name, refused.key, refused.route, refused.form, refused.status, refused.code)
	}

	provider.answers("POST /files", http.StatusInternalServerError, `{"error":{"message":"The provider broke.","type":"server_error"}}`)
	upload("an upload the provider fails", "owner", slug, batch, http.StatusInternalServerError, "", "ambiguous")
	provider.answers("POST /files", http.StatusBadRequest, `{"error":{"message":"The provider refused.","type":"invalid_request_error"}}`)
	upload("an upload the provider refuses", "owner", slug, batch, http.StatusBadRequest, "", "upstream_client")
	provider.answers("POST /files", http.StatusOK, `{"object":"file"}`)
	upload("an upload the provider does not name", "owner", slug, batch, http.StatusBadGateway, "upstream_error", "protocol")
	provider.answers("POST /files", http.StatusOK, `not json`)
	upload("an upload the provider answers unreadably", "owner", slug, batch, http.StatusBadGateway, "upstream_error", "protocol")
	provider.recovers("POST /files")

	file := upload("a batch upload", "owner", slug, batch+part("note", "", "kept"), http.StatusOK, "", "success")
	sent := upstream.sent("POST /files")
	if body := sent[len(sent)-1].body; !strings.Contains(body, `"model":"`+model+`"`) || strings.Contains(body, slug) || !strings.Contains(body, `name="note"`) {
		t.Fatalf("the provider received %q, want the provider model and the caller's extra field", body)
	}
	var state string
	var manifest struct {
		Adapter, Endpoint string
		LineCount         int    `json:"line_count"`
		Operation         string `json:"line_operation"`
	}
	if err := pool.QueryRow(t.Context(), `SELECT state, metadata->'batch_manifest' FROM olp.provider_resources WHERE 'file_'||replace(id::text,'-','')=$1 AND upstream_id=$2 AND metadata->>'upstream_model'=$3`,
		file.ID, "file-"+providerOwned+"1", model).Scan(&state, &manifest); err != nil {
		t.Fatalf("the uploaded file %s is not kept under the provider's name and model: %v", file.ID, err)
	}
	if state != "processed" || manifest.Endpoint != "/v1/chat/completions" || manifest.LineCount != 1 || manifest.Operation != "generation" {
		t.Fatalf("the uploaded file is kept as %q with manifest %+v, want the provider's status and one generation line", state, manifest)
	}

	g.republish(t, func(route *routing.Route) { route.Operations = []string{"generation"} })
	upload("a batch upload on a route without batches", "owner", slug, batch, http.StatusBadRequest, "invalid_request")
}

type responseStreams struct {
	*retainedUpstream
	mu      sync.Mutex
	queries []string
	body    string
	kind    string
	status  int
}

func (p *responseStreams) answers(status int, kind string, events ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.queries, p.body, p.kind, p.status = nil, strings.Join(events, ""), kind, status
}

func (p *responseStreams) asked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.queries)
}

func (p *responseStreams) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" || r.URL.Query().Get("stream") != "true" {
		p.retainedUpstream.ServeHTTP(w, r)
		return
	}
	p.mu.Lock()
	p.queries = append(p.queries, r.URL.RawQuery)
	body, kind, status := p.body, p.kind, p.status
	p.mu.Unlock()
	w.Header().Set("Content-Type", kind)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func responseEvent(sequence int, kind, id, status, extra string) string {
	return fmt.Sprintf("event: %s\ndata: {\"type\":%q,\"sequence_number\":%d,\"response\":{\"id\":%q,\"object\":\"response\",\"created_at\":1700000000,\"status\":%q,\"model\":\"provider-model\",\"output\":[]%s}}\n\n", kind, kind, sequence, id, status, extra)
}

type responseStreamEvent struct {
	Type     string
	Response struct {
		ID, Model          string
		PreviousResponseID string `json:"previous_response_id"`
	}
}

func readResponseStream(t *testing.T, raw []byte) (kinds []string, events []responseStreamEvent) {
	t.Helper()
	for line := range strings.SplitSeq(string(raw), "\n") {
		data, found := strings.CutPrefix(line, "data: ")
		if !found {
			continue
		}
		var event responseStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatalf("event %q: %v", data, err)
		}
		kinds, events = append(kinds, strings.TrimPrefix(event.Type, "response.")), append(events, event)
	}
	return kinds, events
}

func TestAStoredResponseStreamCarriesGatewayIdentifiersForwardsTheCursorSettlesTheStoredStateAndEndsWhereTheProviderBreaks(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	const slug, model, stream, sse = "stored-streams", "responses-model", "?stream=true", "text/event-stream"
	sink := &terminals{}
	upstream := &responseStreams{retainedUpstream: &retainedUpstream{replies: func(request string, nth int) map[string]string {
		replies := openAIReplies(request, nth)
		if nth == 3 {
			replies["POST /responses"] = `{"id":"resp_` + providerOwned + `-3","object":"response","created_at":1700000000,"background":true,"status":"queued","model":"provider-model","output":[]}`
		}
		return replies
	}}}
	g := newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: slug, model: model, kind: "azure_openai", streams: true, operations: []string{"generation"}, upstream: upstream, sink: sink,
	})
	g.key(t, "owner")
	g.key(t, "stranger")
	create := func(previous string) string {
		return g.call(t, "owner", "POST", "/v1/responses", "application/json", `{"model":"`+slug+`","input":"hi",`+previous+`"store":true}`, 200).ID
	}
	first := create("")
	second := create(`"previous_response_id":"` + first + `",`)
	unfinished := create(`"background":true,`)
	provider := func(n int) string { return fmt.Sprintf("resp_%s-%d", providerOwned, n) }
	const usage = `,"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}`
	const delta = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"sequence_number\":2,\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"hello\"}\n\n"
	opened := responseEvent(0, "response.created", provider(1), "in_progress", "")
	completed := responseEvent(3, "response.completed", provider(1), "completed", usage)

	read := func(key, id, query string, status int) (http.Header, []byte, events.Envelope) {
		t.Helper()
		before := len(sink.all())
		header, raw := g.send(t, "GET", "/v1/responses/"+id+query, map[string]string{"Authorization": "Bearer " + key}, "", status)
		harness.Eventually(t, "the read's usage record", time.Second, func() (bool, string) { return len(sink.all()) > before, "" })
		recorded := sink.all()
		return header, raw, recorded[len(recorded)-1]
	}
	attempts := func(envelope events.Envelope) string {
		seen := make([]string, len(envelope.Attempts))
		for i, attempt := range envelope.Attempts {
			seen[i] = fmt.Sprintf("%s %s committed=%t", attempt.Mode, attempt.Class, attempt.Committed)
		}
		return strings.Join(seen, ", ")
	}

	for _, tc := range []struct {
		name, id, query string
		events          []string
		kinds, previous string
		asked           string
		outcome, class  string
		attempt         string
	}{
		{name: "a whole stream", id: first, query: stream, events: []string{opened, responseEvent(1, "response.in_progress", provider(1), "in_progress", ""), delta, completed},
			kinds: "created in_progress output_text.delta completed", asked: "api-version=&stream=true", outcome: "success", attempt: "streaming success committed=true"},
		{name: "a stream resumed after an event", id: first, query: stream + "&starting_after=1&include_obfuscation=false", events: []string{delta, completed},
			kinds: "output_text.delta completed", asked: "api-version=&include_obfuscation=false&starting_after=1&stream=true", outcome: "success", attempt: "streaming success committed=true"},
		{name: "a continuation", id: second, query: stream, events: []string{
			responseEvent(0, "response.created", provider(2), "in_progress", `,"previous_response_id":"`+provider(1)+`"`),
			responseEvent(1, "response.completed", provider(2), "completed", `,"previous_response_id":"`+provider(1)+`"`+usage),
		}, kinds: "created completed", previous: first, asked: "api-version=&stream=true", outcome: "success", attempt: "streaming success committed=true"},
		{name: "a response the provider cut short", id: first, query: stream, events: []string{opened, responseEvent(1, "response.incomplete", provider(1), "incomplete", `,"incomplete_details":{"reason":"max_output_tokens"}`)},
			kinds: "created incomplete", asked: "api-version=&stream=true", outcome: "success", attempt: "streaming success committed=true"},
		{name: "a response the provider failed", id: first, query: stream, events: []string{opened, responseEvent(1, "response.failed", provider(1), "failed", `,"error":{"code":"server_error","message":"refused listed-secret"}`)},
			kinds: "created failed", asked: "api-version=&stream=true", outcome: "failure", class: "upstream_response_failed", attempt: "streaming upstream_server committed=true"},
		{name: "a stream that turns to another response", id: first, query: stream, events: []string{opened, delta, responseEvent(3, "response.completed", provider(2), "completed", usage)},
			kinds: "created output_text.delta", asked: "api-version=&stream=true", outcome: "failure", class: "response_stream_incomplete", attempt: "streaming protocol committed=true"},
		{name: "a stream without its end", id: first, query: stream, events: []string{opened, delta},
			kinds: "created output_text.delta", asked: "api-version=&stream=true", outcome: "failure", class: "response_stream_incomplete", attempt: "streaming protocol committed=true"},
	} {
		upstream.answers(200, sse, tc.events...)
		header, raw, envelope := read("owner", tc.id, tc.query, 200)
		kinds, delivered := readResponseStream(t, raw)
		if header.Get("Content-Type") != sse || header.Get("Cache-Control") != "no-store" || strings.Join(kinds, " ") != tc.kinds || strings.Contains(string(raw), "listed-secret") {
			t.Errorf("%s reached the caller as %q with cache control %q carrying %q, want an uncached event stream of %q without the provider credential: %s",
				tc.name, header.Get("Content-Type"), header.Get("Cache-Control"), kinds, tc.kinds, raw)
		}
		for _, event := range delivered {
			if event.Response.ID != "" && (event.Response.ID != tc.id || event.Response.Model != slug || event.Response.PreviousResponseID != tc.previous) {
				t.Errorf("%s named response %q of model %q after %q, want %q of %q after %q", tc.name, event.Response.ID, event.Response.Model, event.Response.PreviousResponseID, tc.id, slug, tc.previous)
			}
		}
		if asked := upstream.asked(); len(asked) != 1 || asked[0] != tc.asked {
			t.Errorf("%s asked the provider %q, want one read with %q", tc.name, asked, tc.asked)
		}
		if got := attempts(envelope); envelope.Outcome != tc.outcome || envelope.ErrorClass != tc.class || !envelope.Committed || envelope.FirstByte == nil || got != tc.attempt {
			t.Errorf("%s was recorded as %s %q committed=%t first byte %v with attempts %q, want %s %q delivered with %q",
				tc.name, envelope.Outcome, envelope.ErrorClass, envelope.Committed, envelope.FirstByte, got, tc.outcome, tc.class, tc.attempt)
		}
	}

	if kept := storedResponses(t, pool, g, "owner"); len(kept) != 3 || kept[0].state != "completed" || kept[2].state != "queued" {
		t.Fatalf("the gateway kept %+v, want the first response completed and the third still queued", kept)
	}
	upstream.answers(200, sse, responseEvent(0, "response.created", provider(3), "in_progress", ""), responseEvent(1, "response.completed", provider(3), "completed", usage))
	read("owner", unfinished, stream, 200)
	if kept := storedResponses(t, pool, g, "owner"); kept[2].state != "completed" {
		t.Errorf("a stream that ended completed left the stored response %q, want completed", kept[2].state)
	}

	for _, tc := range []struct {
		name, key, query string
		provider         int
		kind             string
		events           []string
		status           int
		code, attempt    string
	}{
		{name: "a stream that opens on another response", key: "owner", query: stream, provider: 200, kind: sse, events: []string{responseEvent(0, "response.created", provider(2), "in_progress", "")},
			status: 502, code: "response_stream_incomplete", attempt: "streaming protocol committed=false"},
		{name: "an empty stream", key: "owner", query: stream, provider: 200, kind: sse, status: 502, code: "response_stream_incomplete", attempt: "streaming protocol committed=false"},
		{name: "a reply that is no stream", key: "owner", query: stream, provider: 200, kind: "application/json", events: []string{`{"id":"` + provider(1) + `"}`},
			status: 502, code: "fidelity_protocol_violation", attempt: "streaming protocol committed=false"},
		{name: "a provider outage", key: "owner", query: stream, provider: 500, kind: "application/json", events: []string{`{"error":{"message":"down"}}`}, status: 500, attempt: "streaming upstream_server committed=false"},
		{name: "another key", key: "stranger", query: stream, status: 404, code: "not_found"},
		{name: "a stream flag that is no boolean", key: "owner", query: "?stream=maybe", status: 400, code: "invalid_request"},
		{name: "a cursor without a stream", key: "owner", query: "?starting_after=1", status: 400, code: "invalid_request"},
		{name: "an unknown control", key: "owner", query: stream + "&verbose=1", status: 400, code: "invalid_request"},
		{name: "an include the gateway does not know", key: "owner", query: stream + "&include=everything", status: 400, code: "invalid_request"},
		{name: "a response the provider no longer has", key: "owner", query: stream, provider: 404, kind: "application/json", events: []string{`{"error":{"message":"gone"}}`},
			status: 404, code: "not_found", attempt: "streaming upstream_client committed=false"},
		{name: "a retry after the provider confirmed the response was missing", key: "owner", query: stream, status: 404, code: "not_found"},
	} {
		script := tc.events
		if tc.provider == 0 {
			script = []string{opened, completed}
		}
		upstream.answers(cmp.Or(tc.provider, 200), cmp.Or(tc.kind, sse), script...)
		_, raw, envelope := read(tc.key, first, tc.query, tc.status)
		var refusal struct{ Error struct{ Code string } }
		_ = json.Unmarshal(raw, &refusal)
		if got := attempts(envelope); refusal.Error.Code != tc.code || envelope.Committed || envelope.FirstByte != nil || got != tc.attempt {
			t.Errorf("%s answered %q recorded committed=%t with attempts %q, want %q undelivered with %q: %s", tc.name, refusal.Error.Code, envelope.Committed, got, tc.code, tc.attempt, raw)
		}
		if asked := upstream.asked(); (len(asked) != 0) != (tc.provider != 0) {
			t.Errorf("%s reached the provider as %q, want a provider read only when the request is one the owner may stream", tc.name, asked)
		}
	}

	native := &responseStreams{retainedUpstream: &retainedUpstream{replies: openAIReplies}}
	g = newStoredGateway(t, newUsageFixture(t, pool), storedPlan{
		slug: "strict-streams", model: model, kind: "azure_openai", profile: "azure-v1-responses", fidelity: routing.FidelityStrict, streams: true,
		operations: []string{"generation"}, upstream: native, sink: sink,
	})
	g.key(t, "owner")
	bound := g.call(t, "owner", "POST", "/v1/responses", "application/json", `{"model":"strict-streams","input":"hi","store":true}`, 200).ID
	for _, tc := range []struct {
		name, query, kinds, asked, outcome, class, attempt string
		events                                             []string
	}{
		{name: "a strict stream", query: stream + "&starting_after=1", events: []string{delta, completed}, kinds: "output_text.delta completed",
			asked: "starting_after=1&stream=true", outcome: "success", attempt: "streaming success committed=true"},
		{name: "a strict response the provider cut short", query: stream, events: []string{opened, responseEvent(1, "response.incomplete", provider(1), "incomplete", `,"incomplete_details":{"reason":"max_output_tokens"}`)},
			kinds: "created incomplete", asked: "stream=true", outcome: "failure", class: "response_incomplete", attempt: "streaming protocol committed=true"},
	} {
		native.answers(200, sse, tc.events...)
		_, raw, envelope := read("owner", bound, tc.query, 200)
		kinds, delivered := readResponseStream(t, raw)
		for _, event := range delivered {
			if event.Response.ID != "" && (event.Response.ID != bound || event.Response.Model != "strict-streams") {
				t.Errorf("%s named response %q of model %q, want %q of the route", tc.name, event.Response.ID, event.Response.Model, bound)
			}
		}
		if got, asked := attempts(envelope), native.asked(); strings.Join(kinds, " ") != tc.kinds || len(asked) != 1 || asked[0] != tc.asked || envelope.Outcome != tc.outcome || envelope.ErrorClass != tc.class || got != tc.attempt {
			t.Errorf("%s delivered %q after asking the provider %q and was recorded as %s %q with %q, want %q after %q recorded as %s %q with %q",
				tc.name, kinds, asked, envelope.Outcome, envelope.ErrorClass, got, tc.kinds, tc.asked, tc.outcome, tc.class, tc.attempt)
		}
	}
}
