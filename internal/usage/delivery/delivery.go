package delivery

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/completeness"
)

// Delivery is the bounded handoff between a finished request and durable
// delivery. Emitting never blocks and never fails a request: an overflow is
// counted, timestamped, and later reported as a gap, because accounting that
// silently forgets what it dropped is worse than accounting that admits it.
type Delivery struct {
	pool     *pgxpool.Pool
	instance string
	counters *completeness.LossCounters
	log      *slog.Logger

	events chan usage.Event

	// intake guards the transition from open to closed. A reader is one
	// in-flight enqueue; the writer closes the channel, so the lock is what
	// makes "send on a closed channel" impossible without making enqueue block
	// on a slow drain.
	intake sync.RWMutex
	closed bool

	processEpoch string
	startedAt    time.Time

	accepted    atomic.Int64
	persisted   atomic.Int64
	dropped     atomic.Int64
	abandoned   atomic.Int64
	retrying    atomic.Bool
	unclean     atomic.Bool
	firstLossMS atomic.Int64
	lastLossMS  atomic.Int64
}

func NewDelivery(pool *pgxpool.Pool, instance string, capacity int, counters *completeness.LossCounters, log *slog.Logger) *Delivery {
	return &Delivery{
		pool: pool, instance: instance, counters: counters, log: log,
		events:       make(chan usage.Event, max(capacity, 1)),
		processEpoch: uuid.Must(uuid.NewV7()).String(),
		startedAt:    time.Now().UTC(),
	}
}

func (d *Delivery) Emit(event usage.Event) error {
	if _, err := usage.Validate(&event); err != nil {
		d.Reject(event.RequestID, err)
		return err
	}
	if err := d.enqueue(event); err != nil {
		d.checkpointLoss(event.RequestID)
		d.log.Warn("usage event not queued", "request_id", event.RequestID, "error", err.Error())
		return err
	}
	return nil
}

func (d *Delivery) Reject(requestID string, cause error) {
	d.countLoss(&d.dropped, 1)
	d.checkpointLoss(requestID)
	d.log.Warn("usage event dropped", "request_id", requestID, "error", cause.Error())
}

func (d *Delivery) checkpointLoss(requestID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := completeness.RecordBudgetLoss(ctx, d.pool, d, d.instance, d.counters); err != nil {
		d.log.Error("accounting loss checkpoint failed", "request_id", requestID, "error", err)
	}
}

func (d *Delivery) Check(ctx context.Context) error {
	return completeness.CheckBudgetAccounting(ctx, d.pool, d)
}

type DeliveryRun struct {
	delivery   *Delivery
	writerDone <-chan struct{}
	reportDone <-chan struct{}
	stop       context.CancelFunc
}

func (d *Delivery) Start(ctx context.Context, client StreamWriter, stream string) (*DeliveryRun, error) {
	if _, err := completeness.CheckpointEpoch(ctx, d.pool, d.instance, d.Snapshot(), false); err != nil {
		return nil, err
	}
	d.log.Info("request metadata gateway registered", "gateway_instance", d.instance)
	running, stop := context.WithCancel(context.WithoutCancel(ctx))
	writerDone, reportDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(writerDone)
		d.runWriter(running, client, stream, d.log)
	}()
	go func() {
		defer close(reportDone)
		completeness.RunLossReporter(running, d.pool, d, d.instance, d.counters, d.log)
	}()
	return &DeliveryRun{delivery: d, writerDone: writerDone, reportDone: reportDone, stop: stop}, nil
}

func (r *DeliveryRun) Drain(ctx context.Context) error {
	r.delivery.closeIntake()
	select {
	case <-r.writerDone:
	case <-ctx.Done():
		r.stop()
		return ctx.Err()
	}
	r.stop()
	select {
	case <-r.reportDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
