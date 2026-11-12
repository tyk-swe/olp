package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage/delivery"
	"github.com/tyk-swe/olp/internal/usage/ingest"

	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

// AccountingSink hands every terminal request to the usage pipeline as the
// content-free event the accounting tables are built from. Emitting is a
// bounded handoff. A rejected event checkpoints its loss with a bounded write
// so other replicas can refuse admission against incomplete cost accounting.
type AccountingSink struct {
	Delivery *delivery.Delivery
	Next     Sink
}

// Terminal records one finished request.
func (a *AccountingSink) Terminal(e Envelope) {
	if a.Next != nil {
		a.Next.Terminal(e)
	}
	if a.Delivery == nil {
		return
	}
	event, err := UsageEvent(e)
	if err != nil {
		a.Delivery.Reject(e.RequestID, err)
		return
	}
	if event == nil {
		return
	}
	_ = a.Delivery.Emit(*event)
}

// PersistingSink records terminal events directly for explicit persistence
// integrations. Production request producers use AccountingSink so failures
// remain visible through the metadata stream and durable producer epoch.
type PersistingSink struct {
	Persist func(ctx context.Context, event *usage.Event, payload []byte) error
	Log     *slog.Logger
}

// PersistTimeout bounds writing one directly persisted record.
const PersistTimeout = 10 * time.Second

// Terminal records one finished request.
func (p *PersistingSink) Terminal(e Envelope) {
	event, err := UsageEvent(e)
	if err != nil {
		p.logger().Warn("usage event not recorded", "request_id", e.RequestID, "error", err.Error())
		return
	}
	if event == nil {
		return
	}
	payload, err := json.Marshal(event)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), PersistTimeout)
		err = p.Persist(ctx, event, payload)
		cancel()
	}
	if err != nil {
		p.logger().Warn("usage event not recorded", "request_id", e.RequestID, "error", err.Error())
	}
}

func (p *PersistingSink) logger() *slog.Logger {
	if p.Log == nil {
		return slog.Default()
	}
	return p.Log
}

// accountingEvent renders one envelope as a usage event, or nil when there is
// nothing to account for: no API key owns a caller request, or it ended before
// a route and a runtime generation could be attributed to it.
func UsageEvent(e Envelope) (*usage.Event, error) {
	if (e.KeyID == "" && !e.Origin.Keyless()) || e.Route == "" || e.RuntimeGenerationID == "" {
		return nil, nil
	}
	if len(e.Attempts) > 0 && e.Attempts[len(e.Attempts)-1].GenerationUsageDeferred {
		return nil, nil
	}
	eventID, err := accountingEventID(e)
	if err != nil {
		return nil, fmt.Errorf("generate usage event ID: %w", err)
	}
	event := &usage.Event{
		CacheResult:          e.CacheResult,
		EvaluatorGuardrailID: OptionalText(e.EvaluatorGuardrailID),
		Version:              usage.WireVersion,
		EventID:              eventID.String(),
		RequestID:            e.AccountingID,
		RuntimeGenerationID:  e.RuntimeGenerationID,
		APIKeyID:             e.KeyID,
		AgentPrincipalID:     e.AgentPrincipalID,
		MemberPrincipalID:    e.MemberPrincipalID,
		DelegationPairDigest: e.DelegationPairDigest,
		Origin:               e.Origin,
		ParentRequestID:      OptionalText(e.ParentRequestID),
		Attribution:          e.Attribution,
		EndUserDigest:        e.EndUserDigest,
		BudgetBoundary:       e.BudgetBoundary,
		GuardrailDecisions:   e.GuardrailDecisions, PolicyDecisions: e.PolicyDecisions,
		BudgetGroupID:      e.BudgetGroupID,
		RouteSlug:          e.Route,
		Operation:          e.Operation,
		Surface:            e.Surface,
		RequestStartedAt:   e.StartedAt,
		RequestCompletedAt: e.CompletedAt,
		ObservedAt:         e.CompletedAt,
		StatusCode:         OptionalStatus(e.Status),
		ErrorClass:         OptionalText(e.ErrorClass),
		LatencyMS:          Milliseconds(e.Duration),
		PayloadCaptured:    e.PayloadCaptured,
		Attempts:           make([]usage.Attempt, 0, len(e.Attempts)),
	}
	for index := range e.Attempts {
		attempt, err := UsageAttempt(e, index)
		if err != nil {
			return nil, err
		}
		event.Attempts = append(event.Attempts, attempt)
	}
	// A request with no attempt has no target and no metering evidence: every
	// field that would describe one stays absent.
	if final := len(e.Attempts) - 1; final >= 0 {
		fact := &e.Attempts[final]
		event.ProviderID = OptionalText(fact.ProviderID)
		event.UpstreamModel = OptionalText(fact.UpstreamModel)
		event.Committed = fact.Committed
		event.FirstByteMS = OptionalMilliseconds(e.FirstByte)
		// The request is settled when the attempt that served it reported its
		// usage and had nothing left to reconcile.
		if fact.UsageObserved {
			evidence := AttemptUsage(fact, e.Operation)
			event.UsageComplete = evidence.Evidence.Complete()
			event.InputTokens, event.OutputTokens, event.CachedInputTokens = evidence.InputTokens, evidence.OutputTokens, evidence.CachedInputTokens
			event.CacheWriteInputTokens, event.CacheWrite5MInputTokens, event.CacheWrite1HInputTokens = evidence.CacheWriteInputTokens, evidence.CacheWrite5MInputTokens, evidence.CacheWrite1HInputTokens
			event.MediaUnits = evidence.MediaUnits
			event.AudioInputTokens, event.CachedAudioInputTokens, event.AudioOutputTokens = evidence.AudioInputTokens, evidence.CachedAudioInputTokens, evidence.AudioOutputTokens
			event.ImageInputTokens, event.CachedImageInputTokens, event.ImageOutputTokens = evidence.ImageInputTokens, evidence.CachedImageInputTokens, evidence.ImageOutputTokens
			event.ToolCalls = evidence.ToolCalls
			event.Batch = evidence.Batch
		} else {
			event.UsageComplete = false
		}
	}
	return event, nil
}

type AttemptPolicy struct {
	Commitment         *routing.Commitment `json:"commitment"`
	CommitmentPinned   bool                `json:"commitment_pinned"`
	DiscountID         *string             `json:"discount_id"`
	DiscountPinned     bool                `json:"discount_pinned"`
	Digest             string              `json:"digest"`
	PricingPinned      bool                `json:"pricing_pinned"`
	RateCardPinned     bool                `json:"rate_card_pinned"`
	RateCardRevisionID *string             `json:"rate_card_revision_id"`
	Strategy           string              `json:"strategy"`
	VendorID           string              `json:"vendor_id"`
}

// accountingAttempt renders one attempt, including the provenance that
// explains why this target served and what it reported.
func UsageAttempt(e Envelope, index int) (usage.Attempt, error) {
	fact := &e.Attempts[index]
	evidence := AttemptUsage(fact, e.Operation)
	attemptID, err := accountingAttemptID(e, index)
	if err != nil {
		return usage.Attempt{}, fmt.Errorf("generate usage attempt ID: %w", err)
	}
	attempt := usage.Attempt{
		Execution:          fact.Execution,
		GuardrailDecisions: fact.GuardrailDecisions,
		ID:                 attemptID.String(),
		// The ordinal is the position in the sequence of attempts made.
		Ordinal:       index + 1,
		ProviderID:    fact.ProviderID,
		UpstreamModel: fact.UpstreamModel,
		StartedAt:     fact.StartedAt,
		CompletedAt:   fact.StartedAt.Add(max(fact.Duration, 0)),
		StatusCode:    OptionalStatus(fact.Status),
		Committed:     fact.Committed,
		LatencyMS:     Milliseconds(fact.Duration),
		FirstByteMS:   OptionalMilliseconds(fact.FirstByte),
		Usage:         &evidence,
		Routing: &usage.Routing{
			Prompt:           fact.Prompt,
			Cache:            fact.Cache,
			Interaction:      fact.Interaction,
			Mode:             OptionalText(fact.Mode),
			CredentialSlotID: OptionalText(fact.SlotID),
			CredentialSource: fact.CredentialSource, BudgetExempt: fact.BudgetExempt,
			CredentialVersionID: OptionalText(fact.CredentialID),
			ProviderRevisionID:  fact.ProviderRevisionID,
			Leg:                 fact.Leg,
			Selector:            OptionalText(fact.Selector),
			Baseline:            fact.Baseline,
		},
	}
	if fact.Retry > 0 {
		retry := fact.Retry
		attempt.Routing.Retry = &retry
	}
	attempt.Routing.Budgets = fact.Budgets
	pricingPinned := fact.PolicyDigest != "" || fact.PricingPinned
	if pricingPinned || fact.CommitmentPinned || fact.CardPinned {
		var discountID *string
		if fact.Discount != nil {
			discountID = &fact.Discount.ID
		}
		policy, err := json.Marshal(AttemptPolicy{
			Commitment: fact.Commitment, CommitmentPinned: fact.CommitmentPinned,
			DiscountID: discountID, DiscountPinned: pricingPinned,
			Digest: fact.PolicyDigest, PricingPinned: pricingPinned,
			RateCardPinned: fact.CardPinned, RateCardRevisionID: fact.CardRevisionID,
			Strategy: fact.Strategy, VendorID: fact.VendorID,
		})
		if err == nil {
			rawPolicy := json.RawMessage(policy)
			attempt.Routing.Policy = &rawPolicy
		}
		if fact.Price != nil && (pricingPinned || fact.CommitmentPinned) {
			attempt.Routing.PricingRevisionID = &fact.Price.RevisionID
		}
	}
	if fact.Class != ClassSuccess && fact.Class != ClassCacheHit {
		attempt.ErrorClass = OptionalText(fact.Class)
	}
	if fact.UsageObserved {
		attempt.Routing.StreamedOutputTokens = streamedTokens(fact)
		if e.Operation == "embeddings" {
			attempt.Routing.StreamedOutputTokens = nil
		}
	}
	if fact.FirstOutput != nil {
		output := Milliseconds(*fact.FirstOutput)
		attempt.Routing.FirstOutputMS = &output
	}
	// The estimate is a diagnostic annotation, recorded only when it is whole:
	// a provenance names how a number was made and there is no number without
	// one. One the event contract would refuse is left off rather than costing
	// the request its billing record.
	var estimated *usage.InputEstimate
	if fact.EstimateProvenance != "" {
		estimated = &usage.InputEstimate{Tokens: max(fact.EstimatedInputTokens, 0), Provenance: usage.EstimateProvenance(fact.EstimateProvenance)}
	}
	if usage.ValidEstimate(estimated, fact.ModelFamily) {
		attempt.InputEstimate, attempt.ModelFamily = estimated, fact.ModelFamily
	}
	return attempt, nil
}

// AttemptSuccessful reports whether the attempt completed successfully, which
// is when a per-request price component applies.
func AttemptSuccessful(fact *AttemptFact) bool {
	return (fact.Class == ClassSuccess || fact.Class == ClassCacheHit) && (fact.Status >= 200 && fact.Status <= 299 || fact.Status == 304)
}

// AttemptUsage is the billing evidence an attempt carries into its usage event.
// It is also what the gateway prices an attempt by when it settles the request's
// cost, so what the budget holds for a request and what accounting later records
// for it are one reading of the same attempt.
func AttemptUsage(fact *AttemptFact, operation string) usage.AttemptUsage {
	if !fact.UsageObserved {
		return usage.AttemptUsage{Evidence: usage.EvidenceFrom(false, fact.UsageComplete, fact.BillingUncertain)}
	}
	if fact.TokenlessUsage {
		return usage.AttemptUsage{Evidence: usage.EvidenceFrom(true, fact.UsageComplete, fact.BillingUncertain)}
	}
	evidence := ObservedUsage(fact.Usage, fact.UsageComplete, operation)
	evidence.Evidence = usage.EvidenceFrom(evidence.Evidence.Observed(), evidence.Evidence.Complete(), fact.BillingUncertain)
	return evidence
}

func ObservedUsage(u *wire.Usage, complete bool, operation string) usage.AttemptUsage {
	return usage.ObservedAttemptUsage(u, complete, operation)
}

// streamedTokens is the output this attempt actually streamed: everything the
// provider billed as output except the reasoning it never sent. It is the
// denominator of streaming throughput, so a provider that reports more
// reasoning than output is floored rather than recorded as a negative rate.
func streamedTokens(fact *AttemptFact) *int64 {
	if fact.Usage == nil {
		return nil
	}
	reasoning := int64(0)
	if fact.Usage.ReasoningTokens != nil {
		reasoning = max(*fact.Usage.ReasoningTokens, 0)
	}
	streamed := max(fact.Usage.OutputTokens-reasoning, 0)
	return &streamed
}

// OptionalText omits an empty string rather than storing one.
func OptionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// OptionalStatus keeps only a status that was actually sent.
func OptionalStatus(status int) *int {
	if status < 100 || status > 599 {
		return nil
	}
	return &status
}

// Milliseconds renders a duration for storage. Time never runs backwards in a
// record, so a negative measurement is reported as none elapsed.
func Milliseconds(d time.Duration) int64 {
	return max(d.Milliseconds(), 0)
}

func OptionalMilliseconds(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	ms := Milliseconds(*d)
	return &ms
}

func accountingEventID(e Envelope) (uuid.UUID, error) {
	for _, attempt := range e.Attempts {
		if attempt.Execution != nil && attempt.Execution.RunID != "" {
			return ingest.WorkflowEventID(attempt.Execution.RunID, e.AccountingID)
		}
	}
	return uuid.NewV7()
}

func accountingAttemptID(e Envelope, index int) (uuid.UUID, error) {
	fact := e.Attempts[index]
	if fact.Execution != nil && fact.Execution.RunID != "" {
		return uuid.Parse(fact.Execution.DispatchID)
	}
	for _, attempt := range e.Attempts {
		if attempt.Execution != nil && attempt.Execution.RunID != "" {
			eventID, err := ingest.WorkflowEventID(attempt.Execution.RunID, e.AccountingID)
			if err != nil {
				return uuid.Nil, err
			}
			return uuid.NewSHA1(eventID, []byte(strconv.Itoa(index+1))), nil
		}
	}
	return uuid.NewV7()
}
