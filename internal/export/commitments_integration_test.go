//go:build integration

package export

import (
	"bytes"
	"encoding/csv"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/export/focusspec"
	"github.com/tyk-swe/olp/internal/usage"
)

func commitmentRows(t *testing.T, r *dayResult) []map[string]string {
	t.Helper()
	var result []map[string]string
	for _, obj := range r.objects {
		if obj.contentType != "application/gzip" {
			continue
		}
		data := mustGunzip(t, obj.body)
		report, err := focusspec.Validate(bytes.NewReader(data), FocusCriteria)
		if err != nil || !report.OK() {
			t.Fatalf("FOCUS validation: %v %+v", err, report)
		}
		records, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range records[1:] {
			m := map[string]string{}
			for i, k := range records[0] {
				m[k] = row[i]
			}
			result = append(result, m)
		}
	}
	return result
}

func TestIntegrationFocusUnusedCommitments(t *testing.T) {
	for _, period := range []string{"hour", "day", "month"} {
		t.Run(period, func(t *testing.T) {
			pool := matrixPool(t)
			now := time.Now().UTC()
			usageDay := now.AddDate(0, 0, -3).Truncate(24 * time.Hour)
			day := usageDay
			if period == "month" {
				day = time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
			}
			f := seedFocusFixture(t, pool, day)
			if _, err := pool.Exec(t.Context(), `UPDATE olp.commitment_schedules SET period=$1,effective_at=$2 WHERE provider_id=$3`, period, day, f.committed); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `SELECT olp.materialize_commitments($1,$2,512)`, day.Add(23*time.Hour), day); err != nil {
				t.Fatal(err)
			}
			w := &FocusWorker{Pool: pool, Now: func() time.Time { return now }}
			sink := focusSink{Destination: Destination{ID: "11111111-1111-4111-8111-111111111111"}, costBasis: "cost", CreatedAt: day}
			generate := func(s focusSink) *dayResult {
				t.Helper()
				r, err := w.generateDay(t.Context(), s, day)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			allRows := func(s focusSink, r *dayResult) []map[string]string {
				out := commitmentRows(t, r)
				if !day.Equal(usageDay) {
					later, err := w.generateDay(t.Context(), s, usageDay)
					if err != nil {
						t.Fatal(err)
					}
					out = append(out, commitmentRows(t, later)...)
				}
				return out
			}
			result := generate(sink)
			rows := commitmentRows(t, result)
			purchases := 1
			if period == "hour" {
				purchases = 24
			}
			if len(rows) != purchases*2 {
				t.Fatalf("%s empty period rows=%d", period, len(rows))
			}
			expected := map[string]focusspec.CommitmentBalance{}
			for _, row := range rows {
				if row["ChargeCategory"] == "Purchase" {
					id := row["CommitmentDiscountId"]
					b := expected[id]
					v := decimal.Zero
					if b.Purchased != "" {
						v = dec(b.Purchased)
					}
					v = v.Add(dec(row["BilledCost"]))
					expected[id] = focusspec.CommitmentBalance{Purchased: v.String(), Allocated: "0", Unused: v.String()}
				}
			}
			if err := focusspec.ValidateCommitments(rows, expected); err != nil {
				t.Fatal(err)
			}
			if result.commitments.PurchasedCost != dec("10").Mul(decimal.NewFromInt(int64(purchases))).String() || result.commitments.UnusedCost != result.commitments.PurchasedCost {
				t.Fatalf("totals: %+v", result.commitments)
			}
			// Incomplete/unpriced metering must remain visible without claiming idle
			// capacity is a measurement. Caller-funded and zero-weight use allocate none.
			at := usageDay.Add(10 * time.Minute)
			focusEvent(t, pool, f.keyCaller, f.committed, "focus-committed", at, &usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(1000)}, "caller", nil)
			focusEvent(t, pool, f.keyA, f.committed, "focus-committed", at, &usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(0)}, "", nil)
			focusEvent(t, pool, f.keyA, f.committed, "unpriced-capacity", at, &usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(10)}, "", nil)
			result = generate(sink)
			if result.commitments.Complete {
				t.Fatal("unpriced usage marked complete")
			}
			if result.commitments.AllocatedCost != "0" {
				t.Fatal(result.commitments)
			}
			// Three equal weights cross both consumers, and all residuals reconcile.
			for i, key := range []string{f.keyA, f.keyB, f.keyA} {
				focusEvent(t, pool, key, f.committed, "focus-committed", at.Add(time.Duration(i+1)*time.Minute), &usage.AttemptUsage{Observed: true, Complete: true, InputTokens: i64(1000)}, "", nil)
			}
			result = generate(sink)
			rows = allRows(sink, result)
			if result.commitments.AllocatedCost != "10" {
				t.Fatalf("allocated %+v", result.commitments)
			}
			unused := dec("10").Mul(decimal.NewFromInt(int64(purchases - 1))).String()
			for id, b := range expected {
				b.Allocated = "10"
				b.Unused = unused
				expected[id] = b
			}
			if err := focusspec.ValidateCommitments(rows, expected); err != nil {
				t.Fatal(err)
			}
			scoped := sink
			scoped.ProjectID = &f.projectA
			for _, row := range allRows(scoped, generate(scoped)) {
				if row["ChargeCategory"] == "Purchase" || row["CommitmentDiscountStatus"] == "Unused" || row["SubAccountId"] != f.projectA {
					t.Fatal("scope leaked commitment", row)
				}
			}
			var retainedRequestDetails bool
			if err := pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.commitment_usage WHERE payload ?| ARRAY['request_id','event_id','input_tokens','output_tokens'])`).Scan(&retainedRequestDetails); err != nil {
				t.Fatal(err)
			}
			if retainedRequestDetails {
				t.Fatal("commitment summary retained request details")
			}
			// Later connection edits must not rewrite purchase or consumer provenance.
			if _, err := pool.Exec(t.Context(), `UPDATE olp.providers SET name='renamed',project_id=$2 WHERE id=$1`, f.committed, f.projectB); err != nil {
				t.Fatal(err)
			}
			for _, row := range allRows(sink, generate(sink)) {
				if row["x_ConnectionName"] != "committed" {
					t.Fatal("historical name changed", row)
				}
			}
			// Detailed facts can disappear while their period's allocation survives.
			if _, err := pool.Exec(t.Context(), `DELETE FROM olp.attempt_usage_facts WHERE provider_id=$1`, f.committed); err != nil {
				t.Fatal(err)
			}
			result = generate(sink)
			if result.commitments.AllocatedCost != "10" {
				t.Fatal("retention changed allocation", result.commitments)
			}
			if err := focusspec.ValidateCommitments(allRows(sink, result), expected); err != nil {
				t.Fatal(err)
			}
		})
	}
}
