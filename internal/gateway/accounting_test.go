package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func TestHealthProbeAccountingUsesTheVersionedLossTrackedPipeline(t *testing.T) {
	emitter := usage.NewEmitter(1)
	probe := accountingEnvelope(t)
	probe.Actor, probe.KeyID, probe.Origin = "system", "", usage.OriginProbe
	probe.Mode = "unary"
	for i := range probe.Attempts {
		probe.Attempts[i].Mode = "unary"
	}
	checkpoints := 0
	sink := &AccountingSink{Emitter: emitter, Log: quiet, RecordLoss: func(ctx context.Context, requestID string) error {
		checkpoints++
		if _, bounded := ctx.Deadline(); !bounded || requestID != probe.RequestID || emitter.Snapshot().Lost() != 1 {
			t.Fatal("probe loss checkpoint lacks its local evidence or deadline")
		}
		// Even a failed durable checkpoint leaves local loss visible for
		// budget admission and for the producer's next epoch checkpoint.
		return errors.New("database unavailable")
	}}
	sink.Terminal(probe)
	if snapshot := emitter.Snapshot(); snapshot.Accepted != 1 || snapshot.Lost() != 0 || checkpoints != 0 {
		t.Fatalf("keyless probe was not queued: %+v, checkpoints=%d", snapshot, checkpoints)
	}
	sink.Terminal(probe)
	if snapshot := emitter.Snapshot(); snapshot.Accepted != 1 || snapshot.Lost() != 1 || snapshot.LastLossAt == nil || checkpoints != 1 {
		t.Fatalf("probe loss disappeared after failed checkpoint: %+v, checkpoints=%d", snapshot, checkpoints)
	}
	emitter.Close()
	writes := 0
	emitter.RunWriter(t.Context(), accountingStreamFunc(func(_ context.Context, args ...string) (any, error) {
		writes++
		if len(args) != 5 || args[0] != "XADD" || args[1] != "metadata" || args[3] != "event" {
			t.Fatalf("unexpected metadata write: %v", args)
		}
		event, err := usage.Decode([]byte(args[4]))
		if err != nil {
			t.Fatal(err)
		}
		if event.Version != usage.WireVersion || event.Origin != usage.OriginProbe || event.APIKeyID != "" || event.RequestID != probe.AccountingID || len(event.Attempts) != len(probe.Attempts) {
			t.Fatalf("probe identity or usage lost in stream: %+v", event)
		}
		if _, err := usage.Validate(event); err != nil {
			t.Fatal(err)
		}
		return "1-0", nil
	}), "metadata", quiet)
	if snapshot := emitter.Snapshot(); writes != 1 || snapshot.Persisted != 1 || snapshot.Lost() != 1 || !snapshot.GracefullyDrained() {
		t.Fatalf("probe pipeline did not drain with loss evidence intact: %+v, writes=%d", snapshot, writes)
	}
}

type accountingStreamFunc func(context.Context, ...string) (any, error)

func (f accountingStreamFunc) Do(ctx context.Context, args ...string) (any, error) {
	return f(ctx, args...)
}

// accountingID is a fresh identifier for a fixture field that must be one.
func accountingID(t *testing.T) string {
	t.Helper()
	return uuid.Must(uuid.NewV7()).String()
}

// accountingEnvelope is a streamed request that failed over once and then
// succeeded, the shape most of the accounting pipeline sees.
func accountingEnvelope(t *testing.T) Envelope {
	t.Helper()
	start := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	firstByte, attemptByte := 120*time.Millisecond, 90*time.Millisecond
	reasoning := int64(2)
	retryAfter := 2 * time.Second
	provider, revision := accountingID(t), accountingID(t)
	return Envelope{
		RequestID:           "client-named-request",
		AccountingID:        accountingID(t),
		Actor:               "api_key",
		KeyID:               accountingID(t),
		Route:               "team-chat",
		RouteRevisionID:     accountingID(t),
		ReleaseSequence:     7,
		Family:              string(wire.Chat),
		Mode:                "streaming",
		Operation:           operationGeneration,
		Surface:             surfaceOpenAI,
		Outcome:             "success",
		Status:              200,
		Committed:           true,
		StartedAt:           start,
		CompletedAt:         start.Add(400 * time.Millisecond),
		Duration:            400 * time.Millisecond,
		FirstByte:           &firstByte,
		RuntimeGenerationID: accountingID(t),
		Usage:               &wire.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10},
		Attempts: []AttemptFact{
			{
				Ordinal: 1, TargetID: accountingID(t), ProviderID: provider,
				ProviderRevisionID: revision, UpstreamModel: "fixture-model",
				SlotID: accountingID(t), CredentialID: accountingID(t), CredentialVersion: 2,
				Mode: "streaming", Status: 429, Class: classRateLimit,
				StartedAt: start, Duration: 15 * time.Millisecond, RetryAfter: &retryAfter,
				UsageComplete: true,
			},
			{
				Ordinal: 2, TargetID: accountingID(t), ProviderID: provider,
				ProviderRevisionID: revision, UpstreamModel: "fixture-model",
				SlotID: accountingID(t), CredentialID: accountingID(t), CredentialVersion: 1,
				Mode: "streaming", Status: 200, Class: classSuccess, Committed: true,
				StartedAt: start.Add(20 * time.Millisecond), Duration: 380 * time.Millisecond,
				FirstByte:     &attemptByte,
				Usage:         &wire.Usage{InputTokens: 4, OutputTokens: 6, TotalTokens: 10, ReasoningTokens: &reasoning},
				UsageObserved: true, UsageComplete: true,
			},
		},
	}
}

// Meaningful output is measured independently of protocol setup frames.

// Streaming throughput is measured against what the client actually
// received: the output the provider billed, less the reasoning it kept to
// itself. Without it the console has no denominator to divide by.

// A background response parks its event as plain JSON on the resource and
// settles it later; the estimate must survive that detour too.

// An estimate of zero tokens is still an estimate.

// A negative estimate is a floor of zero, like the usage the provider reports.

// A request that ended before it started cannot be reconciled with
// anything, and one bad event must not travel with the good ones.

// A request the gateway named itself keeps that name in the ledger, so the
// response header and the stored record agree.

func TestAccountingRecordsLossWhenIDGenerationFails(t *testing.T) {
	restore := uuidNewV7
	t.Cleanup(func() { uuidNewV7 = restore })
	for _, tc := range []struct {
		name   string
		failAt int
	}{
		{"event", 1},
		{"first attempt", 2},
		{"last attempt", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envelope := accountingEnvelope(t)
			emitter := usage.NewEmitter(1)
			defer emitter.Close()
			idError := errors.New("entropy unavailable")
			calls := 0
			uuidNewV7 = func() (uuid.UUID, error) {
				calls++
				if calls == tc.failAt {
					return uuid.UUID{}, idError
				}
				return restore()
			}
			var output bytes.Buffer
			checkpoints := 0
			sink := &AccountingSink{
				Emitter: emitter,
				Log:     slog.New(slog.NewJSONHandler(&output, nil)),
				RecordLoss: func(ctx context.Context, requestID string) error {
					checkpoints++
					if _, bounded := ctx.Deadline(); !bounded || requestID != envelope.RequestID || emitter.Snapshot().Lost() != 1 {
						t.Fatal("loss checkpoint lacks its local evidence, request identity, or deadline")
					}
					return errors.New("database unavailable")
				},
			}
			sink.Terminal(envelope)
			if snapshot := emitter.Snapshot(); snapshot.Accepted != 0 || snapshot.Lost() != 1 || snapshot.LastLossAt == nil || checkpoints != 1 {
				t.Fatalf("UUID failure lost its accounting evidence: %+v, checkpoints=%d", snapshot, checkpoints)
			}
			if !bytes.Contains(output.Bytes(), []byte(idError.Error())) || !bytes.Contains(output.Bytes(), []byte(envelope.RequestID)) {
				t.Fatalf("UUID failure was not logged with its request: %s", output.Bytes())
			}
		})
	}
}

func TestAccountingSkipsEventsWithoutLoss(t *testing.T) {
	for _, tc := range []struct {
		name     string
		envelope Envelope
	}{
		{"missing key", Envelope{Route: "route", RuntimeGenerationID: "generation"}},
		{"missing route", Envelope{KeyID: "key", RuntimeGenerationID: "generation"}},
		{"missing generation", Envelope{KeyID: "key", Route: "route"}},
		{"deferred usage", Envelope{KeyID: "key", Route: "route", RuntimeGenerationID: "generation", Attempts: []AttemptFact{{GenerationUsageDeferred: true}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitter := usage.NewEmitter(1)
			defer emitter.Close()
			sink := &AccountingSink{
				Emitter: emitter,
				Log:     quiet,
				RecordLoss: func(context.Context, string) error {
					t.Fatal("intentional accounting skip recorded as a loss")
					return nil
				},
			}
			sink.Terminal(tc.envelope)
			if snapshot := emitter.Snapshot(); snapshot.Accepted != 0 || snapshot.Lost() != 0 {
				t.Fatalf("intentional accounting skip changed pipeline counters: %+v", snapshot)
			}
		})
	}
}

func TestAccountingAttemptPolicyPayloadCarriesPins(t *testing.T) {
	revision := "card-revision"
	fact := AttemptFact{
		PolicyDigest: "digest-1", Strategy: "cheapest", VendorID: "vendor-1",
		PricingPinned: true, Discount: &usage.RoutingDiscount{ID: "discount-1"},
		CommitmentPinned: true, Commitment: &routing.Commitment{ID: "schedule", Amount: "10", Period: "day"},
		CardPinned: true, CardRevisionID: &revision,
		StartedAt: time.Now(),
	}
	attempt, err := accountingAttempt(Envelope{Operation: "generation", Attempts: []AttemptFact{fact}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.Routing.Policy == nil {
		t.Fatal("pricing provenance missing")
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(*attempt.Routing.Policy, &present); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"commitment", "commitment_pinned", "discount_id", "discount_pinned", "digest", "pricing_pinned", "rate_card_pinned", "rate_card_revision_id", "strategy", "vendor_id"} {
		if _, ok := present[key]; !ok {
			t.Fatalf("policy payload lost key %q: %s", key, *attempt.Routing.Policy)
		}
	}
	var policy struct {
		Commitment *struct {
			ID string `json:"id"`
		} `json:"commitment"`
		CommitmentPinned   bool    `json:"commitment_pinned"`
		DiscountID         *string `json:"discount_id"`
		DiscountPinned     bool    `json:"discount_pinned"`
		Digest             string  `json:"digest"`
		PricingPinned      bool    `json:"pricing_pinned"`
		RateCardPinned     bool    `json:"rate_card_pinned"`
		RateCardRevisionID *string `json:"rate_card_revision_id"`
		Strategy           string  `json:"strategy"`
		VendorID           string  `json:"vendor_id"`
	}
	if err := json.Unmarshal(*attempt.Routing.Policy, &policy); err != nil {
		t.Fatal(err)
	}
	if policy.Commitment == nil || policy.Commitment.ID != "schedule" || !policy.CommitmentPinned ||
		policy.DiscountID == nil || *policy.DiscountID != "discount-1" || !policy.DiscountPinned ||
		policy.Digest != "digest-1" || !policy.PricingPinned || !policy.RateCardPinned ||
		policy.RateCardRevisionID == nil || *policy.RateCardRevisionID != revision ||
		policy.Strategy != "cheapest" || policy.VendorID != "vendor-1" {
		t.Fatalf("policy payload lost a pin: %s", *attempt.Routing.Policy)
	}
}
