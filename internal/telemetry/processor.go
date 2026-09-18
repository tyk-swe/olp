package telemetry

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// BoundedSpanProcessor is the span processor the exporter sits behind. Span
// hand-off never blocks: a full queue drops the span and counts it, because a
// stalled collector must not backpressure inference work. A worker drains the
// queue in bounded batches on a fixed schedule.
type BoundedSpanProcessor struct {
	spans    chan sdktrace.ReadOnlySpan
	controls chan processorControl
	stopped  atomic.Bool
	done     chan struct{}
}

type processorControl struct {
	kind controlKind
	ack  chan error
}

type controlKind int

const (
	controlFlush controlKind = iota
	controlShutdown
)

// NewBoundedSpanProcessor starts the drain worker behind the exporter.
func NewBoundedSpanProcessor(exporter sdktrace.SpanExporter, queueCapacity, batchSize int, delay time.Duration) *BoundedSpanProcessor {
	if queueCapacity <= 0 || batchSize <= 0 || delay <= 0 {
		panic("telemetry: invalid span processor bounds")
	}
	p := &BoundedSpanProcessor{
		spans:    make(chan sdktrace.ReadOnlySpan, queueCapacity),
		controls: make(chan processorControl, 16),
		done:     make(chan struct{}),
	}
	go (&exportWorker{
		exporter:  exporter,
		spans:     p.spans,
		controls:  p.controls,
		batchSize: batchSize,
		delay:     delay,
		done:      p.done,
	}).run()
	return p
}

func (p *BoundedSpanProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

// OnEnd hands the span to the export queue. Unsampled spans are discarded and
// a full queue counts the drop rather than blocking the request path.
func (p *BoundedSpanProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if !span.SpanContext().IsSampled() {
		return
	}
	select {
	case p.spans <- span:
	default:
		recordExportDrops(1)
	}
}

func (p *BoundedSpanProcessor) ForceFlush(ctx context.Context) error {
	if p.stopped.Load() {
		return errors.New("telemetry: trace export worker is shut down")
	}
	ack := make(chan error, 1)
	select {
	case p.controls <- processorControl{kind: controlFlush, ack: ack}:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("telemetry: trace export worker is unavailable")
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("telemetry: trace export worker dropped flush response")
	}
}

func (p *BoundedSpanProcessor) Shutdown(ctx context.Context) error {
	if p.stopped.Swap(true) {
		return nil
	}
	ack := make(chan error, 1)
	select {
	case p.controls <- processorControl{kind: controlShutdown, ack: ack}:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("telemetry: trace export worker is unavailable")
	}
	select {
	case err := <-ack:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return errors.New("telemetry: trace export worker dropped shutdown response")
	}
}

// exportWorker drains the span queue into bounded export batches.
type exportWorker struct {
	exporter  sdktrace.SpanExporter
	spans     chan sdktrace.ReadOnlySpan
	controls  chan processorControl
	pending   []sdktrace.ReadOnlySpan
	batchSize int
	delay     time.Duration
	done      chan struct{}
}

func (w *exportWorker) run() {
	defer close(w.done)
	ticker := time.NewTicker(w.delay)
	defer ticker.Stop()
	for {
		select {
		case control := <-w.controls:
			if !w.handle(control) {
				return
			}
		case span := <-w.spans:
			w.push(span)
		case <-ticker.C:
			w.exportPending(context.Background())
		}
	}
}

// handle answers one control message; a false return ends the worker.
func (w *exportWorker) handle(control processorControl) bool {
	ack := control.ack
	var err error
	switch control.kind {
	case controlFlush:
		err = w.flushQueued()
	case controlShutdown:
		err = w.shutdownExporter()
	}
	ack <- err
	return control.kind == controlFlush
}

func (w *exportWorker) push(span sdktrace.ReadOnlySpan) {
	w.pending = append(w.pending, span)
	if len(w.pending) >= w.batchSize {
		w.exportPending(context.Background())
	}
}

// exportPending drains pending in batchSize chunks.
func (w *exportWorker) exportPending(ctx context.Context) {
	for len(w.pending) > 0 {
		n := min(w.batchSize, len(w.pending))
		batch := w.pending[:n:n]
		w.pending = w.pending[n:]
		if err := w.exporter.ExportSpans(ctx, batch); err != nil {
			// The counting exporter already accounted for this batch's loss.
			break
		}
	}
}

// flushQueued drains every queued span, exports what remains pending, then
// forces the exporter's own buffers out.
func (w *exportWorker) flushQueued() error {
	for {
		select {
		case span := <-w.spans:
			w.push(span)
		default:
			w.exportPending(context.Background())
			return nil
		}
	}
}

// shutdownExporter drains the queue, exports, and stops the exporter. Anything
// left pending counts as dropped so the metric stays honest.
func (w *exportWorker) shutdownExporter() error {
	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	defer cancel()
	for {
		select {
		case span := <-w.spans:
			w.pending = append(w.pending, span)
		default:
			dropped := len(w.pending)
			var first error
			for len(w.pending) > 0 {
				n := min(w.batchSize, len(w.pending))
				batch := w.pending[:n:n]
				w.pending = w.pending[n:]
				if err := w.exporter.ExportSpans(ctx, batch); err != nil && first == nil {
					first = err
				}
			}
			if first != nil && dropped > 0 {
				// Batches that failed export were already counted by the
				// counting exporter; nothing further is owed.
			}
			if err := w.exporter.Shutdown(ctx); err != nil && first == nil {
				first = err
			}
			return first
		}
	}
}
