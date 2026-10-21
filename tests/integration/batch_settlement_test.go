//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

type interruptedBatchReader struct {
	inner       *batches.OpenAIReader
	interrupted bool
}

type oversizedPollingBatchReader struct{ batches.Reader }

func (r *oversizedPollingBatchReader) Poll(context.Context, batches.Settlement, *batches.PinnedTarget) (batches.BatchSnapshot, error) {
	return batches.BatchSnapshot{}, batches.ErrBatchTooLarge
}

func TestBatchRecoveryPreservesAmbiguousSharedInput(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	_, _, slug, key := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}}, []string{"batch"})
	status, uploaded := h.uploadTestFile(slug, key, nativeBatchInput("shared-input"))
	if status != http.StatusOK {
		t.Fatalf("upload: %d %v", status, uploaded)
	}
	fixture.batchCreateStatus.Store(http.StatusBadGateway)
	status, body, _ := h.gatewayRaw(http.MethodPost, "/v1/batches", key, bytes.NewReader(batchCreateBody(uploaded["id"].(string))), map[string]string{"Content-Type": "application/json", "X-OLP-Route": slug})
	if status != http.StatusBadGateway || !bytes.Contains(body, []byte(`"code":"submission_unknown"`)) {
		t.Fatalf("lost acknowledgement: %d %s", status, body)
	}
	store := batches.NewStore(h.Pool)
	claims, err := store.Claim(t.Context(), time.Now().Add(time.Minute), 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %+v %v", claims, err)
	}
	first := claims[0]
	second := first
	second.ResourceID, second.State, second.ClaimID = uuid.New(), batches.StateSubmitting, nil
	second.SubmissionDigest = bytes.Repeat([]byte{1}, 32)
	var peerCursor struct {
		Submission map[string]json.RawMessage `json:"submission"`
	}
	if err := json.Unmarshal(second.Cursor, &peerCursor); err != nil {
		t.Fatal(err)
	}
	peerCursor.Submission["creator_deadline"], _ = json.Marshal(time.Now().Add(time.Minute))
	second.Cursor, _ = json.Marshal(peerCursor)
	if err := store.BeginSubmission(t.Context(), &second); err != nil {
		t.Fatal(err)
	}
	if err := store.Finish(t.Context(), first, batches.StateSubmissionUnknown, time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	assertBatchSettlement(t, h, first.ResourceID, "submission_unknown", 0, 0)
	assertBatchSettlement(t, h, second.ResourceID, "submitting", 0, 0)
	if err := store.MarkUnknown(t.Context(), second.ResourceID, "submission_unknown", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.batch_settlements SET next_attempt_at=now() WHERE resource_id=$1`, first.ResourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	assertBatchSettlement(t, h, first.ResourceID, "submission_unknown", 0, 0)
	assertBatchSettlement(t, h, second.ResourceID, "submission_unknown", 0, 0)
	var mappings int
	if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resources WHERE id=ANY($1::uuid[])`, []uuid.UUID{first.ResourceID, second.ResourceID}).Scan(&mappings); err != nil || mappings != 0 {
		t.Fatalf("ambiguous job acquired an owner: %d err=%v", mappings, err)
	}
	// The second intent never dispatched; once its definitive failure removes
	// it, the remaining provider job has exactly one possible gateway source.
	if err := store.DeleteSubmission(t.Context(), second.ResourceID); err != nil {
		t.Fatal(err)
	}
	setBatchFixtureResults(t, fixture, "batch-up-1", "completed", 1, "shared-result", "", batchChatLine("shared-input", "ok"), "")
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.batch_settlements SET next_attempt_at=now() WHERE resource_id=$1`, first.ResourceID); err != nil {
		t.Fatal(err)
	}
	if _, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	assertBatchSettlement(t, h, first.ResourceID, "settled", 1, 0)
}

func (r *interruptedBatchReader) Poll(ctx context.Context, settlement batches.Settlement, target *batches.PinnedTarget) (batches.BatchSnapshot, error) {
	return r.inner.Poll(ctx, settlement, target)
}

func (r *interruptedBatchReader) Reconcile(ctx context.Context, settlement batches.Settlement, target *batches.PinnedTarget, inputFileID, endpoint string) (batches.Reconciliation, error) {
	return r.inner.Reconcile(ctx, settlement, target, inputFileID, endpoint)
}

func (r *interruptedBatchReader) ReadResults(ctx context.Context, target *batches.PinnedTarget, snapshot batches.BatchSnapshot,
	cursor batches.Cursor, commit batches.ResultPageCommit) (batches.Cursor, bool, error) {
	return r.inner.ReadResults(ctx, target, snapshot, cursor, func(results []batches.Result, next batches.Cursor) error {
		if err := commit(results, next); err != nil {
			return err
		}
		if len(results) > 0 && !r.interrupted {
			r.interrupted = true
			return errors.New("simulated worker crash after committed page")
		}
		return nil
	})
}

func TestBatchSettlementReconcilesPagesUnknownSubmissionsAndPartialResults(t *testing.T) {
	fixture := newOpenAIFixture(t, "")
	h := newAccessHarness(t)
	owner, provider, slug, key := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}},
		[]string{"batch"})
	start := time.Now().UTC().Add(-10 * time.Minute)
	addBatchPrice(t, h, owner, provider["id"].(string), "0.5", start)
	inputIDs := make([]string, 0, 102)
	for i := 0; i < 101; i++ {
		inputIDs = append(inputIDs, fmt.Sprintf("line-%03d", i))
	}
	inputIDs = append(inputIDs, "failed-line")
	inputFileID := uploadBatchInputForSettlement(t, h, key, slug, inputIDs...)

	var output bytes.Buffer
	for i := 0; i < 101; i++ {
		output.WriteString(batchChatLine(fmt.Sprintf("line-%03d", i), "ok"))
	}
	outputID, errorID := "file-result-batch-up-1", "file-error-batch-up-1"
	localID := createBatchForSettlement(t, h, key, slug, inputFileID)
	upstreamID := batchUpstreamID(t, h, localID)
	setBatchFixtureResults(t, fixture, upstreamID, "completed", 102, outputID, errorID,
		output.String(), batchErrorLine("failed-line", "invalid_request"))
	resourceID := batchResourceID(t, localID)
	var apiKeyID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT api_key_id::text FROM olp.batch_settlements WHERE resource_id=$1`, resourceID).Scan(&apiKeyID); err != nil {
		t.Fatal(err)
	}
	var pending bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT olp.has_pending_resource_usage($1::uuid)`, apiKeyID).Scan(&pending); err != nil || !pending {
		t.Fatalf("batch intent did not project a pending budget subject: %v %v", pending, err)
	}

	// Reproduce a worker that claimed the intent just before the gateway's
	// successful response activates it. Its stale finish must not revert state.
	store := batches.NewStore(h.Pool)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.batch_settlements SET state='submitting',upstream_id=NULL,next_attempt_at=now(),created_at=now()-interval '3 minutes',
		cursor=jsonb_set(cursor,'{submission,creator_deadline}',to_jsonb(now()+interval '5 minutes')) WHERE resource_id=$1`, resourceID); err != nil {
		t.Fatal(err)
	}
	if pass, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil || pass.Claimed != 1 {
		t.Fatalf("live creator fencing pass: %+v err=%v", pass, err)
	}
	assertBatchSettlement(t, h, resourceID, "submitting", 0, 0)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.batch_settlements SET next_attempt_at=now() WHERE resource_id=$1`, resourceID); err != nil {
		t.Fatal(err)
	}
	claims, err := store.Claim(t.Context(), time.Now().Add(time.Minute), 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("submission claim: %+v %v", claims, err)
	}
	tx, err := h.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	expected := int64(1)
	if err = store.ActivateTx(t.Context(), tx, resourceID, upstreamID, &expected); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT expected_results FROM olp.batch_settlements WHERE resource_id=$1`, resourceID).Scan(&expected); err != nil || expected != 102 {
		t.Fatalf("activation lowered the submission expectation: %d err=%v", expected, err)
	}
	if err = store.Finish(t.Context(), claims[0], batches.StateSubmitting, time.Now(), ""); !errors.Is(err, batches.ErrClaimLost) {
		t.Fatalf("stale submission claim overwrote activation: %v", err)
	}
	claims[0].State = batches.StateActive
	if err = store.Finish(t.Context(), claims[0], batches.StateActive, time.Now(), ""); err != nil {
		t.Fatal(err)
	}

	reader := &interruptedBatchReader{inner: openAIBatchReader(h)}
	first := batchWorker(h, reader)
	if pass, err := first.ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil || pass.Claimed != 1 || pass.Failed != 1 {
		t.Fatalf("first settlement pass: pass=%+v err=%v", pass, err)
	}
	var pageSettled int64
	if err := h.Pool.QueryRow(t.Context(), `SELECT settled_results FROM olp.batch_settlements WHERE resource_id=$1`, resourceID).Scan(&pageSettled); err != nil || pageSettled != batches.PageSize {
		t.Fatalf("first committed page count=%d err=%v", pageSettled, err)
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.batch_settlements SET next_attempt_at=now() WHERE resource_id=$1`, resourceID); err != nil {
		t.Fatal(err)
	}
	workers := []*batches.Worker{batchWorker(h, nil), batchWorker(h, nil)}
	var wg sync.WaitGroup
	errs := make(chan error, len(workers))
	for _, worker := range workers {
		wg.Go(func() {
			_, err := worker.ReconcileOnce(t.Context(), batches.WorkerBatchSize)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertBatchSettlement(t, h, resourceID, "settled", 102, 0)
	facts, batchFacts, unpricedFacts, cost := batchFactTotals(t, h, resourceID)
	if facts != 102 || batchFacts != 102 || unpricedFacts != 1 || cost != "505.000000000000" {
		t.Fatalf("priced batch facts: total=%d batch=%d unpriced=%d cost=%s", facts, batchFacts, unpricedFacts, cost)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT olp.has_pending_resource_usage($1::uuid)`, apiKeyID).Scan(&pending); err != nil || pending {
		t.Fatalf("settled batch kept a pending budget subject: %v %v", pending, err)
	}

	addBatchPrice(t, h, owner, provider["id"].(string), "", start.Add(time.Minute))
	unpricedID := createBatchForSettlement(t, h, key, slug, uploadBatchInputForSettlement(t, h, key, slug, "unpriced-line"))
	unpricedUpstream := batchUpstreamID(t, h, unpricedID)
	unpricedOutput := "file-result-" + unpricedUpstream
	setBatchFixtureResults(t, fixture, unpricedUpstream, "completed", 1, unpricedOutput, "",
		batchChatLine("unpriced-line", "ok"), "")
	unpricedResource := batchResourceID(t, unpricedID)
	if pass, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil || pass.Settled != 1 {
		t.Fatalf("unpriced settlement pass: pass=%+v err=%v", pass, err)
	}
	facts, batchFacts, unpricedFacts, cost = batchFactTotals(t, h, unpricedResource)
	if facts != 1 || batchFacts != 1 || unpricedFacts != 1 || cost != "0.000000000000" {
		t.Fatalf("missing batch multiplier was priced: total=%d batch=%d unpriced=%d cost=%s", facts, batchFacts, unpricedFacts, cost)
	}

	missingID := createBatchForSettlement(t, h, key, slug, uploadBatchInputForSettlement(t, h, key, slug, "only-line", "missing-line"))
	missingUpstream := batchUpstreamID(t, h, missingID)
	setBatchFixtureResults(t, fixture, missingUpstream, "completed", 1, "file-result-"+missingUpstream, "",
		batchChatLine("only-line", "ok"), "")
	missingResource := batchResourceID(t, missingID)
	if _, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	assertBatchSettlement(t, h, missingResource, "uncertain", 1, 1)

	conflictID := createBatchForSettlement(t, h, key, slug, uploadBatchInputForSettlement(t, h, key, slug, "duplicate-line", "other-line"))
	conflictUpstream := batchUpstreamID(t, h, conflictID)
	conflictContent := batchChatLine("duplicate-line", "first") + batchChatLine("duplicate-line", "different")
	setBatchFixtureResults(t, fixture, conflictUpstream, "completed", 2, "file-result-"+conflictUpstream, "",
		conflictContent, "")
	conflictResource := batchResourceID(t, conflictID)
	if _, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	assertBatchSettlement(t, h, conflictResource, "conflict", 1, 0)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.batch_settlements SET close_after=now(),next_attempt_at=now() WHERE resource_id=$1`, conflictResource); err != nil {
		t.Fatal(err)
	}
	if _, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	assertBatchSettlement(t, h, conflictResource, "uncertain", 1, 1)

	lateConflictID := createBatchForSettlement(t, h, key, slug, uploadBatchInputForSettlement(t, h, key, slug, "duplicate-line"))
	lateUpstream := batchUpstreamID(t, h, lateConflictID)
	setBatchFixtureResults(t, fixture, lateUpstream, "completed", 1, "file-result-"+lateUpstream, "", conflictContent, "")
	lateResource := batchResourceID(t, lateConflictID)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.batch_settlements SET close_after=now() WHERE resource_id=$1`, lateResource); err != nil {
		t.Fatal(err)
	}
	if _, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	assertBatchSettlement(t, h, lateResource, "uncertain", 1, 1)

	failedID := createBatchForSettlement(t, h, key, slug, uploadBatchInputForSettlement(t, h, key, slug, "failed-partial", "failed-line"))
	failedUpstream := batchUpstreamID(t, h, failedID)
	setBatchFixtureResults(t, fixture, failedUpstream, "failed", 2, "file-result-"+failedUpstream, "file-error-"+failedUpstream,
		batchChatLine("failed-partial", "ok"), batchErrorLine("failed-line", "invalid_request"))
	failedResource := batchResourceID(t, failedID)
	if _, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	assertBatchSettlement(t, h, failedResource, "settled", 2, 0)

	cancelledID := createBatchForSettlement(t, h, key, slug, uploadBatchInputForSettlement(t, h, key, slug, "paid-partial", "cancelled-line"))
	cancelledUpstream := batchUpstreamID(t, h, cancelledID)
	setBatchFixtureResults(t, fixture, cancelledUpstream, "cancelled", 2, "file-result-"+cancelledUpstream, "file-error-"+cancelledUpstream,
		batchChatLine("paid-partial", "ok"), batchErrorLine("cancelled-line", "batch_cancelled"))
	cancelledResource := batchResourceID(t, cancelledID)
	if _, err := batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	assertBatchSettlement(t, h, cancelledResource, "settled", 2, 0)

	_, _, ambiguousRoute, ambiguousKey := provisionOpenAIWith(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}},
		[]string{"batch"}, map[string]any{"overall_timeout_ms": 1000})
	ambiguousUploadStatus, ambiguousUpload := h.uploadTestFile(ambiguousRoute, ambiguousKey, nativeBatchInput("ambiguous-input"))
	if ambiguousUploadStatus != http.StatusOK {
		t.Fatalf("ambiguous input upload: %d %v", ambiguousUploadStatus, ambiguousUpload)
	}
	ambiguousSource := batchCreateBody(ambiguousUpload["id"].(string))
	knownID := createBatchForSettlement(t, h, ambiguousKey, ambiguousRoute, ambiguousUpload["id"].(string))
	setBatchFixtureResults(t, fixture, batchUpstreamID(t, h, knownID), "completed", 1, "known-reuse-output", "", batchChatLine("ambiguous-input", "ok"), "")
	hold := make(chan struct{})
	read := make(chan struct{})
	fixture.batchCreateHold, fixture.batchCreateRead = hold, read
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(hold) }) }
	defer release()
	response := make(chan struct {
		status int
		body   []byte
	}, 1)
	go func() {
		status, body, _ := h.gatewayRaw("POST", "/v1/batches", ambiguousKey, bytes.NewReader(ambiguousSource),
			map[string]string{"Content-Type": "application/json", "X-OLP-Route": ambiguousRoute})
		response <- struct {
			status int
			body   []byte
		}{status, body}
	}()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("provider fixture did not read the ambiguous create")
	}
	var firstResponse struct {
		status int
		body   []byte
	}
	select {
	case firstResponse = <-response:
	case <-time.After(5 * time.Second):
		t.Fatal("ambiguous create did not time out")
	}
	if firstResponse.status != http.StatusBadGateway || !bytes.Contains(firstResponse.body, []byte(`"code":"submission_unknown"`)) {
		t.Fatalf("ambiguous create response: %d %s", firstResponse.status, firstResponse.body)
	}
	retryStatus, retryBody, _ := h.gatewayRaw("POST", "/v1/batches", ambiguousKey, bytes.NewReader(ambiguousSource),
		map[string]string{"Content-Type": "application/json", "X-OLP-Route": ambiguousRoute})
	if retryStatus != http.StatusConflict || !bytes.Contains(retryBody, []byte(`"code":"submission_unknown"`)) {
		t.Fatalf("identical retry response: %d %s", retryStatus, retryBody)
	}
	release()
	fixture.batchCreateHold, fixture.batchCreateRead = nil, nil
	ambiguousAuthority, err := h.Runtime.Authenticate(ambiguousKey)
	if err != nil {
		t.Fatal(err)
	}
	var ambiguousResource uuid.UUID
	if err = h.Pool.QueryRow(t.Context(), `SELECT resource_id FROM olp.batch_settlements
		WHERE api_key_id=$1 AND state='submission_unknown' ORDER BY created_at DESC LIMIT 1`, ambiguousAuthority.ID).Scan(&ambiguousResource); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"conflict", "uncertain"} {
		if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.batch_settlements SET state=$2,closed_at=CASE WHEN $2='uncertain' THEN now() ELSE NULL END WHERE resource_id=$1`, ambiguousResource, state); err != nil {
			t.Fatal(err)
		}
		listed := h.want(owner, http.MethodGet, "/api/v1/provider-resources?api_key_id="+ambiguousAuthority.ID+"&state="+state, nil, nil, http.StatusOK)
		items := listed["items"].([]any)
		if len(items) != 1 || items[0].(map[string]any)["settlement"].(map[string]any)["state"] != state {
			t.Fatalf("unmapped %s settlement disappeared from management: %v", state, listed)
		}
	}
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.batch_settlements SET state='submission_unknown',closed_at=NULL WHERE resource_id=$1`, ambiguousResource); err != nil {
		t.Fatal(err)
	}

	if _, err = batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	var adopted int
	if err = h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.provider_resources WHERE id=$1`, ambiguousResource).Scan(&adopted); err != nil || adopted != 1 {
		t.Fatalf("provider-evidence adoption inserted %d resources: %v", adopted, err)
	}
	apiKey := h.want(owner, http.MethodGet, "/api/v1/api-keys/"+ambiguousAuthority.ID, nil, nil, http.StatusOK)
	h.want(owner, http.MethodPost, "/api/v1/api-keys/"+ambiguousAuthority.ID+"/revoke", nil,
		withMatch(apiKey, map[string]string{"Idempotency-Key": uuid.NewString()}), http.StatusOK)
	var adoptedUpstream string
	if err = h.Pool.QueryRow(t.Context(), `SELECT upstream_id FROM olp.batch_settlements WHERE resource_id=$1`, ambiguousResource).Scan(&adoptedUpstream); err != nil {
		t.Fatal(err)
	}
	setBatchFixtureResults(t, fixture, adoptedUpstream, "completed", 1, "file-result-"+adoptedUpstream, "",
		batchChatLine("revoked-key-line", "ok"), "")
	if _, err = h.Pool.Exec(t.Context(), `UPDATE olp.batch_settlements SET next_attempt_at=now() WHERE resource_id=$1`, ambiguousResource); err != nil {
		t.Fatal(err)
	}
	if _, err = batchWorker(h, nil).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	assertBatchSettlement(t, h, ambiguousResource, "settled", 1, 0)
	oversizedID := createBatchForSettlement(t, h, key, slug, uploadBatchInputForSettlement(t, h, key, slug, "oversized-line"))
	if _, err := batchWorker(h, &oversizedPollingBatchReader{Reader: openAIBatchReader(h)}).ReconcileOnce(t.Context(), batches.WorkerBatchSize); err != nil {
		t.Fatal(err)
	}
	assertBatchSettlement(t, h, batchResourceID(t, oversizedID), "uncertain", 0, 1)
	fixture.batchCreateStatus.Store(http.StatusBadGateway)
	defer fixture.batchCreateStatus.Store(0)
	serverFailureSource := []byte(fmt.Sprintf(`{"input_file_id":%q,"endpoint":"/v1/chat/completions","completion_window":"24h","metadata":{"case":"accepted-5xx","rank":9007199254740993}}`, inputFileID))
	status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/batches", key, bytes.NewReader(serverFailureSource), map[string]string{"Content-Type": "application/json"})
	if status != http.StatusBadGateway || !bytes.Contains(raw, []byte(`"code":"submission_unknown"`)) {
		t.Fatalf("accepted HTTP 5xx lost its intent: %d %s", status, raw)
	}
	beforeRetry := fixture.dials.Load()
	status, raw, _ = h.gatewayRaw(http.MethodPost, "/v1/batches", key, bytes.NewReader(serverFailureSource), map[string]string{"Content-Type": "application/json"})
	if status != http.StatusConflict || !bytes.Contains(raw, []byte(`"code":"submission_unknown"`)) || fixture.dials.Load() != beforeRetry {
		t.Fatalf("ambiguous HTTP 5xx permitted duplicate work: %d %s", status, raw)
	}
	var reordered map[string]json.RawMessage
	if err := json.Unmarshal(serverFailureSource, &reordered); err != nil {
		t.Fatal(err)
	}
	pretty, err := json.MarshalIndent(reordered, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	status, raw, _ = h.gatewayRaw(http.MethodPost, "/v1/batches", key, bytes.NewReader(pretty), map[string]string{"Content-Type": "application/json"})
	if status != http.StatusConflict || fixture.dials.Load() != beforeRetry {
		t.Fatalf("equivalent JSON bypassed unresolved-submission protection: %d %s", status, raw)
	}
	var unresolvedID uuid.UUID
	if err := h.Pool.QueryRow(t.Context(), `SELECT resource_id FROM olp.batch_settlements WHERE api_key_id=$1 AND state='submission_unknown' ORDER BY created_at DESC LIMIT 1`, apiKeyID).Scan(&unresolvedID); err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"conflict", "uncertain"} {
		if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.batch_settlements SET state=$2,closed_at=CASE WHEN $2='uncertain' THEN now() ELSE NULL END WHERE resource_id=$1`, unresolvedID, state); err != nil {
			t.Fatal(err)
		}
		status, raw, _ := h.gatewayRaw(http.MethodPost, "/v1/batches", key, bytes.NewReader(serverFailureSource), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusConflict || fixture.dials.Load() != beforeRetry {
			t.Fatalf("unmapped %s intent lost duplicate-dispatch protection: %d %s", state, status, raw)
		}
	}

}

func openAIBatchReader(h *accessHarness) *batches.OpenAIReader {
	return &batches.OpenAIReader{Auth: connectors.NewAuth(&h.Policy), Egress: &h.Policy}
}

func batchWorker(h *accessHarness, reader batches.Reader) *batches.Worker {
	return &batches.Worker{
		Pool: h.Pool, Store: h.Gateway.Batches, Resources: h.Gateway.Resources,
		Resolver: h.Gateway.Resolver, Credentials: h.Runtime, DigestKey: h.AuthKey,
		Auth: connectors.NewAuth(&h.Policy), Egress: &h.Policy,
		Client: h.Policy.Client(5 * time.Minute), Connections: egress.NewConnectionClientCache(16),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Readers: map[string]batches.Reader{
			"openai": reader, "together": batches.NewTogetherReader(connectors.NewAuth(&h.Policy), &h.Policy),
			"mistral": batches.NewMistralReader(connectors.NewAuth(&h.Policy), &h.Policy),
		},
	}
}

func uploadBatchInputForSettlement(t *testing.T, h *accessHarness, key, route string, customIDs ...string) string {
	t.Helper()
	var source strings.Builder
	for _, id := range customIDs {
		source.WriteString(nativeBatchInput(id))
	}
	status, uploaded := h.uploadTestFile(route, key, source.String())
	if status != http.StatusOK {
		t.Fatalf("batch input upload: %d %v", status, uploaded)
	}
	return uploaded["id"].(string)
}

func createBatchForSettlement(t *testing.T, h *accessHarness, key, route, inputFileID string) string {
	t.Helper()
	status, raw, _ := h.gatewayRaw("POST", "/v1/batches", key, bytes.NewReader(batchCreateBody(inputFileID)),
		map[string]string{"Content-Type": "application/json", "X-OLP-Route": route})
	if status != http.StatusOK {
		t.Fatalf("create batch: %d %s", status, raw)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	localID, _ := body["id"].(string)
	if !strings.HasPrefix(localID, "batch_") {
		t.Fatalf("batch response lacks gateway identifier: %v", body)
	}
	return localID
}

func batchCreateBody(inputFileID string) []byte {
	body, _ := json.Marshal(map[string]any{
		"completion_window": "24h",
		"input_file_id":     inputFileID,
		"endpoint":          "/v1/chat/completions",
	})
	return body
}

func batchResourceID(t *testing.T, localID string) uuid.UUID {
	t.Helper()
	raw := strings.TrimPrefix(localID, "batch_")
	if len(raw) != 32 {
		t.Fatalf("invalid local batch ID %q", localID)
	}
	id, err := uuid.Parse(raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:])
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func batchUpstreamID(t *testing.T, h *accessHarness, localID string) string {
	t.Helper()
	var upstream string
	if err := h.Pool.QueryRow(t.Context(), `SELECT upstream_id FROM olp.batch_settlements WHERE resource_id=$1`, batchResourceID(t, localID)).Scan(&upstream); err != nil {
		t.Fatal(err)
	}
	return upstream
}

func setBatchFixtureResults(t *testing.T, fixture *openaiFixture, upstreamID, status string, expected int64,
	outputID, errorID, output, errorLines string) {
	t.Helper()
	fixture.mu.Lock()
	batch, ok := fixture.batches[upstreamID]
	if !ok {
		fixture.mu.Unlock()
		t.Fatalf("fixture has no batch %s", upstreamID)
	}
	batch["status"] = status
	batch["request_counts"] = map[string]any{"total": expected, "completed": int64(0), "failed": int64(0)}
	if outputID != "" {
		batch["output_file_id"] = outputID
		fixture.files[outputID] = map[string]any{"id": outputID, "object": "file", "bytes": len(output), "created_at": time.Now().Unix(), "filename": "output.jsonl", "purpose": "batch_output", "status": "processed"}
	} else {
		delete(batch, "output_file_id")
	}
	if errorID != "" {
		batch["error_file_id"] = errorID
		fixture.files[errorID] = map[string]any{"id": errorID, "object": "file", "bytes": len(errorLines), "created_at": time.Now().Unix(), "filename": "error.jsonl", "purpose": "batch_error", "status": "processed"}
	} else {
		delete(batch, "error_file_id")
	}
	fixture.mu.Unlock()
	if outputID != "" {
		fixture.contentByID.Store(outputID, output)
	}
	if errorID != "" {
		fixture.contentByID.Store(errorID, errorLines)
	}
}

func batchChatLine(customID, text string) string {
	body, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-result", "object": "chat.completion", "created": 1, "model": vendorModel,
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": text}, "finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10},
	})
	line, _ := json.Marshal(map[string]any{
		"custom_id": customID, "response": map[string]any{"status_code": 200, "request_id": "fixture-request", "body": json.RawMessage(body)},
	})
	return string(line) + "\n"
}

func batchErrorLine(customID, code string) string {
	line, _ := json.Marshal(map[string]any{"custom_id": customID, "error": map[string]any{"code": code, "message": "fixture error"}})
	return string(line) + "\n"
}

func addBatchPrice(t *testing.T, h *accessHarness, owner *browser, providerID string, multiplier string, effective time.Time) {
	t.Helper()
	price := map[string]any{
		"provider_kind": "azure_openai", "provider_id": providerID, "model": vendorModel,
		"operation": "generation", "currency": "USD",
		"input_per_million": "1000000", "output_per_million": "1000000",
	}
	if multiplier != "" {
		price["batch_multiplier"] = multiplier
	}
	h.want(owner, http.MethodPost, "/api/v1/pricing/revisions", map[string]any{
		"effective_at": effective.UTC().Format(time.RFC3339Nano), "prices": []any{price},
	}, map[string]string{"Idempotency-Key": uuid.NewString()}, http.StatusCreated)
	h.refresh()
}

func assertBatchSettlement(t *testing.T, h *accessHarness, resourceID uuid.UUID, state string, settled, uncertain int64) {
	t.Helper()
	var gotState string
	var gotSettled, gotUncertain int64
	err := h.Pool.QueryRow(t.Context(), `SELECT state,settled_results,uncertain_results
		FROM olp.batch_settlements WHERE resource_id=$1`, resourceID).Scan(&gotState, &gotSettled, &gotUncertain)
	if err != nil || gotState != state || gotSettled != settled || gotUncertain != uncertain {
		t.Fatalf("batch settlement = %s/%d/%d err=%v, want %s/%d/%d", gotState, gotSettled, gotUncertain, err, state, settled, uncertain)
	}
}

func batchFactTotals(t *testing.T, h *accessHarness, resourceID uuid.UUID) (facts, batchFacts, unpricedFacts int64, cost string) {
	t.Helper()
	err := h.Pool.QueryRow(t.Context(), `SELECT count(*),count(*) FILTER(WHERE f.batch),
		count(*) FILTER(WHERE f.unpriced),COALESCE(sum(COALESCE(f.estimated_cost,0)),0)::text
		FROM olp.batch_settlement_results r JOIN olp.attempt_usage_facts f ON f.event_id=r.event_id
		WHERE r.resource_id=$1`, resourceID).Scan(&facts, &batchFacts, &unpricedFacts, &cost)
	if err != nil {
		t.Fatal(err)
	}
	return
}
