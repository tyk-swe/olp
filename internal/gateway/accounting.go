package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

var uuidNewV7 = uuid.NewV7

// AccountingSink hands every terminal request to the usage pipeline as the
// content-free event the accounting tables are built from. Emitting is a
// bounded handoff. A rejected event checkpoints its loss with a bounded write
// so other replicas can refuse admission against incomplete cost accounting.
type AccountingSink struct {
	RecordLoss func(context.Context, string) error
	Emitter    *usage.Emitter
	Log        *slog.Logger
	Next       Sink
}

// Terminal records one finished request.
func (a *AccountingSink) Terminal(e Envelope) {
	if a.Next != nil {
		a.Next.Terminal(e)
	}
	if a.Emitter == nil {
		return
	}
	event, err := accountingEvent(e)
	if err != nil {
		a.drop(e.RequestID, err)
		return
	}
	if event == nil {
		return
	}
	if _, err := usage.Validate(event); err != nil {
		a.drop(e.RequestID, err)
		return
	}
	if err := a.Emitter.Emit(*event); err != nil {
		a.recordLoss(e.RequestID)
		a.logger().Warn("usage event not queued", "request_id", e.RequestID, "error", err.Error())
	}
}

func (a *AccountingSink) drop(requestID string, err error) {
	a.Emitter.Drop()
	a.recordLoss(requestID)
	a.logger().Warn("usage event dropped", "request_id", requestID, "error", err.Error())
}

func (a *AccountingSink) recordLoss(requestID string) {
	if a.RecordLoss == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.RecordLoss(ctx, requestID); err != nil {
		a.logger().Error("accounting loss checkpoint failed", "request_id", requestID, "error", err)
	}
}

func (a *AccountingSink) logger() *slog.Logger {
	if a.Log == nil {
		return slog.Default()
	}
	return a.Log
}

// PersistingSink records terminal events directly for explicit persistence
// integrations. Production request producers use AccountingSink so failures
// remain visible through the metadata stream and durable producer epoch.
type PersistingSink struct {
	Persist func(ctx context.Context, event *usage.Event, payload []byte) error
	Log     *slog.Logger
}

// persistTimeout bounds writing one directly persisted record.
const persistTimeout = 10 * time.Second

// Terminal records one finished request.
func (p *PersistingSink) Terminal(e Envelope) {
	event, err := accountingEvent(e)
	if err != nil {
		p.logger().Warn("usage event not recorded", "request_id", e.RequestID, "error", err.Error())
		return
	}
	if event == nil {
		return
	}
	payload, err := json.Marshal(event)
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), persistTimeout)
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
func accountingEvent(e Envelope) (*usage.Event, error) {
	if (e.KeyID == "" && !usage.Keyless(e.Origin)) || e.Route == "" || e.RuntimeGenerationID == "" {
		return nil, nil
	}
	if len(e.Attempts) > 0 && e.Attempts[len(e.Attempts)-1].GenerationUsageDeferred {
		return nil, nil
	}
	eventID, err := uuidNewV7()
	if err != nil {
		return nil, fmt.Errorf("generate usage event ID: %w", err)
	}
	event := &usage.Event{
		CacheResult:          e.CacheResult,
		EvaluatorGuardrailID: optionalText(e.EvaluatorGuardrailID),
		Version:              usage.WireVersion,
		EventID:              eventID.String(),
		RequestID:            e.AccountingID,
		RuntimeGenerationID:  e.RuntimeGenerationID,
		APIKeyID:             e.KeyID,
		Origin:               e.Origin,
		ParentRequestID:      optionalText(e.ParentRequestID),
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
		StatusCode:         optionalStatus(e.Status),
		ErrorClass:         optionalText(e.ErrorClass),
		LatencyMS:          milliseconds(e.Duration),
		PayloadCaptured:    e.PayloadCaptured,
		Attempts:           make([]usage.Attempt, 0, len(e.Attempts)),
	}
	for index := range e.Attempts {
		attempt, err := accountingAttempt(e, index)
		if err != nil {
			return nil, err
		}
		event.Attempts = append(event.Attempts, attempt)
	}
	// A request with no attempt has no target and no metering evidence: every
	// field that would describe one stays absent.
	if final := len(e.Attempts) - 1; final >= 0 {
		fact := &e.Attempts[final]
		event.ProviderID = optionalText(fact.ProviderID)
		event.UpstreamModel = optionalText(fact.UpstreamModel)
		event.Committed = fact.Committed
		event.FirstByteMS = optionalMilliseconds(e.FirstByte)
		// The request is settled when the attempt that served it reported its
		// usage and had nothing left to reconcile.
		if fact.UsageObserved {
			evidence := attemptUsage(fact, e.Operation)
			event.UsageComplete = evidence.Complete
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

type attemptPolicy struct {
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
func accountingAttempt(e Envelope, index int) (usage.Attempt, error) {
	fact := &e.Attempts[index]
	evidence := attemptUsage(fact, e.Operation)
	attemptID, err := uuidNewV7()
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
		StatusCode:    optionalStatus(fact.Status),
		Committed:     fact.Committed,
		LatencyMS:     milliseconds(fact.Duration),
		FirstByteMS:   optionalMilliseconds(fact.FirstByte),
		Usage:         &evidence,
		Routing: &usage.Routing{
			Cache:            fact.Cache,
			Interaction:      fact.Interaction,
			Mode:             optionalText(fact.Mode),
			CredentialSlotID: optionalText(fact.SlotID),
			CredentialSource: fact.CredentialSource, BudgetExempt: fact.BudgetExempt,
			CredentialVersionID: optionalText(fact.CredentialID),
			ProviderRevisionID:  fact.ProviderRevisionID,
			Leg:                 fact.Leg,
			Selector:            optionalText(fact.Selector),
			Baseline:            fact.Baseline,
		},
	}
	if fact.Retry > 0 {
		retry := fact.Retry
		attempt.Routing.Retry = &retry
	}
	attempt.Routing.Budgets = fact.Budgets
	pricingPinned := fact.PolicyDigest != "" || fact.PricingPinned
	if pricingPinned || fact.CommitmentPinned {
		var discountID *string
		if fact.Discount != nil {
			discountID = &fact.Discount.ID
		}
		policy, err := json.Marshal(attemptPolicy{
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
		if fact.Price != nil {
			attempt.Routing.PricingRevisionID = &fact.Price.RevisionID
		}
	}
	if fact.Class != classSuccess && fact.Class != "cache_hit" {
		attempt.ErrorClass = optionalText(fact.Class)
	}
	if fact.UsageObserved {
		attempt.Routing.StreamedOutputTokens = streamedTokens(fact)
		if e.Operation == "embeddings" {
			attempt.Routing.StreamedOutputTokens = nil
		}
	}
	if fact.FirstOutput != nil {
		output := milliseconds(*fact.FirstOutput)
		attempt.Routing.FirstOutputMS = &output
	}
	// The estimate is a diagnostic annotation, recorded only when it is whole:
	// a provenance names how a number was made and there is no number without
	// one. One the event contract would refuse is left off rather than costing
	// the request its billing record.
	var estimated *int64
	if fact.EstimateProvenance != "" {
		tokens := max(fact.EstimatedInputTokens, 0)
		estimated = &tokens
	}
	if usage.ValidEstimate(estimated, fact.EstimateProvenance, fact.ModelFamily) {
		attempt.EstimatedInputTokens, attempt.EstimateProvenance, attempt.ModelFamily =
			estimated, fact.EstimateProvenance, fact.ModelFamily
	}
	return attempt, nil
}

// attemptSuccessful reports whether the attempt completed successfully, which
// is when a per-request price component applies.
func attemptSuccessful(fact *AttemptFact) bool {
	return (fact.Class == classSuccess || fact.Class == "cache_hit") && fact.Status >= 200 && fact.Status <= 299
}

// attemptUsage is the billing evidence an attempt carries into its usage event.
// It is also what the gateway prices an attempt by when it settles the request's
// cost, so what the budget holds for a request and what accounting later records
// for it are one reading of the same attempt.
func attemptUsage(fact *AttemptFact, operation string) usage.AttemptUsage {
	if !fact.UsageObserved {
		return usage.AttemptUsage{
			Observed:         false,
			Complete:         fact.UsageComplete,
			BillingUncertain: fact.BillingUncertain,
		}
	}
	if fact.TokenlessUsage {
		return usage.AttemptUsage{Observed: true, Complete: fact.UsageComplete, BillingUncertain: fact.BillingUncertain}
	}
	evidence := observedAttemptUsage(fact.Usage, fact.UsageComplete, operation)
	evidence.BillingUncertain = fact.BillingUncertain
	return evidence
}

func observedAttemptUsage(u *wire.Usage, complete bool, operation string) usage.AttemptUsage {
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

// optionalText omits an empty string rather than storing one.
func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// optionalStatus keeps only a status that was actually sent.
func optionalStatus(status int) *int {
	if status < 100 || status > 599 {
		return nil
	}
	return &status
}

// milliseconds renders a duration for storage. Time never runs backwards in a
// record, so a negative measurement is reported as none elapsed.
func milliseconds(d time.Duration) int64 {
	return max(d.Milliseconds(), 0)
}

func optionalMilliseconds(d *time.Duration) *int64 {
	if d == nil {
		return nil
	}
	ms := milliseconds(*d)
	return &ms
}
