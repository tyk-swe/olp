package usage

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/limits"
)

const debitEventDispatchID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"

func TestDebitDispatchIDRequiresConsistentReferences(t *testing.T) {
	plain := &Event{Attempts: []Attempt{{}}}
	if _, present := debitDispatchID(plain); present {
		t.Fatal("ordinary event routed to debit settlement")
	}
	consistent := &Event{Attempts: []Attempt{
		{Execution: &ExecutionReference{DispatchID: debitEventDispatchID}},
		{Execution: &ExecutionReference{DispatchID: debitEventDispatchID}},
	}}
	if id, present := debitDispatchID(consistent); !present || id != debitEventDispatchID {
		t.Fatalf("dispatch id = %q present=%v", id, present)
	}
	mixed := &Event{Attempts: []Attempt{
		{Execution: &ExecutionReference{DispatchID: debitEventDispatchID}},
		{Execution: &ExecutionReference{DispatchID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50608"}},
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

func workflowTerminalFixture(t *testing.T) (*Event, []byte) {
	t.Helper()
	initial := metadataEvent()
	raw, encodeErr := Encode(initial)
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
	event, decodeErr := Decode(payload)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	return event, payload
}

func TestPersistWorkflowReceiptIsIdempotentAndRejectsConflicts(t *testing.T) {
	event, payload := workflowTerminalFixture(t)
	replayTx := cardRevisionTx{row: workflowReceiptRow{digest: sha256.Sum256(payload)}}
	result, replayErr := PersistEventTx(t.Context(), replayTx, event, payload)
	if replayErr != nil || result.Outcome != PersistOutcomeDuplicate {
		t.Fatalf("workflow replay was not recognized: outcome %v, error %v", result.Outcome, replayErr)
	}
	conflictTx := cardRevisionTx{row: workflowReceiptRow{digest: sha256.Sum256([]byte("different receipt"))}}
	if _, conflictErr := PersistEventTx(t.Context(), conflictTx, event, payload); !errors.Is(conflictErr, ErrInvalidEvent) {
		t.Fatalf("conflicting workflow receipt accepted: %v", conflictErr)
	}
}

func TestPersistWorkflowRejectsAttemptWithoutSettledDebit(t *testing.T) {
	event, payload := workflowTerminalFixture(t)
	ledger := cardRevisionTx{row: unsettledWorkflowRow{}}
	if _, err := PersistEventTx(t.Context(), ledger, event, payload); !errors.Is(err, limits.ErrDebitConflict) {
		t.Fatalf("workflow with an unsettled attempt was accepted: %v", err)
	}
}

func TestPersistWorkflowRejectsUnboundReceiptIdentity(t *testing.T) {
	event, payload := workflowTerminalFixture(t)
	event.EventID = uuid.NewString()
	if _, err := PersistEventTx(t.Context(), cardRevisionTx{}, event, payload); !errors.Is(err, limits.ErrDebitConflict) {
		t.Fatalf("workflow accepted a receipt unrelated to its run and request: %v", err)
	}
}

func TestSessionDebitResultIdentityIsStablePerEvent(t *testing.T) {
	event := &Event{EventID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50609"}
	one := sessionDebitResult(debitEventDispatchID, event, []byte(`{"event":1}`), "0.5", "0.5", true)
	two := sessionDebitResult(debitEventDispatchID, event, []byte(`{"event":1}`), "0.5", "0.5", true)
	if !bytes.Equal(one.ResultDigest, two.ResultDigest) || !bytes.Equal(one.EvidenceDigest, two.EvidenceDigest) {
		t.Fatal("redelivery of one event changed its settlement identity")
	}
	if !one.Final || len(one.ResultDigest) != 32 || len(one.EvidenceDigest) != 32 {
		t.Fatalf("result is not a final digest-bearing settlement: %+v", one)
	}
	other := sessionDebitResult(debitEventDispatchID, &Event{EventID: "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50610"}, []byte(`{"event":1}`), "0.5", "0.5", true)
	if bytes.Equal(one.ResultDigest, other.ResultDigest) {
		t.Fatal("two events share one settlement identity")
	}
}

func TestPersistWorkflowAttemptRejectsUnboundOrOutOfOrderEvidence(t *testing.T) {
	event := metadataEvent()
	if _, err := PersistWorkflowAttempt(t.Context(), nil, event, -1); !errors.Is(err, limits.ErrDebitConflict) {
		t.Fatalf("negative attempt accepted: %v", err)
	}
	if _, err := PersistWorkflowAttempt(t.Context(), nil, event, 0); !errors.Is(err, limits.ErrDebitConflict) {
		t.Fatalf("nonterminal attempt accepted: %v", err)
	}
	if _, err := PersistWorkflowAttempt(t.Context(), nil, event, len(event.Attempts)); !errors.Is(err, limits.ErrDebitConflict) {
		t.Fatalf("out-of-range attempt accepted: %v", err)
	}
	if _, err := PersistWorkflowAttempt(t.Context(), nil, event, len(event.Attempts)-1); !errors.Is(err, limits.ErrDebitConflict) {
		t.Fatalf("attempt without dispatch accepted: %v", err)
	}
	if _, err := PersistWorkflowAttempt(t.Context(), nil, &Event{}, 0); err == nil {
		t.Fatal("invalid usage event accepted")
	}
	raw, encodeErr := Encode(event)
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	attemptID := event.Attempts[len(event.Attempts)-1].ID
	withExecution := func(runID, dispatchID string) *Event {
		t.Helper()
		needle := `"id":"` + attemptID + `",`
		ref := `"execution":{"dispatch_id":"` + dispatchID + `","run_id":"` + runID + `","step_id":"` + uuid.NewString() + `"},` + needle
		decoded, decodeErr := Decode([]byte(strings.Replace(string(raw), needle, ref, 1)))
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
	for name, bound := range map[string]*Event{
		"another run":      withExecution(uuid.NewString(), attemptID),
		"another dispatch": withExecution(event.EventID, uuid.NewString()),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PersistWorkflowAttempt(t.Context(), nil, bound, len(bound.Attempts)-1); !errors.Is(err, limits.ErrDebitConflict) {
				t.Fatalf("mismatched workflow attempt accepted: %v", err)
			}
		})
	}
}
