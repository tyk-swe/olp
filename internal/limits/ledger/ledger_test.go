package ledger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const testID = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607"

type unavailable struct {
	pgx.Tx
	err error
}

func (tx unavailable) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, tx.err
}

func TestAggregateDeltasRefuseUnavailableAccounting(t *testing.T) {
	down := errors.New("ledger unavailable")
	attempt := testID
	snapshots, err := AddAggregateDeltas(t.Context(), unavailable{err: down}, AggregateEvent{EventID: testID, AttemptID: &attempt, KeyID: testID, Route: "test", At: time.Now()})
	if !errors.Is(err, down) || len(snapshots) != 0 {
		t.Fatalf("unavailable attempt accounting was accepted: %v %v", snapshots, err)
	}
}
