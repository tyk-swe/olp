package focusspec

import (
	"bytes"
	"encoding/csv"
	"maps"
	"slices"
	"testing"
)

// criteria are the applicability criteria an OLP FOCUS dataset declares.
var criteria = []string{
	"ACCOUNT_NAMING_SUPPORTED", "COMMITMENT_DISCOUNT_SUPPORTED", "MULTIPLE_PRICING_CATEGORIES_SUPPORTED",
	"NEGOTIATED_PRICING_SUPPORTED", "PUBLIC_PRICE_LIST_SUPPORTED", "SUB_ACCOUNT_SUPPORTED",
	"TAGGING_SUPPORTED", "UNIT_PRICING_SUPPORTED", "USAGE_MEASUREMENT_SUPPORTED",
}

var columns = []string{
	"BilledCost", "BillingAccountId", "BillingAccountName", "BillingCurrency", "BillingPeriodEnd",
	"BillingPeriodStart", "ChargeCategory", "ChargeClass", "ChargeDescription", "ChargeFrequency",
	"ChargePeriodEnd", "ChargePeriodStart", "CommitmentDiscountCategory", "CommitmentDiscountId",
	"CommitmentDiscountName", "CommitmentDiscountQuantity", "CommitmentDiscountStatus",
	"CommitmentDiscountType", "CommitmentDiscountUnit", "ConsumedQuantity", "ConsumedUnit",
	"ContractedCost", "ContractedUnitPrice", "EffectiveCost", "HostProviderName", "InvoiceIssuerName",
	"ListCost", "ListUnitPrice", "PricingCategory", "PricingQuantity", "PricingUnit", "ServiceCategory",
	"ServiceName", "ServiceProviderName", "ServiceSubcategory", "SkuId", "SkuMeter", "SkuPriceDetails",
	"SkuPriceId", "SubAccountId", "SubAccountName", "Tags", "x_ConnectionId",
}

func common() map[string]string {
	return map[string]string{
		"BillingAccountId": "0d6e2c3a-1111-4c4c-9a9a-000000000001", "BillingAccountName": "OpenLLMProxy",
		"BillingCurrency": "USD", "BillingPeriodStart": "2026-10-01T00:00:00Z", "BillingPeriodEnd": "2026-11-01T00:00:00Z",
		"ChargePeriodStart": "2026-10-12T00:00:00Z", "ChargePeriodEnd": "2026-10-13T00:00:00Z",
		"ServiceCategory": "AI and Machine Learning", "ServiceSubcategory": "Generative AI",
		"ServiceName": "OpenAI", "ServiceProviderName": "OpenAI", "HostProviderName": "OpenAI", "InvoiceIssuerName": "OpenAI",
		"SubAccountId": "9b1d1a1e-2222-4c4c-9a9a-000000000002", "SubAccountName": "Core", "x_ConnectionId": "p1",
	}
}

// usage is an uncommitted token line: 1,000 input tokens at 2.50 per
// million, discounted to 2.00.
func usage() map[string]string {
	row := common()
	maps.Copy(row, map[string]string{
		"ChargeCategory": "Usage", "ChargeFrequency": "Usage-Based",
		"ChargeDescription": "Input Tokens for gpt-4o generation via OpenAI",
		"PricingQuantity":   "0.001", "PricingUnit": "1000000 Tokens", "ListUnitPrice": "2.5", "ContractedUnitPrice": "2",
		"ListCost": "0.0025", "ContractedCost": "0.002", "BilledCost": "0.002", "EffectiveCost": "0.002",
		"ConsumedQuantity": "1000", "ConsumedUnit": "Tokens", "PricingCategory": "Standard",
		"SkuId": "openai/gpt-4o/generation/input_per_million", "SkuMeter": "Input Tokens",
		"SkuPriceId": "openai/gpt-4o/generation/input_per_million@2.5", "Tags": `{"team":"core"}`,
	})
	return row
}

// committed is a token line covered by a provisioned-capacity commitment:
// nothing billed, its allocated share effective.
func committed() map[string]string {
	row := usage()
	maps.Copy(row, map[string]string{
		"BilledCost": "0", "EffectiveCost": "0.0015", "PricingCategory": "Committed",
		"CommitmentDiscountId": "olp:commitment:p1:day", "CommitmentDiscountName": "OpenAI day commitment",
		"CommitmentDiscountCategory": "Spend", "CommitmentDiscountType": "Provisioned capacity",
		"CommitmentDiscountStatus": "Used", "CommitmentDiscountQuantity": "0.0015", "CommitmentDiscountUnit": "USD",
	})
	return row
}

// purchase is the commitment's own recurring purchase.
func purchase() map[string]string {
	row := common()
	maps.Copy(row, map[string]string{
		"ChargeCategory": "Purchase", "ChargeFrequency": "Recurring",
		"ChargeDescription": "Provisioned capacity commitment for OpenAI",
		"PricingQuantity":   "1", "PricingUnit": "Commitments", "ListUnitPrice": "300", "ContractedUnitPrice": "300",
		"ListCost": "300", "ContractedCost": "300", "BilledCost": "300", "EffectiveCost": "0", "PricingCategory": "Standard",
		"SkuId": "olp/commitment/day", "SkuMeter": "Provisioned Capacity Commitment", "SkuPriceId": "olp/commitment/day@300",
		"CommitmentDiscountId": "olp:commitment:p1:day", "CommitmentDiscountName": "OpenAI day commitment",
		"CommitmentDiscountCategory": "Spend", "CommitmentDiscountType": "Provisioned capacity",
		"CommitmentDiscountQuantity": "300", "CommitmentDiscountUnit": "USD",
	})
	return row
}

func encode(t *testing.T, header []string, rows []map[string]string) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	w := csv.NewWriter(&out)
	if err := w.Write(header); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		record := make([]string, len(header))
		for i, name := range header {
			record[i] = row[name]
		}
		if err := w.Write(record); err != nil {
			t.Fatal(err)
		}
	}
	w.Flush()
	return &out
}

func validate(t *testing.T, header []string, rows []map[string]string) Report {
	t.Helper()
	report, err := Validate(encode(t, header, rows), criteria)
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func ruleIDs(findings []Finding) []string {
	ids := make([]string, len(findings))
	for i, f := range findings {
		ids[i] = f.RuleID
	}
	return ids
}

// The generator is deterministic; regenerate it when the pin changes.

type mutation struct {
	name  string
	apply func(header *[]string, rows []map[string]string)
	want  []string
}

func set(row int, column, value string) func(*[]string, []map[string]string) {
	return func(_ *[]string, rows []map[string]string) { rows[row][column] = value }
}

func drop(column string) func(*[]string, []map[string]string) {
	return func(header *[]string, _ []map[string]string) {
		*header = slices.DeleteFunc(*header, func(c string) bool { return c == column })
	}
}

func mutations() []mutation {
	return []mutation{
		{"missing BilledCost column", drop("BilledCost"), []string{"CAU-CostAndUsage-D-006-M", "CAU-BilledCost-C-003-M"}},
		{"missing SkuPriceId column", drop("SkuPriceId"), []string{"CAU-ListUnitPrice-C-012-C", "CAU-ContractedUnitPrice-C-014-C"}},
		{"missing Tags column", drop("Tags"), []string{"CAU-CostAndUsage-D-064-C"}},
		{"null BilledCost", set(0, "BilledCost", ""), []string{"CAU-BilledCost-C-003-M"}},
		{"null ListCost", set(0, "ListCost", ""), []string{"CAU-ListCost-C-003-M"}},
		{"ListCost not ListUnitPrice times PricingQuantity", set(0, "ListCost", "0.0026"), []string{"CAU-ListCost-C-011-C"}},
		{"ContractedCost not ContractedUnitPrice times PricingQuantity", set(0, "ContractedCost", "0.0021"), []string{"CAU-ContractedCost-C-011-C"}},
		{"null PricingQuantity on usage", set(0, "PricingQuantity", ""), []string{"CAU-PricingQuantity-C-005-C"}},
		{"null SkuPriceId on usage", set(0, "SkuPriceId", ""), []string{"CAU-ListUnitPrice-C-012-C"}},
		{"null ListUnitPrice with SkuPriceId", set(0, "ListUnitPrice", ""), []string{"CAU-ListUnitPrice-C-005-C", "CAU-ListUnitPrice-C-013-C"}},
		{"negative ListUnitPrice", set(0, "ListUnitPrice", "-2.5"), []string{"CAU-ListUnitPrice-C-008-M"}},
		{"null PricingUnit with PricingQuantity", set(0, "PricingUnit", ""), []string{"CAU-PricingUnit-C-006-C"}},
		{"null ConsumedQuantity on usage", set(0, "ConsumedQuantity", ""), []string{"CAU-ConsumedUnit-C-005-C"}},
		{"ConsumedQuantity on purchase", set(2, "ConsumedQuantity", "1"), []string{"CAU-ConsumedQuantity-C-004-C"}},
		{"space date time", set(0, "ChargePeriodStart", "2026-10-12 00:00:00"), []string{"CAU-ChargePeriodStart-C-001-M", "CAU-ChargePeriodStart-C-002-M"}},
		{"offset date time", set(0, "ChargePeriodEnd", "2026-10-13T00:00:00+00:00"), []string{"CAU-ChargePeriodEnd-C-002-M"}},
		{"lowercase currency", set(0, "BillingCurrency", "usd"), []string{"CAU-BillingCurrency-C-003-M"}},
		{"unknown currency", set(0, "BillingCurrency", "ABC"), []string{"CAU-BillingCurrency-C-003-M"}},
		{"non-national currency", set(0, "BillingCurrency", "XAU"), []string{"CAU-BillingCurrency-C-006-M"}},
		{"thousands separator", set(0, "BilledCost", "1,000"), []string{"CAU-BilledCost-C-002-M"}},
		{"plus sign", set(0, "EffectiveCost", "+0.002"), []string{"CAU-EffectiveCost-C-002-M"}},
		{"unknown charge category", set(0, "ChargeCategory", "Spend"), []string{"CAU-ChargeCategory-C-003-M"}},
		{"usage-based purchase", set(2, "ChargeFrequency", "Usage-Based"), []string{"CAU-ChargeFrequency-C-004-C"}},
		{"null charge frequency", set(0, "ChargeFrequency", ""), []string{"CAU-ChargeFrequency-C-002-M"}},
		{"subcategory of another category", set(0, "ServiceCategory", "Compute"), []string{"CAU-ServiceSubcategory-C-004-M"}},
		{"unknown pricing category", set(0, "PricingCategory", "Discounted"), []string{"CAU-PricingCategory-C-007-M"}},
		{"null commitment status on covered usage", set(1, "CommitmentDiscountStatus", ""), []string{"CAU-CommitmentDiscountStatus-C-004-C"}},
		{"commitment status without commitment", set(0, "CommitmentDiscountStatus", "Used"), []string{"CAU-CommitmentDiscountStatus-C-003-C"}},
		{"unknown commitment category", set(1, "CommitmentDiscountCategory", "Hours"), []string{"CAU-CommitmentDiscountCategory-C-005-M"}},
		{"null commitment quantity on covered usage", set(1, "CommitmentDiscountQuantity", ""), []string{"CAU-CommitmentDiscountQuantity-C-005-C"}},
		{"commitment unit without quantity", set(0, "CommitmentDiscountUnit", "USD"), []string{"CAU-CommitmentDiscountUnit-C-005-C"}},
		{"tags array", set(0, "Tags", `["core"]`), []string{"CAU-Tags-C-001-M"}},
		{"tags nested", set(0, "Tags", `{"team":{"name":"core"}}`), []string{"CAU-Tags-C-001-M"}},
		{"tags duplicate key", set(0, "Tags", `{"team":"a","team":"b"}`), []string{"CAU-Tags-C-001-M"}},
		{"sku price with two skus", set(1, "SkuId", "openai/gpt-4o/generation/other"), []string{"CAU-SkuPriceId-C-008-M"}},
		{"null billing account id", set(0, "BillingAccountId", ""), []string{"CAU-BillingAccountId-C-001-M"}},
		{"null service name", set(0, "ServiceName", ""), []string{"CAU-ServiceName-C-003-M"}},
		{"null invoice issuer", set(0, "InvoiceIssuerName", ""), []string{"CAU-InvoiceIssuerName-C-003-M"}},
		{"correction class", set(0, "ChargeClass", "Fix"), []string{"CAU-ChargeClass-C-005-C"}},
		{"tax with effective not billed", func(_ *[]string, rows []map[string]string) {
			rows[0]["ChargeCategory"] = "Tax"
			for _, c := range []string{"PricingQuantity", "PricingUnit", "ListUnitPrice", "ContractedUnitPrice", "SkuId", "SkuPriceId", "SkuMeter", "PricingCategory", "ConsumedQuantity", "ConsumedUnit"} {
				rows[0][c] = ""
			}
			rows[0]["EffectiveCost"] = "0.001"
		}, []string{"CAU-EffectiveCost-C-017-C"}},
	}
}

func TestMutationsAreViolations(t *testing.T) {
	for _, m := range mutations() {
		t.Run(m.name, func(t *testing.T) {
			header := slices.Clone(columns)
			rows := []map[string]string{usage(), committed(), purchase()}
			m.apply(&header, rows)
			report := validate(t, header, rows)
			got := ruleIDs(report.Violations)
			if len(got) == 0 {
				t.Fatal("no violation reported")
			}
			for _, want := range m.want {
				if !slices.Contains(got, want) {
					t.Fatalf("violations %v do not include %s", got, want)
				}
			}
		})
	}
}
