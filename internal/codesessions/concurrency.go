package codesessions

import (
	"context"
	"net/http"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codemode"
)

const generationLeaseSeconds = (codemode.GenerationTimeout + time.Minute) / time.Second

func codeGeneration(operation string) bool {
	return operation != "connect" && operation != "connect_model" && operation != "prewarm"
}

func chooseCodeAccount(ctx context.Context, tx pgx.Tx, checkCapacity bool, candidates []codeCandidate) (codemode.Candidate, []codemode.Candidate, error) {
	outcomes := make([]codemode.Candidate, len(candidates))
	for index, candidate := range candidates {
		outcomes[index] = candidate.outcome
	}
	for {
		chosen, ok := codemode.ChooseCandidate(outcomes)
		if !ok || !chosen.Eligible() || !checkCapacity {
			return chosen, outcomes, nil
		}
		available, err := lockCodeCapacity(ctx, tx, chosen.AccountID)
		if err != nil {
			return chosen, outcomes, err
		}
		if available {
			return chosen, outcomes, nil
		}
		index := slices.Index(outcomes, chosen)
		outcomes[index].Busy = true
	}
}

func lockCodeCapacity(ctx context.Context, tx pgx.Tx, account string) (bool, error) {
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer savepoint.Rollback(ctx)
	var cap *int
	if err := savepoint.QueryRow(ctx, `SELECT max_concurrency FROM olp.code_accounts WHERE id=$1 FOR NO KEY UPDATE`, account).Scan(&cap); err != nil {
		return false, err
	}
	if cap == nil {
		return true, savepoint.Commit(ctx)
	}
	// Take a new snapshot after locking the account so it includes the last admission.
	var full bool
	err = savepoint.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM olp.code_attempts
		WHERE account_id=$1 AND finished_at IS NULL AND operation<>'prewarm'
		AND created_at>statement_timestamp()-make_interval(secs=>$2) OFFSET $3)`, account, float64(generationLeaseSeconds), *cap-1).Scan(&full)
	if err != nil {
		return false, err
	}
	if full {
		return false, savepoint.Rollback(ctx)
	}
	return true, savepoint.Commit(ctx)
}

func codeAccountBusy() error {
	retry := time.Now().Add(time.Second)
	return &codemode.Refusal{Status: http.StatusTooManyRequests, Code: "code_account_busy", RetryAt: &retry}
}
