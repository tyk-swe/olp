package gateway

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/usage"
)

// AccountingSink hands every terminal request to the usage pipeline as the
// content-free event the accounting tables are built from. Emitting is a
// bounded handoff: it never blocks the request that produced it, and an event
// the pipeline could not store is dropped here rather than poisoning the
// stream for every request behind it.
type AccountingSink struct {
	Emitter *usage.Emitter
	Log     *slog.Logger
	Next    Sink
}

// Terminal records one finished request.
func (a *AccountingSink) Terminal(e Envelope) {
	if a.Next != nil {
		a.Next.Terminal(e)
	}
	if a.Emitter == nil {
		return
	}
	event := accountingEvent(e)
	if event == nil {
		return
	}
	if _, err := usage.Validate(event); err != nil {
		a.Emitter.Drop()
		a.logger().Warn("usage event dropped", "request_id", e.RequestID, "error", err.Error())
		return
	}
	if err := a.Emitter.Emit(*event); err != nil {
		a.logger().Warn("usage event not queued", "request_id", e.RequestID, "error", err.Error())
	}
}

func (a *AccountingSink) logger() *slog.Logger {
	if a.Log == nil {
		return slog.Default()
	}
	return a.Log
}

// accountingEvent renders one envelope as a usage event, or nil when there is
// nothing to account for: no API key owns the request, or it ended before a
// route and a runtime generation could be attributed to it.
func accountingEvent(e Envelope) *usage.Event {
	if e.KeyID == "" || e.Route == "" || e.RuntimeGenerationID == "" {
		return nil
	}
	if len(e.Attempts) > 0 && e.Attempts[len(e.Attempts)-1].ResponseUsageDeferred {
		return nil
	}
	event := &usage.Event{
		Version:             usage.WireVersion,
		EventID:             uuid.Must(uuid.NewV7()).String(),
		RequestID:           e.AccountingID,
		RuntimeGenerationID: e.RuntimeGenerationID,
		APIKeyID:            e.KeyID,
		Attribution:         e.Attribution,
		PolicyDecisions:     e.PolicyDecisions,
		BudgetGroupID:       e.BudgetGroupID,
		RouteSlug:           e.Route,
		Operation:           e.Operation,
		Surface:             e.Surface,
		RequestStartedAt:    e.StartedAt,
		RequestCompletedAt:  e.CompletedAt,
		ObservedAt:          e.CompletedAt,
		StatusCode:          optionalStatus(e.Status),
		ErrorClass:          optionalText(e.ErrorClass),
		LatencyMS:           milliseconds(e.Duration),
		Attempts:            make([]usage.Attempt, 0, len(e.Attempts)),
	}
	for index := range e.Attempts {
		event.Attempts = append(event.Attempts, accountingAttempt(e, index))
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
		event.UsageComplete = fact.UsageObserved && fact.UsageComplete
		if fact.UsageObserved {
			event.InputTokens, event.OutputTokens, event.CachedInputTokens = accountingTokens(fact)
			event.CacheWriteInputTokens, event.CacheWrite5MInputTokens, event.CacheWrite1HInputTokens = accountingCacheWrites(fact)
			event.MediaUnits = accountingMediaUnits(fact)
			if e.Operation == "embeddings" {
				event.OutputTokens = nil
			}
		}
	}
	return event
}

// accountingAttempt renders one attempt, including the provenance that
// explains why this target served and what it reported.
func accountingAttempt(e Envelope, index int) usage.Attempt {
	fact := &e.Attempts[index]
	attempt := usage.Attempt{
		ID: uuid.Must(uuid.NewV7()).String(),
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
		Usage: &usage.AttemptUsage{
			Observed:         fact.UsageObserved,
			Complete:         fact.UsageComplete,
			BillingUncertain: fact.BillingUncertain,
		},
		Routing: &usage.Routing{
			Interaction:         fact.Interaction,
			Outcome:             accountingOutcome(fact),
			Mode:                optionalText(fact.Mode),
			CredentialSlotID:    optionalText(fact.SlotID),
			CredentialVersionID: optionalText(fact.CredentialID),
			ProviderRevisionID:  fact.ProviderRevisionID,
		},
	}
	if fact.PolicyDigest != "" {
		policy, _ := json.Marshal(map[string]any{"digest": fact.PolicyDigest, "strategy": fact.Strategy, "pricing_pinned": true, "vendor_id": fact.VendorID})
		rawPolicy := json.RawMessage(policy)
		attempt.Routing.Policy = &rawPolicy
		if fact.Price != nil {
			attempt.Routing.PricingRevisionID = &fact.Price.RevisionID
		}
	}
	if fact.Class != classSuccess {
		attempt.ErrorClass = optionalText(fact.Class)
	}
	if fact.UsageObserved {
		attempt.Usage.InputTokens, attempt.Usage.OutputTokens, attempt.Usage.CachedInputTokens = accountingTokens(fact)
		attempt.Usage.CacheWriteInputTokens, attempt.Usage.CacheWrite5MInputTokens, attempt.Usage.CacheWrite1HInputTokens = accountingCacheWrites(fact)
		attempt.Usage.MediaUnits = accountingMediaUnits(fact)
		attempt.Routing.StreamedOutputTokens = streamedTokens(fact)
		if e.Operation == "embeddings" {
			attempt.Usage.OutputTokens = nil
			attempt.Routing.StreamedOutputTokens = nil
		}
	}
	if fact.FirstOutput != nil {
		output := milliseconds(*fact.FirstOutput)
		attempt.Routing.FirstOutputMS = &output
	}
	return attempt
}

// accountingOutcome carries the outcome facts one attempt established —
// its provider-declared native status, attributed fault, and the bounded
// proxy-local limit it exhausted — beside its routing provenance. Each fact
// is emitted explicitly when recorded and stays null otherwise; the whole
// evidence is omitted only when no outcome fact was recorded at all, which
// is how records written before outcome facts remain distinguishable.
func accountingOutcome(fact *AttemptFact) *usage.OutcomeEvidence {
	if fact.NativeStatus == "" && fact.FaultOrigin == "" && fact.FaultScope == "" &&
		fact.FaultResource == "" && fact.LimitCategory == "" && fact.Limit == 0 {
		return nil
	}
	evidence := &usage.OutcomeEvidence{
		NativeStatus:  optionalText(fact.NativeStatus),
		FaultOrigin:   optionalText(fact.FaultOrigin),
		FaultScope:    optionalText(fact.FaultScope),
		FaultResource: optionalText(fact.FaultResource),
		LimitCategory: optionalText(fact.LimitCategory),
	}
	if fact.Limit > 0 {
		evidence.Limit = &fact.Limit
	}
	return evidence
}

// accountingTokens is the metering an attempt disclosed. A provider that
// reports a negative count is reporting nonsense, and the floor keeps one
// upstream's malformed reply from failing the whole event.
func accountingTokens(fact *AttemptFact) (input, output, cached *int64) {
	if fact.Usage == nil {
		return nil, nil, nil
	}
	in, out := max(fact.Usage.InputTokens, 0), max(fact.Usage.OutputTokens, 0)
	if fact.Usage.CachedInputTokens != nil {
		hit := max(*fact.Usage.CachedInputTokens, 0)
		cached = &hit
	}
	return &in, &out, cached
}

func accountingCacheWrites(fact *AttemptFact) (write, write5m, write1h *int64) {
	if fact.Usage == nil {
		return nil, nil, nil
	}
	return fact.Usage.CacheWriteInputTokens, fact.Usage.CacheWrite5MInputTokens,
		fact.Usage.CacheWrite1HInputTokens
}

// accountingMediaUnits is the media quantity an attempt disclosed, carried as
// the same decimal text the usage event validates and stores.
func accountingMediaUnits(fact *AttemptFact) *string {
	if fact.Usage == nil || fact.Usage.MediaUnits == nil {
		return nil
	}
	value := *fact.Usage.MediaUnits
	return &value
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
