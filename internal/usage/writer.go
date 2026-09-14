package usage

import (
	"context"
	"log/slog"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
)

const (
	// streamWriteTimeout bounds one XADD. A Valkey that has stopped answering
	// must not hold the buffer hostage: the write is retried instead.
	streamWriteTimeout = time.Second
	writerRetryFloor   = 25 * time.Millisecond
	writerRetryCeiling = 5 * time.Second
)

// RunWriter drains the emitter into the request metadata stream until ctx is
// done, then closes the intake and accounts for whatever is left. It is the
// only reader of the buffer, so exactly one goroutine per emitter runs it.
//
// The context belongs to the process, not to a request: cancelling it is the
// shutdown signal, and everything still buffered at that point is reported as
// lost rather than silently forgotten.
func (e *Emitter) RunWriter(ctx context.Context, client *coordination.Client, stream string, log *slog.Logger) {
	for {
		if ctx.Err() != nil {
			e.abandonAndDrain(0)
			return
		}
		select {
		case <-ctx.Done():
			e.abandonAndDrain(0)
			return
		case event := <-e.events:
			payload, err := Encode(&event)
			if err != nil {
				// One unencodable event (a routing policy that is no longer
				// valid JSON) must not stop every later request from being
				// accounted for. Count the loss and keep draining.
				log.Error("request metadata event could not be encoded", "error", err)
				e.recordLoss(&e.abandoned, 1)
				continue
			}
			if !e.write(ctx, client, stream, payload, log) {
				e.abandonAndDrain(1)
				return
			}
			e.persisted.Add(1)
		}
	}
}

// write retries one event until Valkey accepts it or the process is shutting
// down. It reports whether the event reached the stream.
func (e *Emitter) write(ctx context.Context, client *coordination.Client, stream string, payload []byte, log *slog.Logger) bool {
	backoff := writerRetryFloor
	for {
		// The write deliberately outlives a cancelled ctx: an XADD abandoned in
		// flight has an unknown outcome, and a duplicate delivery costs one
		// idempotent receipt while a lost one costs an unexplained gap.
		write, cancel := context.WithTimeout(context.WithoutCancel(ctx), streamWriteTimeout)
		_, err := client.Do(write, "XADD", stream, "*", "event", string(payload))
		cancel()
		if err == nil {
			e.retrying.Store(false)
			return true
		}
		e.retrying.Store(true)
		log.Warn("request metadata stream write failed; retrying", "error", err)
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
		}
		backoff = min(2*backoff, writerRetryCeiling)
	}
}
