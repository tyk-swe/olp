package usage

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Delivery struct {
	pool     *pgxpool.Pool
	emitter  *Emitter
	instance string
	counters *LossCounters
	log      *slog.Logger
}

func NewDelivery(pool *pgxpool.Pool, instance string, capacity int, counters *LossCounters, log *slog.Logger) *Delivery {
	return &Delivery{pool: pool, emitter: NewEmitter(capacity), instance: instance, counters: counters, log: log}
}

func (d *Delivery) Emit(event Event) error {
	if _, err := Validate(&event); err != nil {
		d.Reject(event.RequestID, err)
		return err
	}
	if err := d.emitter.Emit(event); err != nil {
		d.recordLoss(event.RequestID)
		d.log.Warn("usage event not queued", "request_id", event.RequestID, "error", err.Error())
		return err
	}
	return nil
}

func (d *Delivery) Reject(requestID string, cause error) {
	d.emitter.Drop()
	d.recordLoss(requestID)
	d.log.Warn("usage event dropped", "request_id", requestID, "error", cause.Error())
}

func (d *Delivery) recordLoss(requestID string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := RecordBudgetLoss(ctx, d.pool, d.emitter, d.instance, d.counters); err != nil {
		d.log.Error("accounting loss checkpoint failed", "request_id", requestID, "error", err)
	}
}

func (d *Delivery) Check(ctx context.Context) error {
	return CheckBudgetAccounting(ctx, d.pool, d.emitter)
}

func (d *Delivery) Snapshot() Snapshot { return d.emitter.Snapshot() }

func (d *Delivery) MarkUnclean() { d.emitter.MarkUnclean() }

type DeliveryRun struct {
	delivery   *Delivery
	writerDone <-chan struct{}
	reportDone <-chan struct{}
	stop       context.CancelFunc
}

func (d *Delivery) Start(ctx context.Context, client StreamWriter, stream string) (*DeliveryRun, error) {
	if _, err := CheckpointEpoch(ctx, d.pool, d.instance, d.emitter.Snapshot(), false); err != nil {
		return nil, err
	}
	d.log.Info("request metadata gateway registered", "gateway_instance", d.instance)
	running, stop := context.WithCancel(context.WithoutCancel(ctx))
	writerDone, reportDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(writerDone)
		d.emitter.RunWriter(running, client, stream, d.log)
	}()
	go func() {
		defer close(reportDone)
		RunLossReporter(running, d.pool, d.emitter, d.instance, d.counters, d.log)
	}()
	return &DeliveryRun{delivery: d, writerDone: writerDone, reportDone: reportDone, stop: stop}, nil
}

func (r *DeliveryRun) Drain(ctx context.Context) error {
	r.delivery.emitter.Close()
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
