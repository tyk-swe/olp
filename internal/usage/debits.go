package usage

import (
	"context"
	"crypto/sha256"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
)

func debitDispatchID(event *Event) (string, bool) {
	if event == nil {
		return "", false
	}
	dispatchID := ""
	for _, attempt := range event.Attempts {
		if attempt.Execution == nil {
			continue
		}
		if dispatchID == "" {
			dispatchID = attempt.Execution.DispatchID
			continue
		}
		if attempt.Execution.DispatchID != dispatchID {
			return "", true
		}
	}
	return dispatchID, dispatchID != ""
}

func sessionDebitResult(dispatchID string, event *Event, payload []byte, cost, charge string, complete bool) limits.DebitResult {
	resultDigest := sha256.Sum256([]byte("olp:debit-result:" + dispatchID + ":" + event.EventID))
	evidenceDigest := sha256.Sum256(payload)
	return limits.DebitResult{
		DispatchID: dispatchID, ResultDigest: resultDigest[:], EvidenceDigest: evidenceDigest[:],
		EventID: event.EventID, Cost: cost, Charge: charge, Final: true, Complete: complete,
	}
}

func validateDebitEvent(ctx context.Context, tx pgx.Tx, event *Event, dispatchID string) (string, []string, error) {
	var key, run, step, currency string
	var affinity limits.ExecutionAffinity
	if err := tx.QueryRow(ctx, `SELECT api_key_id::text,COALESCE(run_id::text,''),COALESCE(step_id::text,''),currency,affinity FROM olp.execution_debits WHERE id=$1 FOR UPDATE`, dispatchID).Scan(&key, &run, &step, &currency, &affinity); err != nil {
		return "", nil, err
	}
	if event.APIKeyID != key || event.RuntimeGenerationID != affinity.ReleaseID || len(event.Attempts) == 0 {
		return "", nil, limits.ErrDebitConflict
	}
	for _, attempt := range event.Attempts {
		if attempt.Routing != nil && attempt.Routing.BudgetExempt {
			return "", nil, limits.ErrDebitConflict
		}
		ref := attempt.Execution
		if ref == nil || ref.Validate() != nil || ref.DispatchID != dispatchID || ref.RunID != run || ref.StepID != step || ref.TargetID != affinity.TargetID || ref.RouteRevisionID != affinity.RouteRevisionID {
			return "", nil, limits.ErrDebitConflict
		}
		if affinity.ProviderID != "" && attempt.ProviderID != affinity.ProviderID {
			return "", nil, limits.ErrDebitConflict
		}
		if affinity.ProviderRevisionID != "" && (attempt.Routing == nil || attempt.Routing.ProviderRevisionID != affinity.ProviderRevisionID) {
			return "", nil, limits.ErrDebitConflict
		}
		if affinity.CredentialID != "" || affinity.SlotID != "" || affinity.PricingRevisionID != "" {
			if attempt.Routing == nil {
				return "", nil, limits.ErrDebitConflict
			}
			for _, pin := range []struct {
				expected string
				actual   *string
			}{{affinity.CredentialID, attempt.Routing.CredentialVersionID}, {affinity.SlotID, attempt.Routing.CredentialSlotID}, {affinity.PricingRevisionID, attempt.Routing.PricingRevisionID}} {
				if pin.expected != "" && (pin.actual == nil || *pin.actual != pin.expected) {
					return "", nil, limits.ErrDebitConflict
				}
			}
		}
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT owner_id::text FROM olp.execution_debit_owners WHERE dispatch_id=$1 AND owner_kind='supply' ORDER BY owner_id::text`, dispatchID)
	if err != nil {
		return "", nil, err
	}
	var supplyOwners []string
	for rows.Next() {
		var owner string
		if err = rows.Scan(&owner); err != nil {
			rows.Close()
			return "", nil, err
		}
		supplyOwners = append(supplyOwners, owner)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return "", nil, err
	}
	return currency, supplyOwners, nil
}

func PersistDebitEventTx(ctx context.Context, tx pgx.Tx, event *Event, payload []byte) (Persisted, error) {
	dispatchID, present := debitDispatchID(event)
	if !present || dispatchID == "" {
		return Persisted{}, limits.ErrDebitConflict
	}
	currency, supplyOwners, err := validateDebitEvent(ctx, tx, event, dispatchID)
	if err != nil {
		return Persisted{}, err
	}
	persisted, err := persistEventTx(ctx, tx, event, payload, supplyOwners)
	if err != nil {
		return Persisted{}, err
	}
	if persisted.Outcome == PersistOutcomeRejectedOutsideReplayWindow {
		return persisted, nil
	}
	var cost, charge string
	var factsComplete bool
	var facts int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(sum(COALESCE(estimated_cost,0)),0)::text,COALESCE(sum(COALESCE(charge,0)),0)::text,COALESCE(bool_and(usage_complete AND NOT unpriced AND estimated_cost IS NOT NULL AND charge IS NOT NULL AND (currency=$2 OR (estimated_cost=0 AND charge=0))),false),count(*) FROM olp.attempt_usage_facts WHERE event_id=$1`, event.EventID, currency).Scan(&cost, &charge, &factsComplete, &facts); err != nil {
		return Persisted{}, err
	}
	complete := factsComplete && facts > 0
	for _, attempt := range event.Attempts {
		if attempt.Usage == nil || attempt.Usage.BillingUncertain || !attempt.Usage.Complete {
			complete = false
		}
	}
	result := sessionDebitResult(dispatchID, event, payload, cost, charge, complete)
	if err := limits.SettleDebitTx(ctx, tx, result, persisted.CostSnapshots); err != nil {
		return Persisted{}, err
	}
	return persisted, nil
}

// PersistDebitResultTx is the single financial commit boundary used by
// lifecycle owners. It retains ordinary usage receipt admission and pricing;
// reservations are reduced only after matching observed facts are durable.
func PersistDebitResultTx(ctx context.Context, tx pgx.Tx, event *Event, payload []byte, result limits.DebitResult) (Persisted, error) {
	if event == nil || event.EventID != result.EventID {
		return Persisted{}, limits.ErrDebitConflict
	}
	_, supplyOwners, err := validateDebitEvent(ctx, tx, event, result.DispatchID)
	if err != nil {
		return Persisted{}, err
	}
	if duplicate, err := limits.DebitResultReplayTx(ctx, tx, result); err != nil {
		return Persisted{}, err
	} else if duplicate {
		return Persisted{Outcome: PersistOutcomeDuplicate}, nil
	}
	persisted, err := persistEventTx(ctx, tx, event, payload, supplyOwners)
	if err != nil {
		return Persisted{}, err
	}
	if err = limits.SettleDebitTx(ctx, tx, result, persisted.CostSnapshots); err != nil {
		return Persisted{}, err
	}
	return persisted, nil
}
