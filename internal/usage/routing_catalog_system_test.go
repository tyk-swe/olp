//go:build integration

package usage

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
)

func TestIntegrationRoutingCatalogLoadsCurrentTerms(t *testing.T) {
	pool := notificationPool(t)
	owner, _ := seedParityOwner(t, pool)
	_, project, _ := seedParityProjectKey(t, pool, owner)
	now := time.Now().UTC()
	card, revision := seedParityCard(t, pool, owner, nil, &project, "2", now.Add(-time.Hour))
	seedParityCardPrice(t, pool, revision, Price{ProviderKind: "openai", Model: "model", Operation: "generation", InputPerMillion: rate("7"), Currency: "USD"})
	seedParityDiscount(t, pool, owner, "openai", nil, nil, "0.9", now.Add(-2*time.Hour))
	current := seedParityDiscount(t, pool, owner, "openai", nil, nil, "0.8", now.Add(-time.Hour))
	future := seedParityDiscount(t, pool, owner, "openai", nil, nil, "0.7", now.Add(time.Hour))

	config := pool.Config()
	config.MaxConns = 1
	single, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer single.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	inputs, err := LoadRoutingInputs(ctx, single, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(inputs.Discounts) != 2 || inputs.Discount("openai", "", "", now).ID != current {
		t.Fatalf("current and scheduled discounts missing: %+v", inputs.Discounts)
	}
	inputs.RefreshedAt = now.Add(time.Hour)
	if scheduled := inputs.Discount("openai", "", "", now.Add(time.Hour)); scheduled == nil || scheduled.ID != future {
		t.Fatalf("scheduled discount missing: %+v", scheduled)
	}
	inputs.RefreshedAt = now
	resolved, known := inputs.RateCardRevision(&project, now)
	if !known || resolved == nil || resolved.ID != revision {
		t.Fatalf("resolved revision = %+v, known = %v", resolved, known)
	}
	price := resolved.Explicit("openai", "", "model", "generation")
	if price == nil || price.InputPerMillion == nil || *price.InputPerMillion != "7.000000000000" {
		t.Fatalf("loaded explicit price = %+v", price)
	}
	server := &Server{Access: &access.Server{Pool: single}}
	request := httptest.NewRequest("GET", "/api/v1/rate-cards", nil).WithContext(ctx)
	if _, err = server.listRateCards(request, access.Principal{}); err != nil {
		t.Fatalf("list cards with one connection: %v", err)
	}
	request.SetPathValue("rate_card_id", card)
	if _, err = server.listRateCardRevisions(request, access.Principal{}); err != nil {
		t.Fatalf("list revisions with one connection: %v", err)
	}
}

func TestIntegrationConcurrentCardRevisions(t *testing.T) {
	pool := notificationPool(t)
	owner, _ := seedParityOwner(t, pool)
	card, _ := seedParityCard(t, pool, owner, nil, nil, "1", time.Now().Add(-time.Hour))
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var ready sync.WaitGroup
	ready.Add(2)
	start := make(chan struct{})
	results := make(chan error, 2)
	for range 2 {
		go func() {
			tx, err := pool.Begin(ctx)
			ready.Done()
			if err != nil {
				results <- err
				return
			}
			defer tx.Rollback(context.Background())
			<-start
			_, err = createCardRevision(ctx, tx, owner, card, "2", time.Now(), nil)
			if err == nil {
				err = tx.Commit(ctx)
			}
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var count, maxRevision int
	if err := pool.QueryRow(ctx, "SELECT count(*), max(revision) FROM olp.rate_card_revisions WHERE rate_card_id=$1::uuid", card).Scan(&count, &maxRevision); err != nil {
		t.Fatal(err)
	}
	if count != 3 || maxRevision != 3 {
		t.Fatalf("revisions = %d, max = %d; want 3, 3", count, maxRevision)
	}
}
