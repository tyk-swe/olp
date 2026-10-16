package gateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/google/uuid"

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
