package export

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

type Worker struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	Sender       *Sender
	Log          *slog.Logger
	PassBudget   time.Duration
	DrainPair    func(ctx context.Context, sink, stream string) (bool, error)

	attempts  atomic.Int64
	delivered atomic.Int64
	failed    atomic.Int64
	nextPair  atomic.Int64
}

func (w *Worker) Metrics() (attempts, delivered, failed int64) {
	return w.attempts.Load(), w.delivered.Load(), w.failed.Load()
}

const ExportInterval = 5 * time.Second
const ExportPassTimeout = 20 * time.Second

const claimBatch = 25
const claimLeaseBackoff = 60
const boundedWindow = 2 * time.Second

func backoffSeconds(attempts int) int {
	delay := claimLeaseBackoff
	for i := 1; i < attempts; i++ {
		delay *= 2
		if delay >= 3600 {
			return 3600
		}
	}
	return delay
}

func (w *Worker) passBudget() time.Duration {
	if w.PassBudget > 0 {
		return w.PassBudget
	}
	return ExportPassTimeout
}

func (w *Worker) Pass(ctx context.Context) error {
	pass, cancel := context.WithTimeout(ctx, w.passBudget())
	_, err := w.pass(pass)
	cancel()
	return err
}

func (w *Worker) Run(ctx context.Context) {
	for {
		passCtx, cancel := context.WithTimeout(ctx, w.passBudget())
		progress, err := w.pass(passCtx)
		cancel()
		outcome := usage.OutcomeSuccess
		if err != nil {
			outcome = usage.OutcomeFailure
			w.Log.Warn("export delivery pass failed", "code", "database")
		}
		checkpoint, done := context.WithTimeout(context.WithoutCancel(ctx), boundedWindow)
		if err := usage.CheckpointTask(checkpoint, w.Pool, usage.TaskExportDelivery, outcome, progress); err != nil {
			w.Log.Warn("export delivery checkpoint failed", "code", "database")
		}
		done()
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(ExportInterval):
		}
	}
}

type sinkStream struct {
	sink, stream string
}

func (w *Worker) pass(ctx context.Context) (bool, error) {
	maintain, done := context.WithTimeout(context.WithoutCancel(ctx), boundedWindow)
	_, err := w.Pool.Exec(maintain, maintainSQL, 500)
	done()
	if err != nil {
		return false, err
	}
	var pairs []sinkStream
	rows, err := w.Pool.Query(ctx, activeSinksSQL)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id, typ, destination, format string
		var credential *string
		var streams []string
		if err := rows.Scan(&id, &typ, &destination, &credential, &streams, &format); err != nil {
			rows.Close()
			return false, err
		}
		for _, stream := range streams {
			pairs = append(pairs, sinkStream{id, stream})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	progress := false
	var firstErr error
	drain := w.DrainPair
	if drain == nil {
		drain = w.drainStream
	}
	if n := len(pairs); n != 0 {
		start := int(w.nextPair.Load() % int64(n))
		for i := range n {
			index := (start + i) % n
			if ctx.Err() != nil {
				return progress, firstErr
			}
			w.nextPair.Store(int64(index + 1))
			worked, err := drain(ctx, pairs[index].sink, pairs[index].stream)
			if err != nil {
				firstErr = cmp.Or(firstErr, err)
				w.Log.Warn("export stream pass failed", "sink", pairs[index].sink, "stream", pairs[index].stream, "code", "database")
				continue
			}
			progress = progress || worked
		}
	}
	return progress, firstErr
}

func (w *Worker) drainStream(ctx context.Context, sinkID, stream string) (bool, error) {
	progress := false
	for range claimBatch {
		done, err := w.deliverOne(ctx, sinkID, stream)
		if err != nil {
			return progress, err
		}
		if done {
			return progress, nil
		}
		progress = true
	}
	return true, nil
}

func (w *Worker) deliverOne(ctx context.Context, sinkID, stream string) (bool, error) {
	if ctx.Err() != nil {
		return true, nil
	}
	claimCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tx, err := w.Pool.Begin(claimCtx)
	if err != nil {
		return true, err
	}
	defer tx.Rollback(claimCtx)
	locked := false
	if err = tx.QueryRow(claimCtx, lockStreamSQL, sinkID+":"+stream).Scan(&locked); err != nil || !locked {
		return true, err
	}
	var destination Destination
	var credentialID *string
	if err = tx.QueryRow(claimCtx, readDestinationSQL, sinkID, stream).Scan(&destination.Type, &destination.URL, &credentialID, &destination.Format); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		return true, err
	}
	destination.CredentialID = credentialID
	var credential []byte
	if credentialID != nil {
		if credential, err = access.SecretValueFor(claimCtx, tx, w.Keys, secrets.SinkCredential, w.Installation, *credentialID); err != nil {
			return true, err
		}
	}
	var id, recStream string
	var occurredAt, queuedAt time.Time
	var data []byte
	var attempts int
	err = tx.QueryRow(claimCtx, pendingRecordsSQL, sinkID, stream, 1).Scan(&id, &recStream, &occurredAt, &queuedAt, &data, &attempts)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil
		}
		return true, err
	}
	tag, err := tx.Exec(claimCtx, claimRecordSQL, sinkID, id, attempts)
	if err != nil {
		return true, err
	}
	if tag.RowsAffected() != 1 {
		return true, nil
	}
	if err = tx.Commit(claimCtx); err != nil {
		return true, err
	}
	w.attempts.Add(1)

	sendCtx, sendCancel := context.WithTimeout(ctx, DeliveryTimeout)
	sendErr := w.Sender.Send(sendCtx, destination, credential, Record{ID: id, Stream: recStream, At: queuedAt, Data: json.RawMessage(data)})
	sendCancel()

	finishCtx := ctx
	if finishCtx.Err() != nil {
		finishCtx = context.WithoutCancel(ctx)
	}
	finish, finishCancel := context.WithTimeout(finishCtx, boundedWindow)
	defer finishCancel()
	ftx, err := w.Pool.Begin(finish)
	if err != nil {
		return false, err
	}
	defer ftx.Rollback(finish)
	success := sendErr == nil
	tag, err = ftx.Exec(finish, finishRecordSQL, sinkID, id, success, DeliveryErrorCode(sendErr), backoffSeconds(attempts+1))
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() != 1 {
		return false, errors.New("export pending row already resolved")
	}
	var cursor any
	var deliveredCount, failedCount int
	if success {
		cursor = id
		deliveredCount = 1
		w.delivered.Add(1)
	} else {
		failedCount = 1
		w.failed.Add(1)
	}
	if _, err = ftx.Exec(finish, checkpointCursorSQL, sinkID, stream, cursor, deliveredCount, failedCount); err != nil {
		return false, err
	}
	if err = ftx.Commit(finish); err != nil {
		return false, err
	}
	if sendErr != nil {
		w.Log.Warn("export delivery failed", "sink", sinkID, "stream", stream, "record", id, "code", DeliveryErrorCode(sendErr))
	}
	if ctx.Err() != nil {
		return true, nil
	}
	return false, nil
}
