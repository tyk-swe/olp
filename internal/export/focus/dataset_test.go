package focus

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/export/destination"
)

var chargeDay = time.Date(2026, time.March, 14, 0, 0, 0, 0, time.UTC)

func datasetFor(b destination.CostBasis) dataset {
	return dataset{sinkID: "sink", sinkName: "finance", installationID: "installation", installationName: "Example", currency: "USD", day: chargeDay, basis: bases[b]}
}

func usageFor(cost string, committed bool) usageRow {
	amount := decimal.RequireFromString(cost)
	return usageRow{
		consumer: "project", consumerName: "Project", providerID: "provider", connectionName: "Primary",
		vendorID: "openai", providerKind: "openai", model: "gpt", operation: "chat", route: "default",
		apiKeyID: "key", attribution: "{}", committed: committed, commitmentPeriod: "month",
		allocated: decimal.NewNullDecimal(amount.Div(decimal.NewFromInt(4))), component: "input_tokens",
		unit: "1000000 Tokens", listUnitPrice: decimal.NewFromInt(2), contractedUnitPrice: decimal.NewFromInt(1),
		quantity: decimal.NewFromInt(1), listCost: amount.Mul(decimal.NewFromInt(2)), contractedCost: amount, attempts: 1,
	}
}

func readPart(t *testing.T, body []byte) [][]string {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	records, err := csv.NewReader(gz).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return records
}

func TestAssemblePartsCarryEveryRowUnderHeaders(t *testing.T) {
	cases := []struct {
		name  string
		rows  int
		parts int
	}{
		{"empty day", 0, 1},
		{"over one part", PartRowLimit + 1, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			usage := slices.Concat(slices.Repeat([]usageRow{usageFor("0.000000000004", true)}, c.rows/2), slices.Repeat([]usageRow{usageFor("0.000000000004", false)}, c.rows-c.rows/2))
			got, err := assemble(datasetFor(destination.Cost), 3, chargeDay.Add(30*time.Hour), "test", usage, nil, facts{})
			if err != nil {
				t.Fatal(err)
			}
			last := got.objects[len(got.objects)-1]
			if last.Key != manifestKey(chargeDay) {
				t.Fatalf("last object is %s, want the manifest", last.Key)
			}
			var manifest struct {
				Parts []struct {
					Key    string `json:"key"`
					Rows   int    `json:"rows"`
					Bytes  int    `json:"bytes"`
					SHA256 string `json:"sha256"`
				} `json:"parts"`
				Totals struct {
					Billed    string `json:"billed_cost"`
					Effective string `json:"effective_cost"`
				} `json:"totals"`
			}
			if err := json.Unmarshal(last.Body, &manifest); err != nil {
				t.Fatal(err)
			}
			uploaded := got.objects[:len(got.objects)-1]
			if len(uploaded) != c.parts || len(manifest.Parts) != c.parts {
				t.Fatalf("uploaded %d parts and listed %d, want %d", len(uploaded), len(manifest.Parts), c.parts)
			}
			billed, effective, total := decimal.Zero, decimal.Zero, 0
			for i, object := range uploaded {
				records := readPart(t, object.Body)
				if !slices.Equal(records[0], columns) {
					t.Fatalf("part %d does not start with the FOCUS header", i)
				}
				data := records[1:]
				digest := sha256.Sum256(object.Body)
				listed := manifest.Parts[i]
				if listed.Key != object.Key || listed.Rows != len(data) || listed.Bytes != len(object.Body) || listed.SHA256 != hex.EncodeToString(digest[:]) {
					t.Errorf("manifest part %d = %+v does not describe the uploaded object %s with %d rows", i, listed, object.Key, len(data))
				}
				if len(data) > PartRowLimit {
					t.Errorf("part %d has %d rows, over the %d limit", i, len(data), PartRowLimit)
				}
				billedAt, effectiveAt := slices.Index(records[0], "BilledCost"), slices.Index(records[0], "EffectiveCost")
				for _, row := range data {
					billed = billed.Add(decimal.RequireFromString(row[billedAt]))
					effective = effective.Add(decimal.RequireFromString(row[effectiveAt]))
				}
				total += len(data)
			}
			if total != c.rows {
				t.Errorf("parts carry %d rows, want %d", total, c.rows)
			}
			if !decimal.RequireFromString(manifest.Totals.Billed).Equal(billed) || !decimal.RequireFromString(manifest.Totals.Effective).Equal(effective) {
				t.Errorf("manifest totals billed=%s effective=%s, part cells sum to %s and %s", manifest.Totals.Billed, manifest.Totals.Effective, billed, effective)
			}
		})
	}
}

func TestChargeBasisNeverAppliesCommitments(t *testing.T) {
	row := usageFor("0.5", true)
	for basis, applied := range map[destination.CostBasis]bool{destination.Cost: true, destination.Charge: false} {
		t.Run(string(basis), func(t *testing.T) {
			got, err := assemble(datasetFor(basis), 1, chargeDay.Add(30*time.Hour), "test", []usageRow{row}, nil, facts{})
			if err != nil {
				t.Fatal(err)
			}
			records := readPart(t, got.objects[0].Body)
			cell := func(column string) string { return records[1][slices.Index(records[0], column)] }
			if (cell("CommitmentDiscountId") != "") != applied || (cell("CommitmentDiscountStatus") != "") != applied {
				t.Errorf("commitment columns id=%q status=%q, want applied=%v", cell("CommitmentDiscountId"), cell("CommitmentDiscountStatus"), applied)
			}
			want := cell("ContractedCost")
			if applied {
				want = row.allocated.Decimal.String()
			}
			if !decimal.RequireFromString(cell("EffectiveCost")).Equal(decimal.RequireFromString(want)) {
				t.Errorf("EffectiveCost = %s, want %s", cell("EffectiveCost"), want)
			}
		})
	}
}
