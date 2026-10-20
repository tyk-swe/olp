package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/batches"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

const defaultBatchCompletionWindow = 24 * time.Hour

type batchSubmission struct {
	settlement *batches.Settlement
	resource   *resources.Resource
}

func batchLineOperation(source oif.Document) (string, *Error) {
	value, present := source.Root().Lookup("endpoint")
	endpoint, valid := value.Text()
	if !present || !valid {
		param := "endpoint"
		return "", invalidRequest("unsupported_parameter", "The batch endpoint must be a supported operation.", &param)
	}
	switch endpoint {
	case "/v1/chat/completions", "/v1/responses", "/v1/completions":
		return "generation", nil
	case "/v1/embeddings":
		return "embeddings", nil
	default:
		param := "endpoint"
		return "", invalidRequest("unsupported_parameter", "The batch endpoint must be a supported operation.", &param)
	}
}

func batchCompletionWindow(source oif.Document, vendor string) (time.Duration, *Error) {
	value, present := source.Root().Lookup("completion_window")
	if !present {
		return defaultBatchCompletionWindow, nil
	}
	raw, valid := value.Text()
	if !valid {
		param := "completion_window"
		return 0, invalidRequest("invalid_value", "completion_window must be a valid duration.", &param)
	}
	if len(raw) < 2 {
		return 0, invalidRequest("invalid_value", "completion_window must use whole hours or days.", strPtr("completion_window"))
	}
	unit := raw[len(raw)-1]
	if unit != 'h' && unit != 'd' {
		return 0, invalidRequest("invalid_value", "completion_window must use whole hours or days.", strPtr("completion_window"))
	}
	n, err := strconv.Atoi(raw[:len(raw)-1])
	if err != nil || n <= 0 || unit == 'h' && n > 168 || unit == 'd' && n > 7 {
		param := "completion_window"
		return 0, invalidRequest("invalid_value", "completion_window must use whole hours or days.", &param)
	}
	hours := n
	if unit == 'd' {
		hours *= 24
	}
	minimum, maximum := 24, 24
	if vendor == "groq" {
		maximum = 168
	} else if vendor == "mistral" {
		minimum, maximum = 1, 168
	}
	if hours < minimum || hours > maximum {
		param := "completion_window"
		return 0, invalidRequest("invalid_value", "completion_window is outside the provider's supported range.", &param)
	}
	return time.Duration(hours) * time.Hour, nil
}

func (s *Server) prepareBatchSubmission(ctx context.Context, x *call, authorityID string, p *pin, source oif.Document,
	canonical, effective []byte, serving oif.ServingIdentity,
	localFileID, upstreamFileID, resourceKind, adapter, surface, lineOperation string, window time.Duration, expected *int64, reconciliationCursor json.RawMessage,
) (*batchSubmission, *Error) {
	return s.prepareBatchSubmissionWithID(ctx, x, authorityID, p, source, canonical, effective, serving,
		localFileID, upstreamFileID, resourceKind, adapter, surface, lineOperation, window, expected, reconciliationCursor, uuid.Nil)
}

func (s *Server) prepareBatchSubmissionWithID(ctx context.Context, x *call, authorityID string, p *pin, source oif.Document,
	canonical, effective []byte, serving oif.ServingIdentity,
	localFileID, upstreamFileID, resourceKind, adapter, surface, lineOperation string, window time.Duration, expected *int64, reconciliationCursor json.RawMessage, resourceID uuid.UUID,
) (*batchSubmission, *Error) {
	if s.Batches == nil {
		return nil, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "Batch settlement is not configured.")
	}
	digester, ok := s.Runtime.(interface {
		Digest(secrets.DigestPurpose, string) []byte
	})
	if !ok {
		return nil, serverError(http.StatusServiceUnavailable, "authority_unavailable", "Batch submission identity is unavailable.")
	}
	if resourceID == uuid.Nil {
		resourceID = uuid.Must(uuid.NewV7())
	}
	now := s.now()
	template, err := s.batchSettlementTemplate(x, p, surface, lineOperation, now)
	if err != nil {
		return nil, serverError(http.StatusServiceUnavailable, "accounting_unavailable", "Batch usage accounting could not be prepared.")
	}
	identity, err := json.Marshal([]string{authorityID, x.route.Slug, surface, resourceKind, lineOperation, string(canonical)})
	if err != nil {
		return nil, serverError(http.StatusInternalServerError, "internal_error", "Batch submission identity could not be encoded.")
	}
	digest := digester.Digest(secrets.BatchSubmissionDigest, string(identity))
	cursor := reconciliationCursor
	if len(cursor) == 0 {
		endpointValue, _ := source.Root().Lookup("endpoint")
		endpoint, _ := endpointValue.Text()
		windowLabel := "24h"
		if value, present := source.Root().Lookup("completion_window"); present {
			windowLabel, _ = value.Text()
		}
		cursor, err = json.Marshal(map[string]any{
			"submission": map[string]string{
				"input_file_id":       upstreamFileID,
				"local_input_file_id": localFileID,
				"endpoint":            endpoint,
				"completion_window":   windowLabel,
			},
		})
		if err != nil {
			return nil, serverError(http.StatusInternalServerError, "internal_error", "Batch submission identity could not be stored.")
		}
	}
	closeAfter := now.Add(window + 48*time.Hour)
	resource := &resources.Resource{
		UUID: resourceID, Kind: resourceKind, APIKeyID: authorityID, RouteSlug: x.route.Slug,
		ProviderID: p.provider.ID, ProviderRevisionID: p.provider.RevisionID,
		RouteRevisionID: x.route.RevisionID, TargetID: p.target.ID, SlotID: p.slot.ID,
		CredentialID: p.slot.CredentialID, Metadata: json.RawMessage(`{}`),
	}
	if resourceKind == resources.KindStrictBatch {
		version := resources.DurableContractVersion
		resource.ContractVersion = &version
		resource.ExpiresAt = s.durableExpiry(nil)
		resource.Metadata = durableMetadata(p.model)
	}
	settlement := &batches.Settlement{
		ResourceID: resourceID, Kind: resourceKind, Adapter: adapter, APIKeyID: authorityID,
		RouteSlug: x.route.Slug, RouteRevisionID: x.route.RevisionID, ProviderID: p.provider.ID,
		ProviderRevisionID: p.provider.RevisionID, TargetID: p.target.ID, SlotID: p.slot.ID,
		CredentialID: p.slot.CredentialID, Surface: surface, SubmissionDigest: digest,
		LineOperation: lineOperation, Template: template, State: batches.StateSubmitting, ExpectedResults: expected,
		Cursor: cursor, CloseAfter: closeAfter,
	}
	if err = s.Batches.BeginSubmission(ctx, settlement); err != nil {
		if errors.Is(err, batches.ErrSubmissionInFlight) {
			return nil, serverError(http.StatusConflict, "submission_unknown",
				"An identical batch submission has an unknown outcome; list batches before retrying.")
		}
		return nil, serverError(http.StatusServiceUnavailable, "provider_state_unavailable", "Batch submission intent could not be stored.")
	}
	if resourceKind == resources.KindStrictBatch {
		payload, marshalErr := json.Marshal(durableDocument{
			Version: *resource.ContractVersion, Source: source.Bytes(), Effective: effective,
			Binding: p.model, Serving: serving,
		})
		if marshalErr != nil {
			_ = s.Batches.DeleteSubmission(ctx, resourceID)
			return nil, durableError(marshalErr)
		}
		if err = s.Resources.StageDurableContract(ctx, resourceID, payload, resource.ExpiresAt); err != nil {
			_ = s.Batches.DeleteSubmission(ctx, resourceID)
			return nil, serverError(http.StatusInternalServerError, "internal_error", "The native batch contract could not be staged.")
		}
	}
	return &batchSubmission{settlement: settlement, resource: resource}, nil
}

func (s *Server) batchSettlementTemplate(x *call, p *pin, surface, lineOperation string, now time.Time) (*usage.Event, error) {
	if x == nil || x.request.release == nil || x.route == nil || p == nil {
		return nil, errors.New("batch accounting identity is incomplete")
	}
	attemptCount, attemptVendor, routingInputs := x.attemptCount, x.attemptVendor, x.routingInputs
	fact := s.newFact(x, p.attempt, p.slot, 1, &p.provider)
	x.attemptCount, x.attemptVendor, x.routingInputs = attemptCount, attemptVendor, routingInputs
	fact.StartedAt = now
	fact.Status = http.StatusOK
	fact.Class = classSuccess
	fact.Committed = true
	fact.UsageObserved = false
	fact.UsageComplete = false
	fact.BillingUncertain = true
	if fact.Interaction != nil {
		fact.Interaction.UpstreamState = usage.UpstreamAccepted
		fact.Interaction.ClientState = usage.ClientUnobserved
	}
	if p.hold != nil {
		fact.Budgets = append([]string(nil), p.hold.budgets...)
	}
	if inputs := s.Runtime.RoutingInputs(); inputs != nil {
		fact.Price = inputs.Price(p.provider.Kind, p.provider.ID, p.provider.VendorID, p.model, lineOperation, now)
		fact.Discount = inputs.Discount(p.provider.Kind, p.provider.ID, p.provider.VendorID, now)
	} else {
		fact.Price = nil
		fact.Discount = nil
	}
	fact.PricingPinned = true
	fact.CardPinned = true
	fact.CardRevisionID = nil
	if fact.cardRev != nil {
		fact.CardRevisionID = &fact.cardRev.ID
	}
	accountingID := x.request.accountingID()
	event := accountingEvent(Envelope{
		RequestID: accountingID, AccountingID: accountingID, KeyID: x.keyID,
		EndUserDigest: x.endUserDigest, Origin: usage.OriginBatchSettlement, ParentRequestID: accountingID,
		BudgetGroupID: x.budgetGroupID, Attribution: x.attribution,
		EvaluatorGuardrailID: x.evaluatorGuardrailID, GuardrailDecisions: x.guardrailEvidence(),
		PolicyDecisions: x.policyDecisions, Route: x.route.Slug, RouteRevisionID: x.route.RevisionID,
		Operation: lineOperation, Surface: surface, RuntimeGenerationID: x.request.release.Snapshot.Generation.ID,
		StartedAt: now, CompletedAt: now, Attempts: []AttemptFact{fact},
	})
	if event == nil {
		return nil, errors.New("batch usage event is unavailable")
	}
	if p.provider.ProfileID == "bedrock-batch-inference" {
		uncertain := aggregateTariffNeedsPerRecordUsage(fact.Price) ||
			aggregateTariffNeedsPerRecordUsage(fact.cardRev.Explicit(p.provider.Kind, p.provider.VendorID, p.model, lineOperation))
		var policy map[string]json.RawMessage
		_ = json.Unmarshal(*event.Attempts[0].Routing.Policy, &policy)
		policy["batch_aggregate_pricing_uncertain"], _ = json.Marshal(uncertain)
		encoded, _ := json.Marshal(policy)
		raw := json.RawMessage(encoded)
		event.Attempts[0].Routing.Policy = &raw
	}
	if _, err := usage.Validate(event); err != nil {
		return nil, err
	}
	return event, nil
}

// A manifest supplies only aggregate text-token totals. Per-invocation fees,
// tiers and specialized meters cannot be reconstructed from those totals.
func aggregateTariffNeedsPerRecordUsage(price *usage.RoutingPrice) bool {
	if price == nil {
		return false
	}
	if len(price.Tiers) > 0 || len(price.ToolCalls) > 0 {
		return true
	}
	for _, rate := range []*string{
		price.PerRequest, price.UnitPrice, price.CachedInputPerMillion,
		price.CacheWriteInputPerMillion, price.CacheWrite5MInputPerMillion, price.CacheWrite1HInputPerMillion,
		price.AudioInputPerMillion, price.CachedAudioInputPerMillion, price.AudioOutputPerMillion,
		price.ImageInputPerMillion, price.CachedImageInputPerMillion, price.ImageOutputPerMillion,
	} {
		if rate != nil && strings.Trim(*rate, "0.") != "" {
			return true
		}
	}
	return false
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

func (s *Server) batchSubmissionUnknown(ctx context.Context, resourceID uuid.UUID, message string) *Error {
	commitCtx, stop := resourceCommitContext(ctx)
	defer stop()
	if err := s.Batches.MarkUnknown(commitCtx, resourceID, "submission_unknown", s.now()); err != nil {
		s.log.Warn("batch submission outcome could not be recorded", "resource_id", resourceID, "error", err)
	}
	return serverError(http.StatusBadGateway, "submission_unknown", message)
}
