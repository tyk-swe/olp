package stream

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/evidence"
	"github.com/tyk-swe/olp/internal/export/destination"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	interval      = 5 * time.Second
	passBudget    = 20 * time.Second
	claimWindow   = 30 * time.Second
	boundedWindow = 2 * time.Second
	claimBatch    = 25
	maintainBatch = 500
)

type Worker struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	Sender       *destination.Sender
	Log          *slog.Logger

	attempts atomic.Int64
	failed   atomic.Int64
	nextPair atomic.Int64
}

func (w *Worker) Run(ctx context.Context) {
	workerhealth.Run(ctx, workerhealth.Loop{
		Task: workerhealth.TaskExportDelivery, Interval: interval, Budget: passBudget,
		Health: w.Pool, Log: w.Log, Pass: w.pass,
	})
}

type pair struct {
	sink, stream string
}

func (w *Worker) pass(ctx context.Context) (bool, error) {
	maintain, done := context.WithTimeout(context.WithoutCancel(ctx), boundedWindow)
	_, err := w.Pool.Exec(maintain, maintainSQL, maintainBatch)
	done()
	if err != nil {
		return false, err
	}
	rows, err := w.Pool.Query(ctx, activePairsSQL)
	if err != nil {
		return false, err
	}
	pairs, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (pair, error) {
		var p pair
		return p, row.Scan(&p.sink, &p.stream)
	})
	if err != nil || len(pairs) == 0 {
		return false, err
	}
	start := int(w.nextPair.Load() % int64(len(pairs)))
	progress := false
	var errs []error
	for i := range pairs {
		if ctx.Err() != nil {
			break
		}
		index := (start + i) % len(pairs)
		w.nextPair.Store(int64(index + 1))
		worked, err := w.drain(ctx, pairs[index])
		if err != nil {
			w.Log.Warn("export stream pass failed", "sink", pairs[index].sink, "stream", pairs[index].stream, "code", "database")
		}
		progress = progress || worked
		errs = append(errs, err)
	}
	return progress, errors.Join(errs...)
}

func (w *Worker) drain(ctx context.Context, p pair) (bool, error) {
	for i := range claimBatch {
		resolved, err := w.deliverOne(ctx, p)
		if err != nil || !resolved {
			return i > 0, err
		}
	}
	return true, nil
}

type step int

const (
	idle step = iota
	rejected
	claimed
)

type claim struct {
	target   destination.Target
	loadErr  error
	record   destination.Record
	attempts int
}

func (w *Worker) deliverOne(ctx context.Context, p pair) (bool, error) {
	if ctx.Err() != nil {
		return false, nil
	}
	s, c, err := w.claim(ctx, p)
	if err != nil || s == idle {
		return false, err
	}
	if s == rejected {
		return true, nil
	}
	w.attempts.Add(1)
	sendErr := w.send(ctx, c)
	if sendErr != nil {
		w.failed.Add(1)
		w.Log.Warn("export delivery failed", "sink", p.sink, "stream", p.stream, "record", c.record.ID, "code", destination.DeliveryErrorCode(sendErr))
	}
	return true, w.finish(ctx, p, c, sendErr)
}

func (w *Worker) claim(ctx context.Context, p pair) (step, claim, error) {
	claimCtx, cancel := context.WithTimeout(ctx, claimWindow)
	defer cancel()
	tx, err := w.Pool.Begin(claimCtx)
	if err != nil {
		return idle, claim{}, err
	}
	defer tx.Rollback(claimCtx)
	var locked bool
	if err := tx.QueryRow(claimCtx, lockStreamSQL, p.sink+":"+p.stream).Scan(&locked); err != nil || !locked {
		return idle, claim{}, err
	}
	var sinkType destination.Type
	var format destination.Format
	var url, creatorID string
	var settings json.RawMessage
	var credentialID *string
	err = tx.QueryRow(claimCtx, readSinkSQL, p.sink, p.stream).Scan(&sinkType, &format, &url, &settings, &credentialID, &creatorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return idle, claim{}, nil
	}
	if err != nil {
		return idle, claim{}, err
	}
	var c claim
	var sourceID, projectID *string
	err = tx.QueryRow(claimCtx, nextRecordSQL, p.sink, p.stream).Scan(&c.record.ID, &c.record.At, &c.record.Data, &c.attempts, &sourceID, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return idle, claim{}, nil
	}
	if err != nil {
		return idle, claim{}, err
	}
	admitted, err := evidence.Admit(claimCtx, tx, p.stream, sourceID, projectID, creatorID)
	if err != nil {
		return idle, claim{}, err
	}
	if !admitted {
		if _, err := tx.Exec(claimCtx, rejectEvidenceSQL, p.sink, c.record.ID); err != nil {
			return idle, claim{}, err
		}
		return rejected, claim{}, tx.Commit(claimCtx)
	}
	spec, err := destination.ParseSpec(sinkType, format, settings)
	if err != nil {
		return idle, claim{}, err
	}
	credential, err := destination.ReadCredential(claimCtx, tx, w.Keys, w.Installation, credentialID)
	if err != nil {
		return idle, claim{}, err
	}
	tag, err := tx.Exec(claimCtx, claimRecordSQL, p.sink, c.record.ID, c.attempts)
	if err != nil || tag.RowsAffected() != 1 {
		return idle, claim{}, err
	}
	c.record.Stream = p.stream
	c.record.ObjectKey = p.stream + "/" + c.record.At.UTC().Format(time.DateOnly) + "/" + c.record.ID + ".jsonl"
	c.target, c.loadErr = destination.Load(p.sink, url, spec, credential)
	return claimed, c, tx.Commit(claimCtx)
}

func (w *Worker) send(ctx context.Context, c claim) error {
	if c.loadErr != nil {
		return c.loadErr
	}
	send, cancel := context.WithTimeout(ctx, destination.DeliveryTimeout)
	defer cancel()
	return w.Sender.Send(send, c.target, c.record)
}

func (w *Worker) finish(ctx context.Context, p pair, c claim, sendErr error) error {
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), boundedWindow)
	defer cancel()
	tx, err := w.Pool.Begin(finish)
	if err != nil {
		return err
	}
	defer tx.Rollback(finish)
	success := sendErr == nil
	tag, err := tx.Exec(finish, finishRecordSQL, p.sink, c.record.ID, success, destination.DeliveryErrorCode(sendErr), backoffSeconds(c.attempts+1), destination.DeliveryRejected(sendErr))
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("export pending row already resolved")
	}
	cursor, delivered, failed := any(nil), 0, 1
	if success {
		cursor, delivered, failed = c.record.ID, 1, 0
	}
	if _, err := tx.Exec(finish, checkpointCursorSQL, p.sink, p.stream, cursor, delivered, failed); err != nil {
		return err
	}
	return tx.Commit(finish)
}

func backoffSeconds(attempts int) int {
	return min(60<<min(max(attempts-1, 0), 6), 3600)
}
