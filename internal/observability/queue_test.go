package observability

import (
	"context"
	"strings"
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

func TestAdmissionQueueServesClassesWeightedFairWithoutStarvation(t *testing.T) {
	var classes []int
	for range 8 {
		classes = append(classes, ClassLow, ClassNormal, ClassHigh, ClassCritical)
	}
	order := queued(t, classes)
	// In the first fifteen handoffs the 8:4:2:1 weights give every class its
	// share, so even the low class is served before critical drains.
	served := map[int]int{}
	for _, class := range order[:15] {
		served[class]++
	}
	if served[ClassCritical] != 8 || served[ClassHigh] != 4 || served[ClassNormal] != 2 || served[ClassLow] != 1 {
		t.Fatalf("first fifteen handoffs served %v, want 8:4:2:1", served)
	}
	if len(order) != len(classes) {
		t.Fatalf("served %d of %d queued requests", len(order), len(classes))
	}
}

func TestAdmissionQueueRefusesBeyondItsDepthAndAfterItsTimeout(t *testing.T) {
	pool := NewPool(1).Queue(1, 20*time.Millisecond)
	holder := pool.Enter()
	waiting := pool.Enter()
	if !waiting.Queued() {
		t.Fatal("the queue did not take a request while it had room")
	}
	if pool.Enter() != nil {
		t.Fatal("a full queue accepted another request")
	}
	started := time.Now()
	if waiting.Await(context.Background(), ClassHigh, time.Time{}) {
		t.Fatal("a request was admitted while the slot stayed held")
	}
	if waited := time.Since(started); waited < 20*time.Millisecond {
		t.Fatalf("the queue gave up after %s, before its timeout", waited)
	}
	waiting.Release()
	if pool.Enter() == nil {
		t.Fatal("a released queue position was not returned")
	}
	holder.Release()

	var body strings.Builder
	pool.queue.metrics(&body)
	if !strings.Contains(body.String(), `olp_admission_queue_rejections_total{class="high"} 1`) {
		t.Fatalf("the expired wait was not counted:\n%s", body.String())
	}
}

func TestAdmissionQueueNeverWaitsPastTheRouteDeadline(t *testing.T) {
	pool := NewPool(1).Queue(1, time.Minute)
	holder := pool.Enter()
	defer holder.Release()
	waiting := pool.Enter()
	defer waiting.Release()
	started := time.Now()
	if waiting.Await(context.Background(), ClassCritical, started.Add(20*time.Millisecond)) {
		t.Fatal("a request was admitted while the slot stayed held")
	}
	if waited := time.Since(started); waited > 5*time.Second {
		t.Fatalf("the queue waited %s, past the request deadline", waited)
	}
}

func TestReleasedSlotsPassToWaitersWithoutLeaking(t *testing.T) {
	pool := NewPool(2).Queue(64, time.Minute)
	var wg sync.WaitGroup
	for i := range 256 {
		wg.Go(func() {
			permit := pool.Enter()
			if permit == nil {
				return
			}
			defer permit.Release()
			if permit.Await(context.Background(), i%classCount, time.Time{}) && pool.Admitted() > 2 {
				t.Error("more requests ran than the pool admits")
			}
		})
	}
	wg.Wait()
	if pool.Admitted() != 0 || pool.queue.reserved.Load() != 0 || pool.queue.waiting.Load() != 0 {
		t.Fatalf("the pool leaked: admitted %d, reserved %d, waiting %d", pool.Admitted(), pool.queue.reserved.Load(), pool.queue.waiting.Load())
	}
}
