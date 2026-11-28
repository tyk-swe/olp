//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestUsagePersistsPinnedRateCardsAtLaterObservation(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.api_keys SET project_id=NULL WHERE id=$1`, fixture.keyID); err != nil {
		t.Fatal(err)
	}
	providerRevision := uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_revisions(id,provider_id,revision,name,configuration,models,slots,source_etag,activated_by)
		VALUES($1,$2,1,'Rate card fixture','{}','[]','[]',$1,$3)`, providerRevision, fixture.providerID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	card, oldRevision, newRevision := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_cards(id,project_id,created_by) VALUES($1,$2,$3)`, card, fixture.projectID, fixture.ownerID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.rate_card_revisions(id,rate_card_id,revision,markup,effective_at,created_by)
		VALUES($1,$2,1,2,$3,$4),($5,$2,2,3,$6,$4)`, oldRevision, card, fixture.observedAt.Add(-time.Minute), fixture.ownerID, newRevision, fixture.observedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, policy, revision, charge string
	}{
		{"old_pin", fmt.Sprintf(`{"rate_card_pinned":true,"rate_card_revision_id":%q}`, oldRevision), oldRevision, "0.0004"},
		{"pinned_absence", `{"rate_card_pinned":true,"rate_card_revision_id":null}`, "", "0.0002"},
		{"stale_unpinned", `{"rate_card_pinned":false,"rate_card_revision_id":null}`, newRevision, "0.0006"},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempt := fixture.attempt(1)
			input, output := int64(100), int64(50)
			attempt.Usage.InputTokens, attempt.Usage.OutputTokens = &input, &output
			policy := json.RawMessage(test.policy)
			attempt.Routing = &usage.Routing{ProviderRevisionID: providerRevision, Policy: &policy}
			event := fixture.event(attempt)
			event.RequestCompletedAt = fixture.observedAt.Add(2 * time.Second)
			event.Attempts[0].CompletedAt = event.RequestCompletedAt
			event.ObservedAt = event.RequestCompletedAt
			accepted, err := usage.NewAccountingEvent(event)
			if err != nil {
				t.Fatal(err)
			}
			result, err := ingest.PersistEvent(t.Context(), pool, accepted)
			if err != nil || result.Outcome != ingest.PersistOutcomePersisted {
				t.Fatalf("persist rate-card pin: outcome=%v error=%v", result.Outcome, err)
			}
			var revision string
			var costMatches, chargeMatches bool
			if err := pool.QueryRow(t.Context(), `SELECT COALESCE(rate_card_revision_id::text,''),estimated_cost=0.0002,charge=$2::numeric
				FROM olp.attempt_usage_facts WHERE attempt_id=$1`, attempt.ID, test.charge).Scan(&revision, &costMatches, &chargeMatches); err != nil {
				t.Fatal(err)
			}
			if revision != test.revision || !costMatches || !chargeMatches {
				t.Fatalf("persisted revision=%q cost matches=%t charge matches=%t; want revision=%q charge=%s", revision, costMatches, chargeMatches, test.revision, test.charge)
			}
		})
	}
}
