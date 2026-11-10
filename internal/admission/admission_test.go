package admission

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/synctest"
	"time"

	"github.com/tyk-swe/olp/internal/priority"
)

func TestPoolAdmitsUpToCapacityThenQueuesUpToDepth(t *testing.T) {
	pool := NewPool(2).Queue(1, time.Second)
	first, second := pool.Enter(), pool.Enter()
	if first.Queued() || second.Queued() || pool.Admitted() != 2 {
		t.Fatalf("two requests under capacity: queued=%v,%v admitted=%d", first.Queued(), second.Queued(), pool.Admitted())
	}
	queued := pool.Enter()
	if !queued.Queued() {
		t.Fatal("the request over capacity did not take the queue position")
	}
	if pool.Enter() != nil || pool.AcquirePermit() != nil {
		t.Error("a request beyond the queue depth was admitted")
	}
	queued.Release()
	if !pool.Enter().Queued() {
		t.Error("the abandoned queue position was not reusable")
	}

	first.Release()
	first.Release()
	if pool.Admitted() != 1 {
		t.Errorf("admitted=%d after one request released twice, want 1", pool.Admitted())
	}
	if pool.AcquirePermit() == nil || pool.AcquirePermit() != nil {
		t.Error("releasing one request twice changed how many requests fit")
	}
}

func TestTransferLeavesTheSlotWithOneOwner(t *testing.T) {
	pool := NewPool(1)
	original := pool.AcquirePermit()
	moved := original.Transfer()
	original.Release()
	if pool.Admitted() != 1 || pool.AcquirePermit() != nil {
		t.Fatal("the previous owner released a transferred slot")
	}
	if original.Transfer() != nil {
		t.Error("a slot was transferred twice")
	}
	moved.Release()
	if pool.Admitted() != 0 || pool.AcquirePermit() == nil {
		t.Error("the new owner could not release the slot")
	}
}

func TestReleasedSlotPassesToTheWaitingRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := NewPool(1).Queue(1, time.Minute)
		holder, waiter := pool.Enter(), pool.Enter()
		admitted := make(chan bool)
		go func() { admitted <- waiter.Await(t.Context(), Normal, time.Time{}) }()
		synctest.Wait()

		holder.Release()
		synctest.Wait()
		if pool.AcquirePermit() != nil {
			t.Error("a new arrival took the slot ahead of the waiting request")
		}
		if !<-admitted || waiter.Queued() || pool.Admitted() != 1 {
			t.Errorf("the waiting request did not receive the slot: queued=%v admitted=%d", waiter.Queued(), pool.Admitted())
		}
		arrival := pool.Enter()
		if !arrival.Queued() {
			t.Error("the admitted request kept its queue position")
		}
		arrival.Release()
		waiter.Release()
		if pool.Admitted() != 0 || pool.AcquirePermit() == nil {
			t.Error("the slot was not free after the last request left")
		}
	})
}

func TestWaitingRequestGivesUpAtTheEarliestLimit(t *testing.T) {
	const queueTimeout = 10 * time.Second
	for name, c := range map[string]struct {
		deadline, cancel, want time.Duration
	}{
		"queue timeout":    {want: queueTimeout},
		"request deadline": {deadline: 3 * time.Second, want: 3 * time.Second},
		"late deadline":    {deadline: time.Minute, want: queueTimeout},
		"cancellation":     {cancel: 2 * time.Second, want: 2 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				pool := NewPool(1).Queue(1, queueTimeout)
				holder, waiter := pool.Enter(), pool.Enter()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if c.cancel > 0 {
					time.AfterFunc(c.cancel, cancel)
				}
				started := time.Now()
				deadline := time.Time{}
				if c.deadline > 0 {
					deadline = started.Add(c.deadline)
				}
				if waiter.Await(ctx, Normal, deadline) {
					t.Fatal("the request was admitted while the pool was full")
				}
				if waited := time.Since(started); waited != c.want {
					t.Errorf("gave up after %s, want %s", waited, c.want)
				}
				waiter.Release()
				holder.Release()
				if pool.Admitted() != 0 {
					t.Errorf("admitted=%d, the released slot went to a request that gave up", pool.Admitted())
				}
				if pool.Enter().Queued() || !pool.Enter().Queued() {
					t.Error("the pool did not return to one free slot and one free queue position")
				}
			})
		})
	}
}

func TestQueueSharesSlotsByClassWeight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		classes := priority.Classes()
		cycle := 0
		for _, class := range classes {
			cycle += class.Weight
		}
		pool := NewPool(1).Queue(cycle*len(classes), time.Minute)
		holder := pool.Enter()
		served := make(chan Class, cycle*len(classes))
		for class := range classes {
			for range cycle {
				permit := pool.Enter()
				go func() {
					if permit.Await(t.Context(), Class(class), time.Time{}) {
						served <- Class(class)
					}
					permit.Release()
				}()
			}
		}
		synctest.Wait()
		holder.Release()
		synctest.Wait()
		if len(served) != cap(served) {
			t.Fatalf("%d of %d waiting requests were served", len(served), cap(served))
		}
		var share [priority.Count]int
		for range cycle {
			share[<-served]++
		}
		for class, want := range classes {
			if share[class] != want.Weight {
				t.Errorf("%s requests took %d of the first %d slots, want %d", want.Name, share[class], cycle, want.Weight)
			}
		}
	})
}

func TestPublicKeepsSurfacesFromStarvingEachOther(t *testing.T) {
	type rejection struct{ surface, retryAfter string }
	serve := func(inference, management *Pool, inferenceEnabled bool, path string) (int, rejection) {
		rejected := make(chan rejection, 1)
		public := &Public{Inference: inference, Management: management, InferenceEnabled: inferenceEnabled, Reject: func(w http.ResponseWriter, _ *http.Request, surface string) {
			rejected <- rejection{surface, w.Header().Get("Retry-After")}
			w.WriteHeader(http.StatusTooManyRequests)
		}}
		recorder := httptest.NewRecorder()
		public.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if PermitFromContext(r.Context()) == nil {
				w.WriteHeader(http.StatusInternalServerError)
			}
		})).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		select {
		case r := <-rejected:
			return recorder.Code, r
		default:
			return recorder.Code, rejection{}
		}
	}
	const inferencePath, managementPath = "/v1/chat/completions", "/api/v1/projects"
	full := func() *Pool {
		pool := NewPool(1)
		pool.AcquirePermit()
		return pool
	}

	idle := NewPool(1)
	if code, _ := serve(idle, full(), true, inferencePath); code != http.StatusOK {
		t.Errorf("inference answered %d while only management was full", code)
	}
	if idle.Admitted() != 0 {
		t.Errorf("a finished inference request still holds %d slots", idle.Admitted())
	}
	if code, got := serve(idle, full(), true, managementPath); code != http.StatusTooManyRequests || got != (rejection{"management", RetryAfterOverload}) {
		t.Errorf("management answered %d %+v while full", code, got)
	}
	if code, _ := serve(full(), NewPool(1), true, managementPath); code != http.StatusOK {
		t.Errorf("management answered %d while only inference was full", code)
	}
	if code, got := serve(full(), NewPool(1), true, inferencePath); code != http.StatusTooManyRequests || got != (rejection{"openai", RetryAfterOverload}) {
		t.Errorf("inference answered %d %+v while full", code, got)
	}
	if code, _ := serve(NewPool(1), full(), false, inferencePath); code != http.StatusTooManyRequests {
		t.Errorf("a process without inference answered %d from the idle inference pool", code)
	}
}
