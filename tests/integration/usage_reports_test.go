//go:build integration

package integration_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/ingest"
	"github.com/tyk-swe/olp/internal/usage/reports"
	"github.com/tyk-swe/olp/internal/usage/retention"
	"github.com/tyk-swe/olp/tests/harness"
	"github.com/tyk-swe/olp/tests/harness/vendor"
)

func TestUnpricedBreakdownKeepsAttemptCausesAfterPricesChangeAndUsageRollsUp(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	ctx := t.Context()
	hour := fixture.observedAt.Truncate(time.Hour)
	const missingModel, partialModel = "no-model-price", "partial-model-price"
	var pricingID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO olp.prices
		(pricing_revision_id,provider_kind,provider_id,model,operation,input_per_million,currency)
		SELECT pricing_revision_id,provider_kind,provider_id,$2,operation,1,currency
		FROM olp.prices WHERE provider_id=$1 RETURNING pricing_revision_id`,
		fixture.providerID, partialModel).Scan(&pricingID); err != nil {
		t.Fatal(err)
	}
	persist := func(event *usage.Event) {
		t.Helper()
		accepted, err := usage.NewAccountingEvent(event)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := ingest.PersistEvent(ctx, pool, accepted); err != nil || result.Outcome != ingest.PersistOutcomePersisted {
			t.Fatalf("persist fixture: %+v %v", result, err)
		}
	}
	missing := func(ordinal int) usage.Attempt {
		attempt := fixture.attempt(ordinal)
		attempt.UpstreamModel = missingModel
		return attempt
	}
	noPrice := fixture.event(missing(1), missing(2), missing(3))
	persist(noPrice)

	incomplete := func(ordinal int) usage.Attempt {
		attempt := missing(ordinal)
		attempt.Usage.Evidence = usage.EvidenceObservedIncomplete
		return attempt
	}
	answeredWithoutUsage := missing(1)
	answeredWithoutUsage.Usage = &usage.AttemptUsage{Evidence: usage.EvidenceUnobservedUncertain}
	unsettled := fixture.event(answeredWithoutUsage, incomplete(2))
	unsettled.UsageComplete = false
	persist(unsettled)

	partial := fixture.attempt(1)
	partial.UpstreamModel = partialModel
	persist(fixture.event(partial))
	persist(fixture.event(fixture.attempt(1)))
	historical := fixture.event(missing(1))
	persist(historical)
	if _, err := pool.Exec(ctx, `UPDATE olp.attempt_usage_facts SET unpriced_cause = NULL WHERE event_id = $1`, historical.EventID); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO olp.prices
		(pricing_revision_id,provider_kind,provider_id,model,operation,input_per_million,output_per_million,currency)
		SELECT pricing_revision_id,provider_kind,provider_id,$3,operation,input_per_million,output_per_million,currency
		FROM olp.prices WHERE provider_id=$1 AND model=$2`, fixture.providerID, vendor.Model, missingModel); err != nil {
		t.Fatal(err)
	}
	filters := reports.Filters{Start: hour, End: hour.Add(time.Hour), Projects: reports.OnlyProjects(fixture.projectID)}
	expected := []struct {
		model   string
		cause   contract.UnpricedBreakdownItemCause
		count   int64
		request string
	}{
		{missingModel, contract.NoApplicablePrice, 3, noPrice.RequestID},
		{missingModel, contract.IncompleteUsage, 2, unsettled.RequestID},
		{missingModel, contract.NotRecorded, 1, historical.RequestID},
		{partialModel, contract.MissingPriceComponent, 1, ""},
	}
	assertBreakdown := func(live bool) {
		t.Helper()
		report, err := reports.ReadUnpricedBreakdown(ctx, pool, filters, 50)
		if err != nil || len(report.Items) != len(expected) {
			t.Fatalf("read unpriced groups: %+v %v", report, err)
		}
		for i, want := range expected {
			row := report.Items[i]
			if row.ProviderId.String() != fixture.providerID || row.Operation != noPrice.Operation ||
				row.Model != want.model || row.Cause != want.cause || row.AttemptCount != want.count {
				t.Errorf("group %d = %+v, want %+v", i, row, want)
			}
			if live && want.request != "" && (row.SampleRequestId == nil || row.SampleRequestId.String() != want.request) {
				t.Errorf("group %d did not link the matching request %s", i, want.request)
			}
			if !live && row.SampleRequestId != nil {
				t.Errorf("retained group %d linked expired request %s", i, row.SampleRequestId)
			}
		}
	}
	assertBreakdown(true)
	limited, err := reports.ReadUnpricedBreakdown(ctx, pool, filters, 1)
	if err != nil || len(limited.Items) != 1 || limited.Items[0].AttemptCount != 3 {
		t.Fatalf("limit did not keep the highest-impact group: %+v %v", limited, err)
	}
	privateFilters := filters
	privateFilters.Projects = reports.OnlyProjects(uuid.NewString())
	private, err := reports.ReadUnpricedBreakdown(ctx, pool, privateFilters, 50)
	if err != nil || len(private.Items) != 0 {
		t.Fatalf("another project can see unpriced usage: %+v %v", private, err)
	}
	var pin *uuid.UUID
	var cost, charge *string
	if err := pool.QueryRow(ctx, `SELECT pricing_revision_id, estimated_cost::text, charge::text
		FROM olp.attempt_usage_facts WHERE attempt_id=$1`, partial.ID).Scan(&pin, &cost, &charge); err != nil ||
		pin == nil || *pin != pricingID || cost != nil || charge != nil {
		t.Fatalf("partial rate changed its pin or historical cost/charge: %v %v %v %v", pin, cost, charge, err)
	}

	maintenance, err := retention.RunMaintenance(ctx, pool, hour.AddDate(0, 0, 91))
	if err != nil || maintenance.UsageRows != 8 {
		t.Fatalf("roll up fixture: %+v %v", maintenance, err)
	}
	assertBreakdown(false)
}

func TestUsageReportsKeepWholePeriodAllocationAndCompleteness(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	selected, earlier, later := fixture.attempt(1), fixture.attempt(2), fixture.attempt(3)
	ineligible, legacy, absent := fixture.attempt(4), fixture.attempt(5), fixture.attempt(6)
	event := fixture.event(selected, earlier, later, ineligible, legacy, absent)
	accepted, err := usage.NewAccountingEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ingest.PersistEvent(t.Context(), pool, accepted)
	if err != nil || result.Outcome != ingest.PersistOutcomePersisted {
		t.Fatalf("persist report fixture: outcome=%v error=%v", result.Outcome, err)
	}
	selectedIDs := []string{selected.ID, ineligible.ID, legacy.ID, absent.ID}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.attempt_usage_facts
		SET estimated_cost=0,commitment_period='day',commitment_amount=6,commitment_weight=1,
		 route_slug=CASE WHEN attempt_id=ANY($2::uuid[]) THEN 'report-selected' ELSE 'report-outside' END,
		 container_sessions=CASE WHEN attempt_id=$3 THEN '{"small":2}'::jsonb
		 WHEN attempt_id=$4 THEN '{}'::jsonb WHEN attempt_id=$5 THEN '{"small":3,"large":1}'::jsonb ELSE NULL END
		WHERE event_id=$1`, event.EventID, selectedIDs, selected.ID, legacy.ID, absent.ID); err != nil {
		t.Fatal(err)
	}
	scheduleID, durablePeriodID, legacyPeriodID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	previousDay := fixture.observedAt.UTC().Truncate(24 * time.Hour).Add(-24 * time.Hour)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_schedules
		(id,provider_id,provider_revision_id,provider_name,provider_kind,amount,currency,period,effective_at)
		VALUES ($1,$2,$3,'report fixture','openai_compatible',1,'USD','day',$4)`,
		scheduleID, fixture.providerID, uuid.NewString(), previousDay); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_periods
		(id,schedule_id,bucket_start,bucket_end,amount,legacy)
		VALUES ($1,$2,$3::timestamptz,$3::timestamptz+interval '1 day',1,false),
		 ($4,$2,$3::timestamptz+interval '1 day',$3::timestamptz+interval '2 days',6,true)`,
		durablePeriodID, scheduleID, previousDay, legacyPeriodID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_usage
		(attempt_id,period_id,observed_at,weight,eligible,complete,payload)
		SELECT attempt_id,$2,observed_at+CASE WHEN attempt_id=$3 THEN interval '-1 minute'
		 WHEN attempt_id=$4 THEN interval '1 minute' ELSE interval '0' END,
		 CASE WHEN attempt_id=$5 THEN 10000 ELSE 1 END,attempt_id<>$5,true,'{}'
		FROM olp.attempt_usage_facts WHERE attempt_id=ANY($1::uuid[])`,
		[]string{selected.ID, earlier.ID, later.ID, ineligible.ID}, durablePeriodID, earlier.ID, later.ID, ineligible.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.commitment_usage
		(attempt_id,period_id,observed_at,weight,eligible,complete,payload)
		VALUES ($1,$2,$3,1,true,true,'{}')`, legacy.ID, legacyPeriodID, fixture.observedAt); err != nil {
		t.Fatal(err)
	}
	route := "report-selected"
	filters := reports.Filters{
		Start: fixture.observedAt.Add(-time.Minute), End: fixture.observedAt.Add(time.Minute),
		ProviderID: &fixture.providerID, Route: &route, Projects: reports.AllProjects(),
	}
	now := time.Now()
	summary, err := reports.ReadSummary(t.Context(), pool, filters, now)
	if err != nil {
		t.Fatal(err)
	}
	if summary.AllocatedCommitment != "2.333333333334" || summary.RequestCount != 1 {
		t.Fatalf("selected request allocation changed: %+v", summary)
	}
	if len(summary.ContainerSessions) != 2 || summary.ContainerSessions["small"] != "5" || summary.ContainerSessions["large"] != "1" {
		t.Fatalf("container totals changed: %v", summary.ContainerSessions)
	}
	full, err := reports.ReadSummary(t.Context(), pool, reports.Filters{
		Start: filters.Start, End: filters.End, ProviderID: filters.ProviderID, Projects: reports.AllProjects(),
	}, now)
	if err != nil || full.AllocatedCommitment != "3.000000000000" {
		t.Fatalf("whole period lost its residual: allocation=%s error=%v", full.AllocatedCommitment, err)
	}
	completeness, err := reports.ReadCompleteness(t.Context(), pool, filters, now)
	if err != nil {
		t.Fatal(err)
	}
	if completeness.RequestCount != summary.RequestCount || completeness.UnpricedCount != summary.UnpricedCount ||
		completeness.IncompleteCount != summary.IncompleteCount || completeness.PricedCount != summary.RequestCount-summary.UnpricedCount ||
		completeness.Complete != summary.Complete || completeness.Coverage != summary.Coverage ||
		completeness.GapEvents != summary.GapEvents || completeness.UncertainGapCount != summary.UncertainGapCount ||
		completeness.Consumer.State != summary.Consumer.State ||
		completeness.EstimatedCost == nil || summary.EstimatedCost == nil || *completeness.EstimatedCost != *summary.EstimatedCost ||
		completeness.Currency == nil || summary.Currency == nil || *completeness.Currency != *summary.Currency {
		t.Fatalf("completeness differs from summary: completeness=%+v summary=%+v", completeness, summary)
	}
	hour := fixture.observedAt.Truncate(time.Hour).Add(-2 * time.Hour)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.attempt_usage_hourly
		(bucket,route_slug,provider_id,upstream_model,operation,surface,api_key_id,
		request_count,provider_request_count,model_request_count,target_request_count,
		input_tokens,output_tokens,cached_input_tokens,media_units,
		request_unpriced_count,provider_unpriced_count,model_unpriced_count,target_unpriced_count,
		request_incomplete_count,provider_incomplete_count,model_incomplete_count,target_incomplete_count)
		VALUES($1,'report-hourly',$2,$3,'generation','openai',$4,1,1,1,1,0,0,0,0,0,0,0,0,0,0,0,0)`,
		hour, fixture.providerID, vendor.Model, fixture.keyID); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []time.Duration{0, time.Minute} {
		retainedFilters := reports.Filters{Start: hour.Add(offset), End: hour.Add(time.Hour), ProviderID: &fixture.providerID, Projects: reports.AllProjects()}
		retainedSummary, err := reports.ReadSummary(t.Context(), pool, retainedFilters, now)
		if err != nil {
			t.Fatal(err)
		}
		retainedCompleteness, err := reports.ReadCompleteness(t.Context(), pool, retainedFilters, now)
		if err != nil {
			t.Fatal(err)
		}
		if retainedCompleteness.RequestCount != retainedSummary.RequestCount ||
			retainedCompleteness.Coverage != retainedSummary.Coverage ||
			retainedCompleteness.EstimatedCost != nil || retainedSummary.EstimatedCost != nil ||
			(retainedCompleteness.RequestCount == 1) != (offset == 0) {
			t.Fatalf("retained-hour completeness differs at offset %s: completeness=%+v summary=%+v", offset, retainedCompleteness, retainedSummary)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.attempt_usage_hourly SET provider_unpriced_count=2
		WHERE bucket=$1 AND provider_id=$2`, hour, fixture.providerID); err != nil {
		t.Fatal(err)
	}
	invalidFilters := reports.Filters{Start: hour, End: hour.Add(time.Hour), ProviderID: &fixture.providerID, Projects: reports.AllProjects()}
	if _, err := reports.ReadSummary(t.Context(), pool, invalidFilters, now); err == nil {
		t.Fatal("summary accepted more unpriced requests than total requests")
	}
	if _, err := reports.ReadCompleteness(t.Context(), pool, invalidFilters, now); err == nil {
		t.Fatal("completeness accepted more unpriced requests than total requests")
	}
}

func TestReconciliationContinuesExcludingCallerPaidSpendAfterRollup(t *testing.T) {
	t.Parallel()
	for _, scenario := range []struct {
		name          string
		legacy        bool
		operatorSpend string
	}{{"durable", false, "8.25"}, {"legacy", true, "5.75"}} {
		t.Run(scenario.name, func(t *testing.T) {
			t.Parallel()
			checkCallerPaidSpendAfterRollup(t, scenario.legacy, scenario.operatorSpend)
		})
	}
}

func checkCallerPaidSpendAfterRollup(t *testing.T, legacy bool, operatorSpend string) {
	t.Helper()
	ctx := t.Context()
	pool, _ := harness.Database(t)
	f := newUsageFixture(t, pool)
	day := f.observedAt.Truncate(24 * time.Hour)
	if _, err := pool.Exec(ctx, `INSERT INTO olp.commitment_schedules
		(provider_id,provider_revision_id,provider_name,provider_kind,amount,currency,period,effective_at)
		VALUES($1,$2,'reconciliation fixture','openai_compatible',5,'USD','day',$3)`,
		f.providerID, uuid.NewString(), day); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		caller     bool
		cost       string
		commitment bool
	}{{false, "3.25", false}, {true, "7.5", false}, {false, "0", true}, {true, "0", true}} {
		attempt := f.attempt(1)
		event, err := usage.NewAccountingEvent(f.event(attempt))
		if err != nil {
			t.Fatal(err)
		}
		if result, err := ingest.PersistEvent(ctx, pool, event); err != nil || result.Outcome != ingest.PersistOutcomePersisted {
			t.Fatalf("persist reconciliation sample %+v: %v, %v", sample, result.Outcome, err)
		}
		if _, err := pool.Exec(ctx, `UPDATE olp.attempt_usage_facts SET caller_credential=$2,
			estimated_cost=$3::text::numeric,commitment_period=CASE WHEN $4 THEN 'day' END,
			commitment_amount=CASE WHEN $4 THEN 5 END,commitment_weight=CASE WHEN $4 THEN 1 END
			WHERE attempt_id=$1`, attempt.ID, sample.caller, sample.cost, sample.commitment); err != nil {
			t.Fatal(err)
		}
	}
	if legacy {
		result, err := pool.Exec(ctx, `UPDATE olp.commitment_periods SET legacy=true WHERE provider_id=$1 AND bucket_start=$2`, f.providerID, day)
		if err != nil || result.RowsAffected() != 1 {
			t.Fatalf("mark fixture period legacy: %v, %v", result, err)
		}
	}
	check := func() {
		t.Helper()
		rows, err := reports.ReconciliationSpend(ctx, pool, day, day.Add(24*time.Hour), []string{f.providerID})
		if err != nil || len(rows) != 1 {
			t.Fatalf("reconciliation returned %+v, %v", rows, err)
		}
		row := rows[0]
		if strings.TrimRight(row.AttributedCost, "0") != operatorSpend || row.PricedAttempts != 2 || row.ExcludedCallerAttempts != 2 {
			t.Fatalf("reconciliation returned %+v, want gateway spend %s and two excluded caller attempts", row, operatorSpend)
		}
		totals, err := reports.ReadSummary(ctx, pool, reports.Filters{Start: day, End: day.Add(24 * time.Hour),
			ProviderID: &f.providerID, Projects: reports.AllProjects()}, day.Add(24*time.Hour))
		if err != nil || strings.TrimRight(strings.TrimRight(totals.AllocatedCommitment, "0"), ".") != "5" {
			t.Fatalf("usage summary commitment = %q, want 5, error = %v", totals.AllocatedCommitment, err)
		}
	}
	check()
	if result, err := retention.RunMaintenance(ctx, pool, day.AddDate(0, 0, 91)); err != nil || result.UsageRows != 4 {
		t.Fatalf("rollup returned %+v, %v", result, err)
	}
	check()
}
