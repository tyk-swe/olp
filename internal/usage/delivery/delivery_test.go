package delivery

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tyk-swe/olp/internal/usage/completeness"
)

type fakeLossCheckpoint struct {
	mu       sync.Mutex
	recorded []int64
	calls    atomic.Int64
	current  atomic.Int64
	maximum  atomic.Int64
	started  chan struct{}
	release  chan struct{}
	err      error
	delay    time.Duration
	once     sync.Once
}

func (f *fakeLossCheckpoint) run(d *Delivery) func(context.Context) error {
	return func(context.Context) error {
		f.calls.Add(1)
		now := f.current.Add(1)
		for {
			seen := f.maximum.Load()
			if now <= seen || f.maximum.CompareAndSwap(seen, now) {
				break
			}
		}
		defer f.current.Add(-1)
		f.mu.Lock()
		f.recorded = append(f.recorded, d.Snapshot().Dropped)
		f.mu.Unlock()
		if f.started != nil {
			f.once.Do(func() { close(f.started) })
		}
		if f.release != nil {
			<-f.release
		}
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		return f.err
	}
}

func (f *fakeLossCheckpoint) lastRecorded() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.recorded) == 0 {
		return -1
	}
	return f.recorded[len(f.recorded)-1]
}

func newTestDelivery(fake *fakeLossCheckpoint) *Delivery {
	d := NewDelivery(nil, "test", 1, &completeness.LossCounters{}, slog.New(slog.DiscardHandler))
	d.recordLoss = fake.run(d)
	return d
}

func rejectConcurrently(d *Delivery, losses int) {
	var wg sync.WaitGroup
	for i := range losses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.Reject(fmt.Sprintf("request-%d", i), errors.New("lost"))
		}()
	}
	wg.Wait()
}

func TestCheckpointLossCoalescesConcurrentLosses(t *testing.T) {
	const losses = 32
	fake := &fakeLossCheckpoint{started: make(chan struct{}), release: make(chan struct{})}
	d := newTestDelivery(fake)

	done := make(chan struct{})
	go func() {
		defer close(done)
		rejectConcurrently(d, losses)
	}()
	<-fake.started
	deadline := time.Now().Add(5 * time.Second)
	for d.Snapshot().Dropped < losses {
		if time.Now().After(deadline) {
			t.Fatalf("got %d dropped losses counted, want %d", d.Snapshot().Dropped, losses)
		}
		time.Sleep(time.Millisecond)
	}
	close(fake.release)
	<-done

	if got := fake.maximum.Load(); got != 1 {
		t.Fatalf("got %d concurrent loss checkpoints, want 1", got)
	}
	if got := fake.calls.Load(); got > 3 {
		t.Fatalf("got %d loss checkpoints for %d concurrent losses, want a coalesced few", got, losses)
	}
	if got := fake.lastRecorded(); got != losses {
		t.Fatalf("got last checkpoint covering %d dropped losses, want %d before the handlers completed", got, losses)
	}

	callsBefore := fake.calls.Load()
	d.Reject("request-after", errors.New("lost"))
	if got := fake.calls.Load(); got != callsBefore+1 {
		t.Fatalf("got %d loss checkpoints after one later loss, want %d", got, callsBefore+1)
	}
	if got := fake.lastRecorded(); got != losses+1 {
		t.Fatalf("got follow-up checkpoint covering %d dropped losses, want %d", got, losses+1)
	}
}

func TestCheckpointLossCoalescesFailedCheckpoints(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const losses = 16
		fake := &fakeLossCheckpoint{
			started: make(chan struct{}),
			release: make(chan struct{}),
			err:     errors.New("checkpoint unavailable"),
			delay:   20 * time.Millisecond,
		}
		d := newTestDelivery(fake)

		start := time.Now()
		done := make(chan struct{})
		go func() {
			defer close(done)
			rejectConcurrently(d, losses)
		}()
		<-fake.started
		synctest.Wait()
		if got := d.Snapshot().Dropped; got != losses {
			t.Fatalf("got %d dropped losses counted, want %d", got, losses)
		}
		close(fake.release)
		<-done
		elapsed := time.Since(start)

		if got := fake.maximum.Load(); got != 1 {
			t.Fatalf("got %d concurrent failed loss checkpoints, want 1", got)
		}
		if got := fake.calls.Load(); got > 3 {
			t.Fatalf("got %d failed loss checkpoints for %d concurrent losses, want a coalesced few", got, losses)
		}
		if elapsed >= losses*20*time.Millisecond {
			t.Fatalf("got %d concurrent failed losses taking %s, want far less than one checkpoint wait each", losses, elapsed)
		}
	})
}
