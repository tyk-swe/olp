//go:build integration

package integration_test

import (
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

func TestAccountingRejectsConcurrentIdentityReuse(t *testing.T) {
	for _, identity := range []string{"event", "request"} {
		t.Run(identity, func(t *testing.T) {
			f := acctSeed(t, acctPool(t))
			events := []*usage.Event{
				acctEvent(t, f, acctEventOptions{Attempts: []usage.Attempt{
					acctAttempt(t, f.Provider, 1, "model", 200, acctObserved(7, 5, nil, nil)),
				}}),
				acctEvent(t, f, acctEventOptions{Attempts: []usage.Attempt{
					acctAttempt(t, f.Provider, 1, "model", 200, acctObserved(7, 5, nil, nil)),
				}}),
			}
			if identity == "event" {
				events[1].EventID = events[0].EventID
			} else {
				events[1].RequestID = events[0].RequestID
			}
			type result struct {
				persisted usage.Persisted
				err       error
			}
			results := make(chan result, 2)
			start := make(chan struct{})
			for _, event := range events {
				payload, err := usage.Encode(event)
				if err != nil {
					t.Fatal(err)
				}
				go func() {
					<-start
					persisted, err := usage.PersistEvent(t.Context(), f.Pool, event, payload)
					results <- result{persisted, err}
				}()
			}
			close(start)
			persisted, rejected := 0, 0
			for range events {
				r := <-results
				switch {
				case r.err == nil && r.persisted.Outcome == usage.PersistOutcomePersisted:
					persisted++
				case errors.Is(r.err, usage.ErrInvalidEvent):
					rejected++
				default:
					t.Fatalf("unexpected persistence outcome: %+v", r)
				}
			}
			if persisted != 1 || rejected != 1 {
				t.Fatalf("persisted/rejected = %d/%d, want 1/1", persisted, rejected)
			}
			if got := acctCount(t, f, "SELECT count(*) FROM olp_go.attempt_usage_facts"); got != 1 {
				t.Fatalf("facts = %d, want one attribution", got)
			}
		})
	}
}

func TestAccountingDoesNotHideChangedPayloadBehindFacts(t *testing.T) {
	f := acctSeed(t, acctPool(t))
	event := acctEvent(t, f, acctEventOptions{Attempts: []usage.Attempt{
		acctAttempt(t, f.Provider, 1, "model", 200, acctObserved(7, 5, nil, nil)),
	}})
	acctPersist(t, f, event)
	event.RouteSlug = "different-route"
	if _, err := acctPersistErr(t, f, event); !errors.Is(err, usage.ErrInvalidEvent) {
		t.Fatalf("changed payload: %v, want an identity conflict", err)
	}
}

func TestReceiptsProtectRequestsWithNoRemainingFacts(t *testing.T) {
	f := acctSeed(t, acctPool(t))
	original := acctEvent(t, f, acctEventOptions{})
	acctPersist(t, f, original)
	for _, identity := range []string{"event", "request"} {
		conflict := acctEvent(t, f, acctEventOptions{})
		if identity == "event" {
			conflict.EventID = original.EventID
		} else {
			conflict.RequestID = original.RequestID
		}
		if _, err := acctPersistErr(t, f, conflict); !errors.Is(err, usage.ErrInvalidEvent) {
			t.Fatalf("%s identity without facts: %v", identity, err)
		}
	}
}

func TestRequestFiltersSurviveUsageFactRetention(t *testing.T) {
	f := acctSeed(t, acctPool(t))
	model := "retained-model"
	event := acctEvent(t, f, acctEventOptions{Attempts: []usage.Attempt{
		acctAttempt(t, f.Provider, 1, model, 200, acctObserved(7, 5, nil, nil)),
	}})
	acctPersist(t, f, event)
	// Usage retention may remove facts before request/attempt history expires.
	acctExec(t, f.Pool, "DELETE FROM olp_go.attempt_usage_facts WHERE request_id=$1", event.RequestID)
	rows, _, err := usage.ListRequests(t.Context(), f.Pool,
		usage.RequestFilters{ProviderID: &f.Provider, Model: &model, AllProjects: true}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ID != event.RequestID {
		t.Fatalf("retained filtered history = %+v", rows)
	}
}

func TestPricingUsesTheRecordedRevisionVendor(t *testing.T) {
	f := acctSeed(t, acctPool(t))
	provider, revision := acctProvider(t, f, "openai_compatible",
		`{"kind":"openai_compatible","auth_mode":"none","endpoint":"https://example.com/v1","options":{}}`)
	acctPricing(t, f, 1, time.Now().UTC().Add(-time.Hour),
		acctPrice{Kind: "openai_compatible", Model: "model", Operation: "generation",
			Input: acctPtr("1"), Output: acctPtr("1")},
		acctPrice{Kind: "openai_compatible", VendorID: acctPtr("openrouter"),
			Model: "model", Operation: "generation", Input: acctPtr("100"), Output: acctPtr("100")})
	attempt := acctAttempt(t, provider, 1, "model", 200, acctObserved(7, 5, nil, nil))
	attempt.Routing = &usage.Routing{ProviderRevisionID: revision}
	event := acctEvent(t, f, acctEventOptions{Attempts: []usage.Attempt{attempt}})
	acctExec(t, f.Pool, `UPDATE olp_go.providers
		SET configuration=$2::json
		WHERE id=$1`, provider,
		`{"kind":"openai_compatible","auth_mode":"none","endpoint":"https://example.com/v1","options":{"vendor_id":"openrouter"}}`)
	acctPersist(t, f, event)
	fact := acctLoadFact(t, f, event.RequestID, 1)
	acctSameMoney(t, f, fact.Cost, "0.000012")
}

func TestExpiredEventGapCoversItsHistoricalWindow(t *testing.T) {
	f := acctSeed(t, acctPool(t))
	at := time.Now().UTC().Add(-8 * 24 * time.Hour)
	event := acctEvent(t, f, acctEventOptions{ObservedAt: at})
	acctPersist(t, f, event)
	summary, err := usage.ReadSummary(t.Context(), f.Pool,
		usage.Filters{Start: at.Add(-time.Minute), End: at.Add(time.Minute), AllProjects: true}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if summary.UncertainGapCount != 1 || summary.Complete {
		t.Fatalf("expired event disappeared from historical completeness: %+v", summary)
	}
}
