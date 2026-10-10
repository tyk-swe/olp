package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type missingSignalTx struct {
	pgx.Tx
}

func (missingSignalTx) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("missing measurements must not be treated as known recoveries")
}

func TestMissingMeasurementsPreserveActiveIncidents(t *testing.T) {
	for _, event := range []string{"runtime.install_failed", "worker.stale", "provider.credential.failing"} {
		t.Run(event, func(t *testing.T) {
			worker := &notificationWorker{}
			count, err := worker.recoverMissingSignals(t.Context(), missingSignalTx{}, signalRule{event: event}, nil, time.Now())
			if err != nil || count != 0 {
				t.Fatalf("missing measurement recovery = %d,%v", count, err)
			}
		})
	}
}
