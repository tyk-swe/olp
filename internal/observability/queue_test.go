package observability

import (
	"context"
	"sync"
	"testing"
	"time"
)

// queued fills the one-slot pool and parks one waiter per class entry,
// returning the order in which their slots were handed over.
func queued(t *testing.T, classes []int) []int {
	t.Helper()
	pool := NewPool(1).Queue(len(classes), time.Minute)
	holder := pool.Enter()
	if holder == nil || holder.Queued() {
		t.Fatal("the first request did not take the free slot")
	}
	var (
		mu    sync.Mutex
		order []int
		done  sync.WaitGroup
	)
	for i, class := range classes {
		permit := pool.Enter()
		if !permit.Queued() {
			t.Fatal("a request beyond capacity was not queued")
		}
		done.Go(func() {
			if !permit.Await(context.Background(), class, time.Time{}) {
				t.Error("a queued request lost its slot")
				return
			}
			mu.Lock()
			order = append(order, class)
			mu.Unlock()
			permit.Release()
		})
		waitFor(t, func() bool { return pool.queue.waiting.Load() == int64(i+1) })
	}
	holder.Release()
	done.Wait()
	return order
}

func waitFor(t *testing.T, ready func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ready(); {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the queue")
		}
		time.Sleep(time.Millisecond)
	}
}

// In the first fifteen handoffs the 8:4:2:1 weights give every class its
// share, so even the low class is served before critical drains.
