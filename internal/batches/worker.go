package batches

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	WorkerConcurrency = 4
	WorkerBatchSize   = 16
	leaseHeartbeat    = 30 * time.Second
)

var (
	errResultConflict = errors.New("batch result conflict")
)

type Reader interface {
	Poll(context.Context, Settlement, *PinnedTarget) (BatchSnapshot, error)
	Reconcile(context.Context, Settlement, *PinnedTarget, string, string) (Reconciliation, error)
	ReadResults(context.Context, *PinnedTarget, BatchSnapshot, Cursor, ResultPageCommit) (Cursor, bool, error)
}

type Worker struct {
	Pool         *pgxpool.Pool
	Store        *Store
	Resources    *resources.Store
	Resolver     *resources.Resolver
	Credentials  runtime.Credentials
	Keys         *secrets.KeyRing
	Installation string
	DigestKey    *secrets.AuthKey
	Auth         *connectors.Auth
	Egress       *egress.Policy
	Client       *http.Client
	Connections  *egress.ConnectionClientCache
	Limiter      *limits.Limiter
	Log          *slog.Logger
	Readers      map[string]Reader
	Now          func() time.Time
}

type Pass struct {
	Claimed   int
	Settled   int
	Uncertain int
	Failed    int
}

type reconcileOutcome int

const (
	outcomeProgress reconcileOutcome = iota
	outcomeUncertain
	outcomeFailed
)

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Worker) logger() *slog.Logger {
	if w.Log != nil {
		return w.Log
	}
	return slog.Default()
}

func (w *Worker) setup() {
	if w.Store == nil && w.Pool != nil {
		w.Store = NewStore(w.Pool)
	}
	if w.Resources == nil && w.Pool != nil && w.Keys != nil {
		w.Resources = resources.NewEncrypted(w.Pool, w.Installation, w.Keys)
	}
	if w.Client == nil && w.Egress != nil {
		w.Client = w.Egress.Client(5 * time.Minute)
	}
	if w.Connections == nil {
		w.Connections = egress.NewConnectionClientCache(128)
	}
	if w.Readers == nil {
		w.Readers = map[string]Reader{}
	}
	if w.Readers["anthropic"] == nil {
		w.Readers["anthropic"] = &AnthropicReader{Auth: w.Auth, Egress: w.Egress}
	}
	if w.Readers["gemini"] == nil {
		w.Readers["gemini"] = &GeminiReader{Auth: w.Auth, Egress: w.Egress}
	}
	if w.Readers["openai"] == nil {
		w.Readers["openai"] = &OpenAIReader{Auth: w.Auth, Egress: w.Egress}
	}
	if w.Readers["vertex"] == nil {
		w.Readers["vertex"] = &VertexReader{Auth: w.Auth, Egress: w.Egress}
	}
	if w.Readers["bedrock"] == nil {
		w.Readers["bedrock"] = &BedrockReader{Auth: w.Auth, Egress: w.Egress}
	}
	if w.Readers["anthropic"] == nil {
		w.Readers["anthropic"] = &AnthropicReader{Auth: w.Auth, Egress: w.Egress}
	}
}

func (w *Worker) Run(ctx context.Context) {
	w.setup()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		pass, err := w.ReconcileOnce(ctx, WorkerBatchSize)
		outcome := usage.OutcomeSuccess
		if err != nil || pass != nil && pass.Failed > 0 {
			outcome = usage.OutcomeFailure
			if err != nil {
				w.logger().Warn("batch settlement pass failed", "error", err)
			}
		}
		if w.Pool != nil {
			if checkpointErr := usage.CheckpointTask(ctx, w.Pool, usage.TaskBatchSettlement, outcome, pass != nil && pass.Claimed > 0); checkpointErr != nil {
				w.logger().Debug("batch settlement checkpoint failed", "error", checkpointErr)
			}
		}
		if pass != nil && pass.Claimed > 0 {
			w.logger().Info("batch settlement pass completed",
				"claimed", pass.Claimed, "settled", pass.Settled,
				"uncertain", pass.Uncertain, "failed", pass.Failed)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) ReconcileOnce(ctx context.Context, limit int) (*Pass, error) {
	w.setup()
	pass := &Pass{}
	if w.Store == nil || w.Pool == nil {
		return pass, errors.New("batch settlement storage is unavailable")
	}
	if limit <= 0 || limit > WorkerBatchSize {
		limit = WorkerBatchSize
	}
	claims, err := w.Store.Claim(ctx, w.now(), limit)
	if err != nil {
		return pass, err
	}
	pass.Claimed = len(claims)
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, WorkerConcurrency)
	outcomes := make(chan reconcileOutcome, len(claims))
	for _, claimed := range claims {
		settlement := claimed
		wg.Go(func() {
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				outcomes <- outcomeFailed
				return
			}
			defer func() { <-semaphore }()
			outcomes <- w.reconcileClaimed(ctx, settlement)
		})
	}
	wg.Wait()
	close(outcomes)
	for outcome := range outcomes {
		switch outcome {
		case outcomeProgress:
			pass.Settled++
		case outcomeUncertain:
			pass.Uncertain++
		default:
			pass.Failed++
		}
	}
	return pass, nil
}

func (w *Worker) reconcileClaimed(ctx context.Context, settlement Settlement) reconcileOutcome {
	if settlement.ClaimID == nil {
		return outcomeFailed
	}
	if w.Readers[settlement.Adapter] == nil {
		return w.finishRetry(ctx, settlement, settlement.State, "batch_adapter_unavailable")
	}
	leaseCtx, stopLease := w.keepLease(ctx, settlement)
	defer stopLease()
	now := w.now()
	if settlement.State == StateSubmitting {
		next := settlement.CreatedAt.Add(2 * time.Minute)
		if !now.Before(next) {
			if err := w.Store.Finish(leaseCtx, settlement, StateSubmissionUnknown, now, "submission_interrupted"); err != nil {
				w.logFailure(settlement, "submission_interrupted", err)
				return outcomeFailed
			}
			return outcomeProgress
		}
		if err := w.Store.Finish(leaseCtx, settlement, StateSubmitting, next, ""); err != nil {
			w.logFailure(settlement, "submission_wait", err)
			return outcomeFailed
		}
		return outcomeProgress
	}
	if settlement.State == StateConflict {
		if !now.Before(settlement.CloseAfter) {
			return w.closeUncertain(leaseCtx, settlement, nil)
		}
		if err := w.Store.Finish(leaseCtx, settlement, StateConflict, settlement.CloseAfter, "result_conflict"); err != nil {
			w.logFailure(settlement, "result_conflict", err)
			return outcomeFailed
		}
		return outcomeProgress
	}
	target, errorClass := w.pinnedTarget(leaseCtx, settlement)
	if errorClass != "" {
		return w.failOrClose(leaseCtx, settlement, errorClass)
	}
	if settlement.State == StateSubmissionUnknown {
		return w.reconcileSubmission(leaseCtx, settlement, target)
	}
	return w.reconcileActive(leaseCtx, settlement, target, nil)
}

func (w *Worker) reconcileSubmission(ctx context.Context, settlement Settlement, target *PinnedTarget) reconcileOutcome {
	reader := w.Readers[settlement.Adapter]
	inputFileID, _, endpoint, err := submissionFiles(settlement.Cursor)
	if settlement.Adapter == "vertex" || settlement.Adapter == "bedrock" {
		inputFileID, err = "cloud-submission", nil
	}
	if err != nil || inputFileID == "" {
		if !w.now().Before(settlement.CloseAfter) {
			return w.closeUncertain(ctx, settlement, nil)
		}
		return w.finishRetry(ctx, settlement, StateSubmissionUnknown, "")
	}
	reconciliation, err := reader.Reconcile(ctx, settlement, target, inputFileID, endpoint)
	if err != nil {
		if errors.Is(err, ErrProviderConflict) {
			return w.finishConflict(ctx, settlement)
		}
		return w.failOrClose(ctx, settlement, "submission_reconcile_unavailable")
	}
	switch len(reconciliation.Matches) {
	case 0:
		if !w.now().Before(settlement.CloseAfter) {
			return w.closeUncertain(ctx, settlement, nil)
		}
		return w.finishPoll(ctx, settlement, StateSubmissionUnknown)
	case 1:
		snapshot := reconciliation.Matches[0]
		if err := w.adopt(ctx, settlement, snapshot); err != nil {
			return w.failOrClose(ctx, settlement, "submission_adoption_unavailable")
		}
		settlement.UpstreamID = &snapshot.ID
		settlement.ExpectedResults = snapshot.Expected
		settlement.State = StateActive
		return w.reconcileActive(ctx, settlement, target, &snapshot)
	default:
		if err := w.Store.Finish(ctx, settlement, StateConflict, settlement.CloseAfter, "submission_conflict"); err != nil {
			w.logFailure(settlement, "submission_conflict", err)
			return outcomeFailed
		}
		return outcomeProgress
	}
}

func (w *Worker) reconcileActive(ctx context.Context, settlement Settlement, target *PinnedTarget, known *BatchSnapshot) reconcileOutcome {
	reader := w.Readers[settlement.Adapter]
	if reader == nil {
		return w.finishRetry(ctx, settlement, settlement.State, "batch_adapter_unavailable")
	}
	var snapshot BatchSnapshot
	var err error
	if known != nil {
		snapshot = *known
	} else {
		snapshot, err = reader.Poll(ctx, settlement, target)
		if err != nil {
			if errors.Is(err, ErrProviderConflict) || errors.Is(err, ErrCursorConflict) {
				return w.finishConflict(ctx, settlement)
			}
			return w.failOrClose(ctx, settlement, "batch_poll_unavailable")
		}
	}
	if snapshot.Expected != nil && (settlement.ExpectedResults == nil || *settlement.ExpectedResults != *snapshot.Expected) {
		if err = w.Store.SetExpected(ctx, settlement, snapshot.Expected); err != nil {
			return w.failOrClose(ctx, settlement, "batch_checkpoint_unavailable")
		}
		settlement.ExpectedResults = snapshot.Expected
	}
	cursor := Cursor{}
	if len(settlement.Cursor) > 0 {
		if err = json.Unmarshal(settlement.Cursor, &cursor); err != nil {
			return w.finishConflict(ctx, settlement)
		}
	}
	exhausted := snapshot.OutputFileID == "" && snapshot.ErrorFileID == ""
	var progress bool
	if !exhausted {
		_, exhausted, err = reader.ReadResults(ctx, target, snapshot, cursor, func(results []Result, next Cursor) error {
			page := make([]PageResult, 0, len(results))
			for _, result := range results {
				if w.DigestKey == nil {
					return errors.New("batch digest key is unavailable")
				}
				page = append(page, PageResult{
					CustomIDDigest: CustomIDDigest(w.DigestKey, result.CustomID),
					Result:         result,
				})
			}
			snapshots, _, conflict, commitErr := w.Store.CommitPage(ctx, settlement, page, next, w.now())
			if commitErr != nil {
				return commitErr
			}
			progress = progress || len(results) > 0
			encoded, _ := json.Marshal(next)
			settlement.Cursor = encoded
			w.applySnapshots(ctx, snapshots)
			if conflict {
				return errResultConflict
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, ErrCursorConflict) || errors.Is(err, errResultConflict) {
				return w.finishConflict(ctx, settlement)
			}
			if errors.Is(err, ErrStorageScope) {
				return w.closeUncertainWithError(ctx, settlement, "storage_scope")
			}
			if errors.Is(err, ErrBatchTooLarge) || errors.Is(err, ErrLineTooLarge) {
				return w.closeUncertain(ctx, settlement, nil)
			}
			return w.failOrClose(ctx, settlement, "batch_result_read_unavailable")
		}
	}
	counts, err := w.Store.ResultCounts(ctx, settlement.ResourceID)
	if err != nil {
		return w.failOrClose(ctx, settlement, "batch_checkpoint_unavailable")
	}
	now := w.now()
	decision := CloseDecision(settlement.ExpectedResults, counts, false, snapshot.Terminal && exhausted, now, settlement.CloseAfter)
	if decision.State == StateSettled || decision.State == StateUncertain {
		snapshots, closeErr := w.Store.Close(ctx, settlement, decision.State, decision.Gap, w.gapDigest(), now)
		if closeErr != nil {
			w.logFailure(settlement, "batch_close_unavailable", closeErr)
			return outcomeFailed
		}
		w.applySnapshots(ctx, snapshots)
		if decision.State == StateUncertain {
			return outcomeUncertain
		}
		return outcomeProgress
	}
	state := StateActive
	if snapshot.Terminal || snapshot.OutputFileID != "" || snapshot.ErrorFileID != "" {
		state = StateSettling
	}
	if progress {
		settlement.Attempts = 0
	}
	next := now.Add(pollBackoff(settlement.Attempts))
	if err = w.Store.Finish(ctx, settlement, state, next, ""); err != nil {
		w.logFailure(settlement, "batch_checkpoint_unavailable", err)
		return outcomeFailed
	}
	return outcomeProgress
}

func (w *Worker) adopt(ctx context.Context, settlement Settlement, snapshot BatchSnapshot) error {
	if w.Resources == nil {
		return errors.New("provider resource store is unavailable")
	}
	inputFileID, localInputID := "", ""
	if settlement.Adapter != "vertex" && settlement.Adapter != "bedrock" {
		var err error
		inputFileID, localInputID, _, err = submissionFiles(settlement.Cursor)
		if err != nil {
			return err
		}
	}
	metadata, state := batchMetadata(snapshot.Body, resourceModel(settlement.Template), localInputID)
	if settlement.Adapter == "vertex" || settlement.Adapter == "bedrock" {
		var err error
		metadata, state, err = cloudAdoptMetadata(settlement, snapshot)
		if err != nil {
			return err
		}
	}
	if settlement.Adapter == "together" || settlement.Adapter == "mistral" {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(metadata, &fields)
		if fields == nil {
			fields = map[string]json.RawMessage{}
		}
		fields["adapter"], _ = json.Marshal(settlement.Adapter)
		fields["completion_window"], _ = json.Marshal(submissionWindow(settlement.Cursor))
		metadata, _ = json.Marshal(fields)
	}
	resource := &resources.Resource{
		UUID: settlement.ResourceID, Kind: settlement.Kind, APIKeyID: settlement.APIKeyID,
		RouteSlug: settlement.RouteSlug, ProviderID: settlement.ProviderID,
		ProviderRevisionID: settlement.ProviderRevisionID, RouteRevisionID: settlement.RouteRevisionID,
		TargetID: settlement.TargetID, SlotID: settlement.SlotID, CredentialID: settlement.CredentialID,
		UpstreamID: snapshot.ID, State: state, Metadata: metadata,
	}
	tx, err := w.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if settlement.Kind == resources.KindStrictBatch {
		payload, readErr := w.Resources.ReadStagedDurableContract(ctx, settlement.ResourceID)
		if readErr != nil {
			return readErr
		}
		var document stagedDurableDocument
		if json.Unmarshal(payload, &document) != nil || !strictAdoptionMatches(snapshot.Body, document.Effective, inputFileID) {
			return errors.New("strict batch reconciliation evidence is invalid")
		}
		document.Result = snapshot.Body
		payload, err = json.Marshal(document)
		if err != nil {
			return err
		}
		version := resources.DurableContractVersion
		resource.ContractVersion = &version
		expires := settlement.CreatedAt.Add(resources.DurableLifetime - time.Second)
		resource.ExpiresAt = &expires
		resource.Metadata, _ = json.Marshal(map[string]string{"upstream_model": resourceModel(settlement.Template)})
		_, err = w.Resources.PutDurableContractTx(ctx, tx, resource, payload)
	} else {
		_, err = w.Resources.PutTx(ctx, tx, resource)
	}
	if err != nil {
		return err
	}
	expected := snapshot.Expected
	if settlement.ExpectedResults != nil {
		expected = settlement.ExpectedResults
	}
	if err = w.Store.ActivateTx(ctx, tx, settlement.ResourceID, snapshot.ID, expected); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type stagedDurableDocument struct {
	Version   string          `json:"version"`
	Source    []byte          `json:"source"`
	Effective []byte          `json:"effective,omitempty"`
	Result    []byte          `json:"result,omitempty"`
	Binding   string          `json:"binding"`
	Serving   json.RawMessage `json:"serving"`
}

func strictAdoptionMatches(result, effective []byte, inputFileID string) bool {
	var batch struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		InputFileID string `json:"input_file_id"`
	}
	if json.Unmarshal(result, &batch) != nil || batch.ID == "" || batch.InputFileID != inputFileID ||
		batchStateRank(batch.Status) == 0 {
		return false
	}
	var request struct {
		InputFileID string `json:"input_file_id"`
	}
	return json.Unmarshal(effective, &request) == nil && request.InputFileID == inputFileID
}

func batchStateRank(state string) int {
	switch state {
	case "validating", "in_progress", "finalizing", "cancelling",
		"completed", "failed", "expired", "cancelled":
		return 1
	}
	return 0
}

func resourceModel(template *usage.Event) string {
	if template == nil || template.UpstreamModel == nil {
		return ""
	}
	return *template.UpstreamModel
}

func batchMetadata(body []byte, model, localInput string) (json.RawMessage, string) {
	metadata := map[string]json.RawMessage{}
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) == nil {
		for name, raw := range object {
			if name != "id" && name != "local_input_file_id" {
				metadata[name] = raw
			}
		}
	}
	if model != "" {
		metadata["upstream_model"], _ = json.Marshal(model)
	}
	if localInput != "" {
		metadata["local_input_file_id"], _ = json.Marshal(localInput)
	}
	state := "validating"
	if raw := metadata["status"]; len(raw) > 0 {
		var status string
		if json.Unmarshal(raw, &status) == nil && status != "" {
			state = status
		}
	}
	encoded, _ := json.Marshal(metadata)
	return encoded, state
}

func submissionFiles(cursor []byte) (inputFileID, localInputID, endpoint string, err error) {
	var value struct {
		Submission struct {
			InputFileID  string `json:"input_file_id"`
			LocalInputID string `json:"local_input_file_id"`
			Endpoint     string `json:"endpoint"`
			GeminiFile   string `json:"gemini_file"`
		} `json:"submission"`
	}
	if len(cursor) == 0 || json.Unmarshal(cursor, &value) != nil {
		return "", "", "", errors.New("batch submission identity is unavailable")
	}
	if value.Submission.GeminiFile != "" {
		return value.Submission.GeminiFile, "", "", nil
	}
	return value.Submission.InputFileID, value.Submission.LocalInputID, value.Submission.Endpoint, nil
}

func submissionWindow(cursor []byte) string {
	var value struct {
		Submission struct {
			CompletionWindow string `json:"completion_window"`
		} `json:"submission"`
	}
	if json.Unmarshal(cursor, &value) == nil && value.Submission.CompletionWindow != "" {
		return value.Submission.CompletionWindow
	}
	return "24h"
}

func (w *Worker) pinnedTarget(ctx context.Context, settlement Settlement) (*PinnedTarget, string) {
	if w.Resolver == nil || w.Egress == nil || w.Credentials == nil {
		return nil, "provider_resource_runtime_unavailable"
	}
	resource := &resources.Resource{
		Kind: settlement.Kind, APIKeyID: settlement.APIKeyID, RouteSlug: settlement.RouteSlug,
		ProviderID: settlement.ProviderID, ProviderRevisionID: settlement.ProviderRevisionID,
		RouteRevisionID: settlement.RouteRevisionID, TargetID: settlement.TargetID,
		SlotID: settlement.SlotID, CredentialID: settlement.CredentialID,
	}
	provider, route, slot, err := w.Resolver.ResolveCurrent(ctx, resource, "batch")
	if err != nil {
		if errors.Is(err, resources.ErrUnavailable) {
			return nil, "provider_resource_credential_unavailable"
		}
		return nil, "provider_resource_runtime_unavailable"
	}
	var model string
	for _, target := range route.Targets {
		if target.ID == settlement.TargetID && target.ProviderID == settlement.ProviderID {
			model = target.ProviderModel
			break
		}
	}
	if model == "" {
		return nil, "provider_resource_runtime_unavailable"
	}
	config := provider.Connector()
	if config.CredentialSource == "caller" {
		return nil, "provider_resource_credential_unavailable"
	}
	var secret []byte
	if slot.CredentialID != nil {
		if w.Credentials.Eligibility(*slot.CredentialID) != routing.Eligible {
			return nil, "provider_resource_credential_unavailable"
		}
		secret, _, err = w.Credentials.Secret(ctx, nil, *slot.CredentialID)
		if err != nil {
			return nil, "provider_resource_credential_unavailable"
		}
	} else if connectors.SecretRequired(config.AuthMode) {
		return nil, "provider_resource_credential_unavailable"
	}
	var networkSecret []byte
	if provider.Network != nil && provider.Network.CredentialID != "" {
		if w.Credentials.Eligibility(provider.Network.CredentialID) != routing.Eligible {
			return nil, "provider_resource_credential_unavailable"
		}
		networkSecret, err = w.Credentials.NetworkSecret(ctx, nil, provider.ID, provider.Network.CredentialID)
		if err != nil {
			return nil, "provider_resource_credential_unavailable"
		}
	}
	client := w.Client
	if provider.Network != nil || provider.ProfileID != "" {
		credentialID := "none"
		if slot.CredentialID != nil {
			credentialID = *slot.CredentialID
		}
		scope := provider.ID + "/" + provider.RevisionID + "/" + slot.ID + "/" + credentialID
		client, err = w.Connections.ClientScoped(scope, *w.Egress, provider.Network, networkSecret, 5*time.Minute)
		if err != nil {
			return nil, "provider_resource_runtime_unavailable"
		}
	}
	if client == nil {
		return nil, "provider_resource_runtime_unavailable"
	}
	return &PinnedTarget{Config: config, Model: model, Secret: secret, Client: client}, ""
}

func (w *Worker) keepLease(ctx context.Context, settlement Settlement) (context.Context, func()) {
	leaseCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(leaseHeartbeat)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case <-ticker.C:
				if err := w.Store.ExtendClaim(leaseCtx, settlement, w.now()); err != nil {
					w.logger().Warn("batch settlement lease renewal failed", "resource_id", settlement.ResourceID, "error", err)
					cancel()
					return
				}
			}
		}
	}()
	return leaseCtx, func() {
		cancel()
		<-done
	}
}

func (w *Worker) finishConflict(ctx context.Context, settlement Settlement) reconcileOutcome {
	now := w.now()
	if !now.Before(settlement.CloseAfter) {
		return w.closeUncertain(ctx, settlement, nil)
	}
	if err := w.Store.Finish(ctx, settlement, StateConflict, settlement.CloseAfter, "result_conflict"); err != nil {
		w.logFailure(settlement, "result_conflict", err)
		return outcomeFailed
	}
	return outcomeProgress
}

func (w *Worker) failOrClose(ctx context.Context, settlement Settlement, errorClass string) reconcileOutcome {
	if !w.now().Before(settlement.CloseAfter) {
		return w.closeUncertain(ctx, settlement, nil)
	}
	return w.finishRetry(ctx, settlement, settlement.State, errorClass)
}

func (w *Worker) finishRetry(ctx context.Context, settlement Settlement, state State, errorClass string) reconcileOutcome {
	next := w.now().Add(errorBackoff(settlement.Attempts))
	if err := w.Store.Finish(ctx, settlement, state, next, errorClass); err != nil {
		w.logFailure(settlement, errorClass, err)
		return outcomeFailed
	}
	if errorClass != "" {
		w.logger().Warn("batch settlement will retry", "resource_id", settlement.ResourceID, "error_class", errorClass)
		return outcomeFailed
	}
	return outcomeProgress
}

func (w *Worker) finishPoll(ctx context.Context, settlement Settlement, state State) reconcileOutcome {
	if err := w.Store.Finish(ctx, settlement, state, w.now().Add(pollBackoff(settlement.Attempts)), ""); err != nil {
		w.logFailure(settlement, "batch_poll_checkpoint", err)
		return outcomeFailed
	}
	return outcomeProgress
}

func (w *Worker) closeUncertain(ctx context.Context, settlement Settlement, counts ResultCounts) reconcileOutcome {
	if counts == nil {
		var err error
		counts, err = w.Store.ResultCounts(ctx, settlement.ResourceID)
		if err != nil {
			w.logFailure(settlement, "batch_close_unavailable", err)
			return outcomeFailed
		}
	}
	decision := CloseDecision(settlement.ExpectedResults, counts, settlement.State == StateConflict, false,
		w.now(), settlement.CloseAfter)
	state := decision.State
	gap := decision.Gap
	if state != StateSettled && gap == 0 {
		gap = 1
		if settlement.ExpectedResults != nil {
			gap = max(*settlement.ExpectedResults-counts.Observed(), 1)
		}
	}
	if state != StateSettled {
		state = StateUncertain
	}
	snapshots, err := w.Store.Close(ctx, settlement, state, gap, w.gapDigest(), w.now())
	if err != nil {
		w.logFailure(settlement, "batch_close_unavailable", err)
		return outcomeFailed
	}
	w.applySnapshots(ctx, snapshots)
	if state == StateUncertain {
		return outcomeUncertain
	}
	return outcomeProgress
}

func (w *Worker) closeUncertainWithError(ctx context.Context, settlement Settlement, errorClass string) reconcileOutcome {
	counts, err := w.Store.ResultCounts(ctx, settlement.ResourceID)
	if err != nil {
		w.logFailure(settlement, "batch_close_unavailable", err)
		return outcomeFailed
	}
	gap := int64(1)
	if settlement.ExpectedResults != nil {
		gap = max(*settlement.ExpectedResults-counts.Observed(), 1)
	}
	snapshots, err := w.Store.CloseWithErrorClass(ctx, settlement, StateUncertain, gap, w.gapDigest(), w.now(), errorClass)
	if err != nil {
		w.logFailure(settlement, errorClass, err)
		return outcomeFailed
	}
	w.applySnapshots(ctx, snapshots)
	return outcomeUncertain
}

func (w *Worker) gapDigest() []byte {
	if w.DigestKey == nil {
		return nil
	}
	return CustomIDDigest(w.DigestKey, "*")
}

func (w *Worker) applySnapshots(ctx context.Context, snapshots []limits.CostSnapshot) {
	if w.Limiter == nil {
		return
	}
	for _, snapshot := range snapshots {
		if _, _, err := w.Limiter.ApplyCostSnapshot(ctx, snapshot); err != nil {
			w.logger().Warn("batch cost snapshot application failed; reconciliation will repair it", "error", err)
		}
	}
}

func (w *Worker) logFailure(settlement Settlement, errorClass string, err error) {
	w.logger().Error("batch settlement checkpoint failed",
		"resource_id", settlement.ResourceID, "error_class", errorClass, "error", err)
}

func pollBackoff(attempts int) time.Duration {
	exponent := max(min(attempts-1, 9), 0)
	return min(30*time.Second*time.Duration(1<<exponent), 10*time.Minute)
}

func errorBackoff(attempts int) time.Duration {
	exponent := max(min(attempts-1, 6), 0)
	return min(5*time.Second*time.Duration(1<<exponent), 300*time.Second)
}
