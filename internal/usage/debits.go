package usage

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
)

// PersistDebitResultTx is the single financial commit boundary used by
// lifecycle owners. It retains ordinary usage receipt admission and pricing;
// reservations are reduced only after matching observed facts are durable.
func PersistDebitResultTx(ctx context.Context, tx pgx.Tx, event *Event, payload []byte, result limits.DebitResult) (Persisted, error) {
	if event == nil || event.EventID != result.EventID {
		return Persisted{}, limits.ErrDebitConflict
	}
	var key, run, step string
	var affinity limits.ExecutionAffinity
	if err := tx.QueryRow(ctx, `SELECT api_key_id::text,COALESCE(run_id::text,''),COALESCE(step_id::text,''),affinity FROM olp.execution_debits WHERE id=$1 FOR UPDATE`, result.DispatchID).Scan(&key, &run, &step, &affinity); err != nil {
		return Persisted{}, err
	}
	if event.APIKeyID != key || event.RuntimeGenerationID != affinity.ReleaseID || len(event.Attempts) == 0 {
		return Persisted{}, limits.ErrDebitConflict
	}
	for _, attempt := range event.Attempts {
		ref := attempt.Execution
		if ref == nil || ref.Validate() != nil || ref.DispatchID != result.DispatchID || ref.RunID != run || ref.StepID != step || ref.TargetID != affinity.TargetID || ref.RouteRevisionID != affinity.RouteRevisionID {
			return Persisted{}, limits.ErrDebitConflict
		}
		if affinity.ProviderID != "" && attempt.ProviderID != affinity.ProviderID {
			return Persisted{}, limits.ErrDebitConflict
		}
		if affinity.ProviderRevisionID != "" && (attempt.Routing == nil || attempt.Routing.ProviderRevisionID != affinity.ProviderRevisionID) {
			return Persisted{}, limits.ErrDebitConflict
		}
		if affinity.CredentialID != "" || affinity.SlotID != "" || affinity.PricingRevisionID != "" {
			if attempt.Routing == nil {
				return Persisted{}, limits.ErrDebitConflict
			}
			for _, pin := range []struct {
				expected string
				actual   *string
			}{{affinity.CredentialID, attempt.Routing.CredentialVersionID}, {affinity.SlotID, attempt.Routing.CredentialSlotID}, {affinity.PricingRevisionID, attempt.Routing.PricingRevisionID}} {
				if pin.expected != "" && (pin.actual == nil || *pin.actual != pin.expected) {
					return Persisted{}, limits.ErrDebitConflict
				}
			}
		}
	}
	persisted, err := PersistEventTx(ctx, tx, event, payload)
	if err != nil {
		return Persisted{}, err
	}
	if err = limits.SettleDebitTx(ctx, tx, result); err != nil {
		return Persisted{}, err
	}
	return persisted, nil
}
