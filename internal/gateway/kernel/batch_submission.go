package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/usage"
)

const DefaultBatchCompletionWindow = 24 * time.Hour

type BatchIntent struct {
	ResourceID uuid.UUID
	Kind       string
	Adapter    connectors.BatchAdapter
	Surface    batches.Surface
	Operation  wire.LineOperation
	Window     time.Duration
	Expected   *int64
	Submission batches.Submission
	Source     oif.Document
	Canonical  []byte
	Effective  []byte
	serving    oif.ServingIdentity
}

type batchSubmission struct {
	Resource resources.Resource
	expected *int64
}

type AcceptedBatch struct {
	UpstreamID string
	State      string
	Metadata   json.RawMessage
	expected   *int64
	contract   []byte
}

type batchWindow struct {
	label  string
	length time.Duration
}

var errUnsupportedBatchEndpoint = fault.InvalidRequest("unsupported_parameter", "The batch endpoint must be a supported operation.", new("endpoint"))

func batchLineOperation(source oif.Document) (string, wire.LineOperation, *fault.Error) {
	value, present := source.Root().Lookup("endpoint")
	endpoint, valid := value.Text()
	if !present || !valid {
		return "", "", errUnsupportedBatchEndpoint
	}
	operation, ok := wire.BatchLineOperation(endpoint)
	if !ok {
		return "", "", errUnsupportedBatchEndpoint
	}
	return endpoint, operation, nil
}

func batchCompletionWindow(source oif.Document, vendor string) (batchWindow, *fault.Error) {
	value, present := source.Root().Lookup("completion_window")
	if !present {
		return batchWindow{label: "24h", length: DefaultBatchCompletionWindow}, nil
	}
	raw, valid := value.Text()
	if !valid {
		return batchWindow{}, fault.InvalidRequest("invalid_value", "completion_window must be a valid duration.", new("completion_window"))
	}
	hours, ok := windowHours(raw)
	if !ok {
		return batchWindow{}, fault.InvalidRequest("invalid_value", "completion_window must use whole hours or days.", new("completion_window"))
	}
	minimum, maximum := completionWindowRange(vendor)
	if hours < minimum || hours > maximum {
		return batchWindow{}, fault.InvalidRequest("invalid_value", "completion_window is outside the provider's supported range.", new("completion_window"))
	}
	return batchWindow{label: raw, length: time.Duration(hours) * time.Hour}, nil
}

func windowHours(raw string) (int, bool) {
	if len(raw) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(raw[:len(raw)-1])
	if err != nil || n <= 0 {
		return 0, false
	}
	switch raw[len(raw)-1] {
	case 'h':
		return n, n <= 168
	case 'd':
		return n * 24, n <= 7
	default:
		return 0, false
	}
}

func completionWindowRange(vendor string) (int, int) {
	switch vendor {
	case "groq":
		return 24, 168
	case "mistral":
		return 1, 168
	default:
		return 24, 24
	}
}

func (s *Server) BeginBatch(ctx context.Context, x *Call, authorityID string, p *Pin, intent BatchIntent) (batchSubmission, *fault.Error) {
	if s.Deps.Batches == nil {
		return batchSubmission{}, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Batch settlement is not configured.")
	}
	if p.Provider.CredentialSource == "caller" {
		return batchSubmission{}, PolicyUnavailable("target_capability", "Batch submissions require gateway-managed credentials so settlement can run after the request ends.")
	}
	digester := s.Deps.Digester
	if digester == nil {
		return batchSubmission{}, fault.ServerError(http.StatusServiceUnavailable, "authority_unavailable", "Batch submission identity is unavailable.")
	}
	now := s.Now()
	template, err := s.settlementTemplate(x, p, string(intent.Surface), string(intent.Operation), now)
	if err != nil {
		return batchSubmission{}, fault.ServerError(http.StatusServiceUnavailable, "accounting_unavailable", "Batch usage accounting could not be prepared.")
	}
	canonical, err := canonicalBatchJSON(intent.Canonical)
	if err != nil {
		return batchSubmission{}, fault.InvalidRequest("invalid_json", "The batch submission must be valid JSON.", nil)
	}
	identity, err := json.Marshal([]string{authorityID, x.Route.Slug, string(intent.Surface), intent.Kind, string(intent.Operation), string(canonical)})
	if err != nil {
		return batchSubmission{}, fault.ServerError(http.StatusInternalServerError, "internal_error", "Batch submission identity could not be encoded.")
	}
	resource := s.batchResource(x, authorityID, p, intent)
	err = s.Deps.Batches.BeginSubmission(ctx, batches.Intent{
		ResourceID: resource.UUID, Adapter: intent.Adapter, Binding: batchBinding(resource), Surface: intent.Surface,
		SubmissionDigest: digester.Digest(secrets.BatchSubmissionDigest, string(identity)), LineOperation: intent.Operation,
		Template: template, ExpectedResults: intent.Expected, Submission: intent.Submission,
		CreatorDeadline: creatorDeadline(ctx, now, x.Route.OverallTimeout), CloseAfter: now.Add(intent.Window + 48*time.Hour),
	})
	if errors.Is(err, batches.ErrSubmissionInFlight) {
		return batchSubmission{}, fault.ServerError(http.StatusConflict, "submission_unknown",
			"An identical batch submission has an unknown outcome; list batches before retrying.")
	}
	if err != nil {
		return batchSubmission{}, fault.ServerError(http.StatusServiceUnavailable, "provider_state_unavailable", "Batch submission intent could not be stored.")
	}
	submission := batchSubmission{Resource: resource, expected: intent.Expected}
	if intent.Kind != resources.KindStrictBatch {
		return submission, nil
	}
	payload, err := json.Marshal(durableDocument{
		Version: *resource.ContractVersion, Source: intent.Source.Bytes(), Effective: intent.Effective,
		Binding: p.Model, Serving: intent.serving,
	})
	if err != nil {
		s.abandonBatch(ctx, resource.UUID)
		return batchSubmission{}, durableError(err)
	}
	if err = s.Deps.Resources.StageDurableContract(ctx, resource.UUID, payload, resource.ExpiresAt); err != nil {
		s.abandonBatch(ctx, resource.UUID)
		return batchSubmission{}, fault.ServerError(http.StatusInternalServerError, "internal_error", "The native batch contract could not be staged.")
	}
	return submission, nil
}

func (s *Server) batchResource(x *Call, authorityID string, p *Pin, intent BatchIntent) resources.Resource {
	id := intent.ResourceID
	if id == uuid.Nil {
		id = uuid.Must(uuid.NewV7())
	}
	resource := resources.Resource{
		UUID: id, Kind: intent.Kind, APIKeyID: authorityID, RouteSlug: x.Route.Slug,
		ProviderID: p.Provider.ID, ProviderRevisionID: p.Provider.RevisionID,
		RouteRevisionID: x.Route.RevisionID, TargetID: p.Target.ID, SlotID: p.Slot.ID,
		CredentialID: p.Slot.CredentialID, Metadata: json.RawMessage(`{}`),
	}
	if intent.Kind != resources.KindStrictBatch {
		return resource
	}
	version := resources.DurableContractVersion
	resource.ContractVersion, resource.ExpiresAt, resource.Metadata = &version, s.durableExpiry(nil), durableMetadata(p.Model)
	return resource
}

func batchBinding(resource resources.Resource) batches.Binding {
	return batches.Binding{
		Kind: resource.Kind, APIKeyID: resource.APIKeyID, RouteSlug: resource.RouteSlug,
		RouteRevisionID: resource.RouteRevisionID, ProviderID: resource.ProviderID,
		ProviderRevisionID: resource.ProviderRevisionID, TargetID: resource.TargetID,
		SlotID: resource.SlotID, CredentialID: resource.CredentialID,
	}
}

func creatorDeadline(ctx context.Context, now time.Time, overallTimeoutMillis int64) time.Time {
	deadline := now.Add(time.Duration(overallTimeoutMillis) * time.Millisecond)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		return ctxDeadline.Add(resourceCommitTimeout)
	}
	return deadline.Add(resourceCommitTimeout)
}

func (s *Server) abandonBatch(ctx context.Context, resourceID uuid.UUID) {
	commitCtx, stop := ResourceCommitContext(ctx)
	defer stop()
	if err := s.Deps.Batches.DeleteSubmission(commitCtx, resourceID); err != nil {
		s.log.Warn("batch submission intent could not be removed", "resource_id", resourceID, "error", err)
	}
}

func (s *Server) SendBatch(ctx context.Context, x *Call, p *Pin, resourceID uuid.UUID, endpoint string, body []byte) ([]byte, *PinnedExchange, *fault.Error) {
	resp, failure := s.PinnedDo(ctx, x, p, http.MethodPost, endpoint, body, "application/json")
	if failure != nil {
		if failure.acceptance.Unresolved() {
			return nil, nil, s.BatchSubmissionUnknown(ctx, resourceID, "The provider may have accepted this batch, but its outcome is unknown.")
		}
		s.abandonBatch(ctx, resourceID)
		return nil, nil, UpstreamError(failure)
	}
	defer resp.Body.Close()

	result, err := resp.ReadBody(s.Config.MaxResponseBytes)
	if err != nil {
		return nil, nil, s.BatchSubmissionUnknown(ctx, resourceID, "The provider response could not be read. The batch may have been accepted.")
	}
	return result, resp.Exchange, nil
}

func (s *Server) ActivateBatch(ctx context.Context, submission batchSubmission, accepted AcceptedBatch) (*resources.Resource, *fault.Error) {
	commitCtx, stop := ResourceCommitContext(ctx)
	defer stop()
	stored, err := s.commitAcceptedBatch(commitCtx, submission, accepted)
	if err == nil {
		return stored, nil
	}
	if markErr := s.Deps.Batches.MarkUnknown(commitCtx, submission.Resource.UUID, batches.ErrorSubmissionCommitUnavailable, s.Now()); markErr != nil {
		s.log.Warn("accepted batch intent could not be marked unknown", "resource_id", submission.Resource.UUID, "error", markErr)
	}
	return nil, fault.ServerError(http.StatusBadGateway, "submission_unknown", "The provider accepted the batch, but its local state could not be confirmed.")
}

func (s *Server) commitAcceptedBatch(ctx context.Context, submission batchSubmission, accepted AcceptedBatch) (*resources.Resource, error) {
	resource := submission.Resource
	resource.UpstreamID, resource.State = accepted.UpstreamID, accepted.State
	if accepted.Metadata != nil {
		resource.Metadata = accepted.Metadata
	}
	tx, err := s.Deps.Resources.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	stored, err := s.putAcceptedBatch(ctx, tx, &resource, accepted.contract)
	if err != nil {
		return nil, err
	}
	expected := submission.expected
	if expected == nil {
		expected = accepted.expected
	}
	if err = s.Deps.Batches.ActivateTx(ctx, tx, resource.UUID, accepted.UpstreamID, expected); err != nil {
		return nil, err
	}
	return stored, tx.Commit(ctx)
}

func (s *Server) putAcceptedBatch(ctx context.Context, tx pgx.Tx, resource *resources.Resource, contract []byte) (*resources.Resource, error) {
	if contract != nil {
		return s.Deps.Resources.PutDurableContractTx(ctx, tx, resource, contract)
	}
	return s.Deps.Resources.PutTx(ctx, tx, resource)
}

func (s *Server) RequireBatchEvidenceSettled(ctx context.Context, resourceID uuid.UUID) *fault.Error {
	if s.Deps.Batches == nil {
		return fault.ServerError(http.StatusServiceUnavailable, "accounting_unavailable", "Batch settlement state is unavailable.")
	}
	pending, err := s.Deps.Batches.Pending(ctx, resourceID)
	if err != nil {
		return fault.ServerError(http.StatusServiceUnavailable, "accounting_unavailable", "Batch settlement state could not be read.")
	}
	if pending {
		return fault.ServerError(http.StatusConflict, "batch_settlement_pending", "The resource is still needed for batch settlement; retry after settlement closes.")
	}
	return nil
}

func (s *Server) settlementTemplate(x *Call, p *Pin, surface, lineOperation string, now time.Time) (*usage.Event, error) {
	if x == nil || x.request.release == nil || x.Route == nil || p == nil {
		return nil, errors.New("batch accounting identity is incomplete")
	}
	if s.Deps.Runtime == nil {
		return nil, errors.New("batch pricing inputs are unavailable")
	}
	inputs := s.Deps.Runtime.RoutingInputs()
	if inputs == nil || now.Sub(inputs.RefreshedAt) > routinginputs.PerformanceMaxAge {
		return nil, errors.New("batch pricing inputs are unavailable or stale")
	}
	attemptCount, attemptVendor, routingInputs := x.attemptCount, x.attemptVendor, x.routingInputs
	fact := s.newFact(x, p.attempt, p.Slot, 1, &p.Provider)
	x.attemptCount, x.attemptVendor, x.routingInputs = attemptCount, attemptVendor, routingInputs
	pricingCall := Call{
		request: request{release: x.request.release}, authority: x.authority,
		cardPinned: x.cardPinned, cardRev: x.cardRev, routingInputs: inputs,
		routeLeg: routeLeg{Route: x.Route},
	}
	fact.CardRev = pricingCall.attemptCardRevision(p.Provider.ID, now)
	fact.StartedAt = now
	fact.Status = http.StatusOK
	fact.Class = ClassSuccess
	fact.Committed = true
	fact.UsageObserved = false
	fact.UsageComplete = false
	fact.BillingUncertain = true
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = upstream.Accepted
		fact.Interaction.ClientState = usage.ClientUnobserved
	}
	if p.Hold != nil {
		fact.Budgets = append([]string(nil), p.Hold.budgets...)
	}
	fact.Price = inputs.Price(string(p.Provider.Kind), p.Provider.ID, p.Provider.VendorID, p.Model, lineOperation, now)
	fact.Discount = inputs.Discount(string(p.Provider.Kind), p.Provider.ID, p.Provider.VendorID, now)
	fact.PricingPinned = true
	fact.CardPinned = true
	accountingID := x.request.accountingID()
	event, err := events.UsageEvent(events.Envelope{
		RequestID: accountingID, AccountingID: accountingID, KeyID: x.keyID,
		EndUserDigest: x.endUserDigest, Origin: usage.OriginBatchSettlement, ParentRequestID: accountingID,
		BudgetGroupID: x.budgetGroupID, Attribution: x.attribution,
		EvaluatorGuardrailID: x.evaluatorGuardrailID, GuardrailDecisions: x.guard.evidence(),
		PolicyDecisions: x.policyDecisions, Route: x.Route.Slug, RouteRevisionID: x.Route.RevisionID,
		Operation: lineOperation, Surface: surface, RuntimeGenerationID: x.request.release.Snapshot().Generation().ID,
		StartedAt: now, CompletedAt: now, Attempts: []events.AttemptFact{fact},
	})
	if err != nil {
		return nil, err
	}
	if event == nil {
		return nil, errors.New("batch usage event is unavailable")
	}
	if p.Provider.ProfileID == connectors.BedrockBatchProfile {
		uncertain := aggregateTariffNeedsPerRecordUsage(fact.Price) ||
			aggregateTariffNeedsPerRecordUsage(fact.CardRev.Explicit(string(p.Provider.Kind), p.Provider.VendorID, p.Model, lineOperation))
		policy, err := withPolicyFlag(*event.Attempts[0].Routing.Policy, "batch_aggregate_pricing_uncertain", uncertain)
		if err != nil {
			return nil, err
		}
		event.Attempts[0].Routing.Policy = &policy
	}
	if _, err := usage.Validate(event); err != nil {
		return nil, err
	}
	return event, nil
}

// A manifest supplies only aggregate text-token totals. Per-invocation fees,
// tiers and specialized meters cannot be reconstructed from those totals.
func aggregateTariffNeedsPerRecordUsage(price *rates.RoutingPrice) bool {
	if price == nil {
		return false
	}
	if len(price.Tiers) > 0 || len(price.ToolCalls) > 0 {
		return true
	}
	if price.PerRequest != nil && strings.Trim(*price.PerRequest, "0.") != "" {
		return true
	}
	for _, rate := range []*string{
		price.UnitPrice, price.CachedInputPerMillion,
		price.CacheWriteInputPerMillion, price.CacheWrite5MInputPerMillion, price.CacheWrite1HInputPerMillion,
		price.AudioInputPerMillion, price.CachedAudioInputPerMillion, price.AudioOutputPerMillion,
		price.ImageInputPerMillion, price.CachedImageInputPerMillion, price.ImageOutputPerMillion,
	} {
		if rate != nil {
			return true
		}
	}
	return false
}

func withPolicyFlag(policy json.RawMessage, name string, value bool) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(policy, &fields); err != nil {
		return nil, err
	}
	fields[name] = json.RawMessage(strconv.FormatBool(value))
	return json.Marshal(fields)
}

func batchExpectedResults(body []byte) *int64 {
	var response struct {
		RequestCounts *struct {
			Total *int64 `json:"total"`
		} `json:"request_counts"`
	}
	if json.Unmarshal(body, &response) != nil || response.RequestCounts == nil ||
		response.RequestCounts.Total == nil || *response.RequestCounts.Total < 0 {
		return nil
	}
	count := *response.RequestCounts.Total
	return &count
}

func (s *Server) BatchSubmissionUnknown(ctx context.Context, resourceID uuid.UUID, message string) *fault.Error {
	return s.markBatchUnknown(ctx, resourceID, fault.ServerError(http.StatusBadGateway, "submission_unknown", message))
}

func (s *Server) markBatchUnknown(ctx context.Context, resourceID uuid.UUID, failure *fault.Error) *fault.Error {
	commitCtx, stop := ResourceCommitContext(ctx)
	defer stop()
	if err := s.Deps.Batches.MarkUnknown(commitCtx, resourceID, batches.ErrorSubmissionUnknown, s.Now()); err != nil {
		s.log.Warn("batch submission outcome could not be recorded", "resource_id", resourceID, "error", err)
	}
	return failure
}
