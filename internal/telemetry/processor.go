package telemetry

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type countingExporter struct {
	inner   sdktrace.SpanExporter
	dropped prometheus.Counter
}

func (c *countingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	err := c.inner.ExportSpans(ctx, spans)
	if err != nil {
		c.dropped.Add(float64(len(spans)))
	}
	return err
}

func (c *countingExporter) Shutdown(ctx context.Context) error { return c.inner.Shutdown(ctx) }

// boundedSpanProcessor exists because the SDK batch processor blocks or drops
// silently. Hand-off here never blocks and every drop is counted, so a
// stalled collector cannot backpressure inference work.
type boundedSpanProcessor struct {
	spans    chan sdktrace.ReadOnlySpan
	controls chan processorControl
	stopped  atomic.Bool
	done     chan struct{}
	dropped  prometheus.Counter
}

type controlKind int

const (
	controlFlush controlKind = iota
	controlShutdown
)

type processorControl struct {
	kind controlKind
	ack  chan error
}

func newBoundedSpanProcessor(exporter sdktrace.SpanExporter, dropped prometheus.Counter) *boundedSpanProcessor {
	p := &boundedSpanProcessor{
		spans:    make(chan sdktrace.ReadOnlySpan, exportQueueCapacity),
		controls: make(chan processorControl, 16),
		done:     make(chan struct{}),
		dropped:  dropped,
	}
	go (&exportWorker{exporter: exporter, spans: p.spans, controls: p.controls, done: p.done}).run()
	return p
}

func (p *boundedSpanProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (p *boundedSpanProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	if !span.SpanContext().IsSampled() {
		return
	}
	select {
	case p.spans <- span:
	default:
		p.dropped.Inc()
	}
}

func (p *boundedSpanProcessor) ForceFlush(ctx context.Context) error {
	if p.stopped.Load() {
		return errors.New("telemetry: trace export worker is shut down")
	}
	return p.send(ctx, controlFlush)
}

func (p *boundedSpanProcessor) Shutdown(ctx context.Context) error {
	if p.stopped.Swap(true) {
		return nil
	}
	return p.send(ctx, controlShutdown)
}

func (p *boundedSpanProcessor) send(ctx context.Context, kind controlKind) error {
	ack := make(chan error, 1)
	select {
	case p.controls <- processorControl{kind: kind, ack: ack}:
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
		return errors.New("telemetry: trace export worker exited before answering")
	}
}

type exportWorker struct {
	exporter sdktrace.SpanExporter
	spans    chan sdktrace.ReadOnlySpan
	controls chan processorControl
	pending  []sdktrace.ReadOnlySpan
	done     chan struct{}
}

func (w *exportWorker) run() {
	defer close(w.done)
	ticker := time.NewTicker(exportScheduleDelay)
	defer ticker.Stop()
	for {
		select {
		case control := <-w.controls:
			if control.kind == controlShutdown {
				control.ack <- w.shutdown()
				return
			}
			control.ack <- w.drain(context.Background())
		case span := <-w.spans:
			w.push(context.Background(), span)
		case <-ticker.C:
			w.exportPending(context.Background())
		}
	}
}

// push errors are already counted as drops by countingExporter.
func (w *exportWorker) push(ctx context.Context, span sdktrace.ReadOnlySpan) error {
	w.pending = append(w.pending, span)
	if len(w.pending) < exportBatchSize {
		return nil
	}
	return w.exportPending(ctx)
}

func (w *exportWorker) exportPending(ctx context.Context) error {
	if len(w.pending) == 0 {
		return nil
	}
	batch := w.pending
	w.pending = nil
	return w.exporter.ExportSpans(ctx, batch)
}

func (w *exportWorker) drain(ctx context.Context) error {
	for {
		select {
		case span := <-w.spans:
			if err := w.push(ctx, span); err != nil {
				return errors.Join(err, w.drain(ctx))
			}
		default:
			return w.exportPending(ctx)
		}
	}
}

func (w *exportWorker) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), exportTimeout)
	defer cancel()
	return errors.Join(w.drain(ctx), w.exporter.Shutdown(ctx))
}
