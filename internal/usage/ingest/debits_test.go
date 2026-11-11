package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/usage"
)

const debitEventDispatchID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"

func TestDebitDispatchIDRequiresConsistentReferences(t *testing.T) {
	plain := &usage.Event{Attempts: []usage.Attempt{{}}}
	if _, present := debitDispatchID(plain); present {
		t.Fatal("ordinary event routed to debit settlement")
	}
	consistent := &usage.Event{Attempts: []usage.Attempt{
		{Execution: &usage.ExecutionReference{DispatchID: debitEventDispatchID}},
		{Execution: &usage.ExecutionReference{DispatchID: debitEventDispatchID}},
	}}
	if id, present := debitDispatchID(consistent); !present || id != debitEventDispatchID {
		t.Fatalf("dispatch id = %q present=%v", id, present)
	}
	mixed := &usage.Event{Attempts: []usage.Attempt{
		{Execution: &usage.ExecutionReference{DispatchID: debitEventDispatchID}},
		{Execution: &usage.ExecutionReference{DispatchID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"}},
	}}
	if id, present := debitDispatchID(mixed); !present || id != "" {
		t.Fatalf("mixed dispatch references accepted with id %q", id)
	}
}

type workflowReceiptRow struct{ digest [sha256.Size]byte }

func (r workflowReceiptRow) Scan(destinations ...any) error {
	if len(destinations) != 1 {
		return errors.New("unexpected receipt columns")
	}
	switch destinations[0].(type) {
	case *string:
		return pgx.ErrNoRows
	case *[]byte:
		return pgtype.NewMap().Scan(pgtype.ByteaOID, pgtype.BinaryFormatCode, r.digest[:], destinations[0])
	default:
		return errors.New("unexpected receipt column type")
	}
}

type unsettledWorkflowRow struct{}

func (unsettledWorkflowRow) Scan(destinations ...any) error {
	if len(destinations) != 1 {
		return errors.New("unexpected workflow columns")
	}
	switch destinations[0].(type) {
	case *string:
		return pgtype.NewMap().Scan(pgtype.TextOID, pgtype.TextFormatCode, []byte("claimed"), destinations[0])
	case *bool:
		return pgtype.NewMap().Scan(pgtype.BoolOID, pgtype.TextFormatCode, []byte("false"), destinations[0])
	default:
		return errors.New("unexpected workflow column type")
	}
}

func workflowTerminalFixture(t *testing.T) (*usage.Event, []byte) {
	t.Helper()
	initial := metadataEvent()
	raw, encodeErr := usage.Encode(initial)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	firstID := initial.Attempts[0].ID
	finalID := initial.Attempts[1].ID
	firstNeedle := `"id":"` + firstID + `",`
	finalNeedle := `"id":"` + finalID + `",`
	firstRef := `"execution":{"dispatch_id":"` + firstID + `","run_id":"` + initial.EventID + `","step_id":"` + uuid.NewString() + `"},` + firstNeedle
	finalRef := `"execution":{"dispatch_id":"` + finalID + `","run_id":"` + initial.EventID + `","step_id":"` + uuid.NewString() + `"},` + finalNeedle
	payload := []byte(strings.NewReplacer(firstNeedle, firstRef, finalNeedle, finalRef).Replace(string(raw)))
	event, decodeErr := usage.Decode(payload)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	return event, payload
}

func TestPersistWorkflowReceiptIsIdempotentAndRejectsConflicts(t *testing.T) {
	_, payload := workflowTerminalFixture(t)
	accepted, acceptErr := usage.DecodeAccountingEvent(payload)
	if acceptErr != nil {
		t.Fatal(acceptErr)
	}
	replayTx := stubTx{row: workflowReceiptRow{digest: sha256.Sum256(payload)}}
	clear(payload)
	result, replayErr := PersistEventTx(t.Context(), replayTx, accepted)
	if replayErr != nil || result.Outcome != PersistOutcomeDuplicate {
		t.Fatalf("workflow replay was not recognized: outcome %v, error %v", result.Outcome, replayErr)
	}
	conflictTx := stubTx{row: workflowReceiptRow{digest: sha256.Sum256([]byte("different receipt"))}}
	if _, conflictErr := PersistEventTx(t.Context(), conflictTx, accepted); !errors.Is(conflictErr, usage.ErrInvalidEvent) {
		t.Fatalf("conflicting workflow receipt accepted: %v", conflictErr)
	}
}

func TestPersistWorkflowRejectsAttemptWithoutSettledDebit(t *testing.T) {
	_, payload := workflowTerminalFixture(t)
	accepted, acceptErr := usage.DecodeAccountingEvent(payload)
	if acceptErr != nil {
		t.Fatal(acceptErr)
	}
	ledger := stubTx{row: unsettledWorkflowRow{}}
	if _, err := PersistEventTx(t.Context(), ledger, accepted); !errors.Is(err, debits.ErrConflict) {
		t.Fatalf("workflow with an unsettled attempt was accepted: %v", err)
	}
}

func TestPersistWorkflowRejectsUnboundReceiptIdentity(t *testing.T) {
	event, _ := workflowTerminalFixture(t)
	event.EventID = uuid.NewString()
	changed, changedErr := usage.NewAccountingEvent(event)
	if changedErr != nil {
		t.Fatal(changedErr)
	}
	if _, err := PersistEventTx(t.Context(), stubTx{}, changed); !errors.Is(err, debits.ErrConflict) {
		t.Fatalf("workflow accepted a receipt unrelated to its run and request: %v", err)
	}
}

func TestSessionDebitResultIdentityIsStablePerEvent(t *testing.T) {
	event := &usage.Event{EventID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50609"}
	one := sessionDebitResult(debitEventDispatchID, event, []byte(`{"event":1}`), "0.5", "0.5", true)
	two := sessionDebitResult(debitEventDispatchID, event, []byte(`{"event":1}`), "0.5", "0.5", true)
	if !bytes.Equal(one.ResultDigest, two.ResultDigest) || !bytes.Equal(one.EvidenceDigest, two.EvidenceDigest) {
		t.Fatal("redelivery of one event changed its settlement identity")
	}
	if !one.Final || len(one.ResultDigest) != 32 || len(one.EvidenceDigest) != 32 {
		t.Fatalf("result is not a final digest-bearing settlement: %+v", one)
	}
	other := sessionDebitResult(debitEventDispatchID, &usage.Event{EventID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50610"}, []byte(`{"event":1}`), "0.5", "0.5", true)
	if bytes.Equal(one.ResultDigest, other.ResultDigest) {
		t.Fatal("two events share one settlement identity")
	}
}

func TestPersistWorkflowAttemptRejectsUnboundOrOutOfOrderEvidence(t *testing.T) {
	event := metadataEvent()
	if _, err := PersistWorkflowAttempt(t.Context(), nil, event, -1); !errors.Is(err, debits.ErrConflict) {
		t.Fatalf("negative attempt accepted: %v", err)
	}
	if _, err := PersistWorkflowAttempt(t.Context(), nil, event, 0); !errors.Is(err, debits.ErrConflict) {
		t.Fatalf("nonterminal attempt accepted: %v", err)
	}
	if _, err := PersistWorkflowAttempt(t.Context(), nil, event, len(event.Attempts)); !errors.Is(err, debits.ErrConflict) {
		t.Fatalf("out-of-range attempt accepted: %v", err)
	}
	if _, err := PersistWorkflowAttempt(t.Context(), nil, event, len(event.Attempts)-1); !errors.Is(err, debits.ErrConflict) {
		t.Fatalf("attempt without dispatch accepted: %v", err)
	}
	if _, err := PersistWorkflowAttempt(t.Context(), nil, &usage.Event{}, 0); err == nil {
		t.Fatal("invalid usage event accepted")
	}
	raw, encodeErr := usage.Encode(event)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	attemptID := event.Attempts[len(event.Attempts)-1].ID
	withExecution := func(runID, dispatchID string) *usage.Event {
		t.Helper()
		needle := `"id":"` + attemptID + `",`
		ref := `"execution":{"dispatch_id":"` + dispatchID + `","run_id":"` + runID + `","step_id":"` + uuid.NewString() + `"},` + needle
		decoded, decodeErr := usage.Decode([]byte(strings.Replace(string(raw), needle, ref, 1)))
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		return decoded
	}
	pool, poolErr := pgxpool.New(t.Context(), "postgres://localhost:1/no_database?connect_timeout=1")
	if poolErr != nil {
		t.Fatal(poolErr)
	}
	pool.Close()
	valid := withExecution(event.EventID, attemptID)
	if _, err := PersistWorkflowAttempt(t.Context(), pool, valid, len(valid.Attempts)-1); err == nil {
		t.Fatal("workflow settlement succeeded with a closed database pool")
	}
	for name, bound := range map[string]*usage.Event{
		"another run":      withExecution(uuid.NewString(), attemptID),
		"another dispatch": withExecution(event.EventID, uuid.NewString()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PersistWorkflowAttempt(t.Context(), nil, bound, len(bound.Attempts)-1); !errors.Is(err, debits.ErrConflict) {
				t.Fatalf("mismatched workflow attempt accepted: %v", err)
			}
		})
	}
}

type stubTx struct {
	pgx.Tx
	row pgx.Row
}

func (tx stubTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (tx stubTx) QueryRow(context.Context, string, ...any) pgx.Row { return tx.row }

func metadataEvent() *usage.Event {
	completedAt := time.Now().UTC()
	firstProvider := uuid.NewString()
	finalProvider := uuid.NewString()
	model := "model-b"
	status := 200
	firstByte := int64(18)
	tokens := int64(7)
	return &usage.Event{
		Version:             usage.WireVersion,
		EventID:             uuid.NewString(),
		RequestID:           uuid.NewString(),
		RuntimeGenerationID: uuid.NewString(),
		APIKeyID:            uuid.NewString(),
		ProviderID:          &finalProvider,
		RouteSlug:           "primary",
		UpstreamModel:       &model,
		Operation:           "generation",
		Surface:             "openai",
		RequestStartedAt:    completedAt.Add(-25 * time.Millisecond),
		RequestCompletedAt:  completedAt,
		ObservedAt:          completedAt,
		StatusCode:          &status,
		Committed:           true,
		LatencyMS:           25,
		FirstByteMS:         &firstByte,
		InputTokens:         &tokens,
		UsageComplete:       true,
		Attempts: []usage.Attempt{
			metadataAttempt(1, firstProvider, "model-a", false),
			metadataAttempt(2, finalProvider, "model-b", true),
		},
	}
}

func metadataAttempt(ordinal int, providerID string, model string, committed bool) usage.Attempt {
	completedAt := time.Now().UTC()
	evidence := usage.AttemptUsage{Evidence: usage.EvidenceFrom(committed, true, false)}
	if committed {
		tokens := int64(7)
		evidence.InputTokens = &tokens
	}
	status := 200
	firstByte := int64(3)
	return usage.Attempt{
		ID:            uuid.NewString(),
		Ordinal:       ordinal,
		ProviderID:    providerID,
		UpstreamModel: model,
		StartedAt:     completedAt.Add(-10 * time.Millisecond),
		CompletedAt:   completedAt,
		StatusCode:    &status,
		Committed:     committed,
		LatencyMS:     10,
		FirstByteMS:   &firstByte,
		Usage:         &evidence,
	}
}
