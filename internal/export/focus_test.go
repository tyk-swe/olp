package export

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/export/focusspec"
)

func dec(s string) decimal.Decimal { return decimal.RequireFromString(s) }

// syntheticRows covers every shape the generator writes: a plain priced group,
// a committed group (with shares resolved), a fallback line, and a tiered line.
func syntheticRows() []focusRow {
	weight := dec("2")
	amount := dec("10")
	return []focusRow{
		{ // input tokens, discounted
			consumer: "p1", consumerName: "One", providerID: "prov", connectionName: "OpenAI",
			vendorID: "openai", providerKind: "openai", providerProject: "p1",
			model: "gpt-4o", operation: "generation", route: "chat", apiKeyID: "k1",
			attribution: `{"team":"core"}`,
			component:   "input_per_million", unit: "1000000 Tokens",
			listUnitPrice: dec("3"), contractedUnitPrice: dec("1.5"),
			quantity: dec("0.001"), listCost: dec("0.003"), contractedCost: dec("0.0015"),
			attempts: 1,
		},
		{ // committed: allocated effective cost
			consumer: "p1", consumerName: "One", providerID: "prov", connectionName: "OpenAI",
			vendorID: "openai", providerKind: "openai",
			model: "gpt-4o", operation: "generation", route: "chat",
			committed: true, commitmentPeriod: "day", commitmentBucket: "2026-04-15 00:00:00",
			component: "output_per_million", unit: "1000000 Tokens",
			listUnitPrice: dec("12"), contractedUnitPrice: dec("12"),
			quantity: dec("0.5"), listCost: dec("6"), contractedCost: dec("6"),
			periodWeight: &weight, latestAmount: &amount,
			attempts: 4,
		},
		{ // fallback attempt line
			providerID: "prov", connectionName: "OpenAI", vendorID: "openai", providerKind: "openai",
			model: "gpt-4o", operation: "generation",
			component: "attempt", unit: "Requests",
			listUnitPrice: dec("0.01"), contractedUnitPrice: dec("0.01"),
			quantity: dec("1"), listCost: dec("0.01"), contractedCost: dec("0.01"),
			attempts: 1, fallbackAttempts: 1,
		},
		{ // tiered line
			providerID: "prov", connectionName: "OpenAI", vendorID: "openai", providerKind: "openai",
			model: "gpt-4o", operation: "generation",
			component: "input_per_million", unit: "1000000 Tokens",
			tier:          ptrString("1000000"),
			listUnitPrice: dec("2"), contractedUnitPrice: dec("2"),
			quantity: dec("2"), listCost: dec("4"), contractedCost: dec("4"),
			attempts: 2,
		},
	}
}

func ptrString(s string) *string { return &s }

func testFocusContext(basis string) *focusContext {
	return &focusContext{
		installationID:   "11111111-2222-3333-4444-555555555555",
		installationName: "TestInstall",
		currency:         "USD",
		day:              time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
		monthStart:       time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		monthEnd:         time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
		costBasis:        basis,
	}
}

func gunzip(t *testing.T, body []byte) []byte {
	t.Helper()
	r, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := out.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func readCSV(t *testing.T, body []byte) [][]string {
	t.Helper()
	rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestFocusCSVColumns(t *testing.T) {
	if len(focusColumns) != 51 {
		t.Fatalf("focusColumns = %d, want 51", len(focusColumns))
	}
	fc := testFocusContext("cost")
	var rows [][]string
	for _, r := range syntheticRows() {
		rows = append(rows, r.render(fc))
	}
	rows = append(rows, purchaseRow(fc, "prov", "OpenAI", "openai", "openai", "p1", "One", "day", "2026-04-15 00:00:00", dec("10")))
	_, parts := buildFocusParts(focusSink{Destination: Destination{ID: "s1"}, costBasis: "cost"}, fc.day, 1, rows)
	if len(parts) != 1 {
		t.Fatalf("parts = %d", len(parts))
	}
	data := readCSV(t, gunzip(t, parts[0].body))
	if got := strings.Join(data[0], ","); got != strings.Join(focusColumns, ",") {
		t.Fatalf("header mismatch:\n%s", got)
	}
	if len(data) != 6 {
		t.Fatalf("rows = %d, want header + 5", len(data))
	}
	// Every non-committed usage row: billed = effective = contracted.
	for _, row := range data[1:5] {
		m := map[string]string{}
		for i, c := range focusColumns {
			m[c] = row[i]
		}
		if m["PricingCategory"] == "Committed" {
			continue
		}
		if m["BilledCost"] != m["ContractedCost"] || m["EffectiveCost"] != m["ContractedCost"] {
			t.Fatalf("plain row billed/effective/contracted: %s %s %s", m["BilledCost"], m["EffectiveCost"], m["ContractedCost"])
		}
		if m["BillingCurrency"] != "USD" || m["BillingAccountName"] != "TestInstall" {
			t.Fatalf("accounting columns: %v", m)
		}
	}
	// Committed row: billed 0, effective = list × amount/weight = 6×10/2 = 30? — wait, that is >0
	for _, row := range data[1:5] {
		m := map[string]string{}
		for i, c := range focusColumns {
			m[c] = row[i]
		}
		if m["PricingCategory"] != "Committed" {
			continue
		}
		if m["BilledCost"] != "0" {
			t.Fatalf("committed BilledCost = %s", m["BilledCost"])
		}
		if m["EffectiveCost"] != "30" {
			t.Fatalf("committed EffectiveCost = %s, want 30 (6*10/2)", m["EffectiveCost"])
		}
		if m["CommitmentDiscountId"] != "olp:commitment:prov:day" || m["CommitmentDiscountStatus"] != "Used" {
			t.Fatalf("committed fields: %v %v", m["CommitmentDiscountId"], m["CommitmentDiscountStatus"])
		}
		if m["CommitmentDiscountUnit"] != "USD" {
			t.Fatalf("CommitmentDiscountUnit = %s", m["CommitmentDiscountUnit"])
		}
	}
	// Tiered row carries the tier price id.
	for _, row := range data[1:5] {
		m := map[string]string{}
		for i, c := range focusColumns {
			m[c] = row[i]
		}
		if m["x_PriceTier"] == "1000000" && m["SkuPriceId"] != "openai/gpt-4o/generation/input_per_million@2@above=1000000" {
			t.Fatalf("tiered SkuPriceId = %s", m["SkuPriceId"])
		}
	}
	// Purchase row.
	purchase := data[5]
	pm := map[string]string{}
	for i, c := range focusColumns {
		pm[c] = purchase[i]
	}
	if pm["ChargeCategory"] != "Purchase" || pm["ChargeFrequency"] != "Recurring" ||
		pm["BilledCost"] != "10" || pm["EffectiveCost"] != "0" ||
		pm["SkuId"] != "olp/commitment/day" || pm["SkuMeter"] != "Provisioned Capacity Commitment" ||
		pm["PricingUnit"] != "Commitments" {
		t.Fatalf("purchase row: %v", pm)
	}
	if pm["SubAccountId"] != "p1" || pm["CommitmentDiscountQuantity"] != "10" {
		t.Fatalf("purchase account/discount: %v %v", pm["SubAccountId"], pm["CommitmentDiscountQuantity"])
	}
}

func TestFocusCSVChargeBasis(t *testing.T) {
	fc := testFocusContext("charge")
	var rows [][]string
	for _, r := range syntheticRows() {
		rows = append(rows, r.render(fc))
	}
	data := readCSV(t, gunzip(t, func() []byte {
		_, parts := buildFocusParts(focusSink{Destination: Destination{ID: "s"}, costBasis: "charge"}, fc.day, 1, rows)
		return parts[0].body
	}()))
	for _, row := range data[1:] {
		m := map[string]string{}
		for i, c := range focusColumns {
			m[c] = row[i]
		}
		if m["PricingCategory"] != "Standard" || m["CommitmentDiscountId"] != "" {
			t.Fatalf("charge row carries commitment fields: %v", m)
		}
		if m["ListUnitPrice"] != m["ContractedUnitPrice"] {
			t.Fatalf("charge unit prices differ: %s vs %s", m["ListUnitPrice"], m["ContractedUnitPrice"])
		}
		if m["ServiceProviderName"] != "TestInstall" || m["InvoiceIssuerName"] != "TestInstall" {
			t.Fatalf("charge issuer/provider: %v %v", m["ServiceProviderName"], m["InvoiceIssuerName"])
		}
	}
}

func TestFocusPartRotation(t *testing.T) {
	old := FocusPartRowLimit
	FocusPartRowLimit = 2
	defer func() { FocusPartRowLimit = old }()
	fc := testFocusContext("cost")
	var rows [][]string
	for _, r := range syntheticRows() {
		rows = append(rows, r.render(fc))
	}
	_, parts := buildFocusParts(focusSink{Destination: Destination{ID: "s"}}, fc.day, 1, rows)
	if len(parts) != 2 {
		t.Fatalf("parts = %d, want 2", len(parts))
	}
	for i, part := range parts {
		data := readCSV(t, gunzip(t, part.body))
		if len(data) != 3 && len(data) != 1+int(part.rows) {
			t.Fatalf("part %d rows", i)
		}
		if !strings.Contains(part.key, "part-0000") {
			t.Fatalf("part key %s", part.key)
		}
	}
	if parts[0].key != "focus/v1.4/billing_period=2026-04/charge_date=2026-04-15/revision=0001/part-00000.csv.gz" {
		t.Fatalf("part key %s", parts[0].key)
	}
}

func TestFocusDeterminism(t *testing.T) {
	fc := testFocusContext("cost")
	var rows [][]string
	for _, r := range syntheticRows() {
		rows = append(rows, r.render(fc))
	}
	_, a := buildFocusParts(focusSink{Destination: Destination{ID: "s"}}, fc.day, 1, rows)
	_, b := buildFocusParts(focusSink{Destination: Destination{ID: "s"}}, fc.day, 1, rows)
	if !bytes.Equal(a[0].body, b[0].body) {
		t.Fatal("same input produced different part bytes")
	}
	sum := sha256.Sum256(a[0].body)
	if hex.EncodeToString(sum[:]) != a[0].sha256hex {
		t.Fatal("sha256 field does not match the object bytes")
	}
}

// TestFocusConformance runs every part through the pinned FOCUS 1.4 checker:
// this is the contract the M6 exit criterion validates.
func TestFocusConformance(t *testing.T) {
	for _, basis := range []string{"cost", "charge"} {
		fc := testFocusContext(basis)
		var rows [][]string
		for _, r := range syntheticRows() {
			rows = append(rows, r.render(fc))
		}
		if basis == "cost" {
			rows = append(rows, purchaseRow(fc, "prov", "OpenAI", "openai", "openai", "p1", "One",
				"day", "2026-04-15 00:00:00", dec("10")))
		}
		_, parts := buildFocusParts(focusSink{Destination: Destination{ID: "s"}, costBasis: basis}, fc.day, 1, rows)
		for _, part := range parts {
			report, err := focusspec.Validate(bytes.NewReader(gunzip(t, part.body)), FocusCriteria)
			if err != nil {
				t.Fatalf("%s: validate: %v", basis, err)
			}
			t.Logf("%s part: evaluated=%d dynamic=%d not-applicable=%d violations=%d",
				basis, report.Evaluated, report.Dynamic, report.NotApplicable, len(report.Violations))
			for _, v := range report.Violations {
				t.Logf("  violation %s", v)
			}
			if !report.OK() {
				t.Fatalf("%s: conformance violations", basis)
			}
		}
	}
}

func TestFocusManifest(t *testing.T) {
	w := &FocusWorker{Now: func() time.Time { return time.Date(2026, 4, 16, 6, 0, 0, 0, time.UTC) }}
	fc := testFocusContext("cost")
	var rows [][]string
	for _, r := range syntheticRows() {
		rows = append(rows, r.render(fc))
	}
	_, parts := buildFocusParts(focusSink{Destination: Destination{ID: "s1"}}, fc.day, 2, rows)
	accounting := "7.0135"
	body, err := focusManifest(w, focusSink{Destination: Destination{ID: "s1"}, Name: "Warehouse", costBasis: "cost"}, fc, 2, result{
		parts:          parts,
		rowCount:       int64(len(rows)),
		included:       8,
		fallbackLines:  1,
		unpriced:       2,
		incomplete:     1,
		uncharged:      3,
		callerExcluded: 4,
		ingestionGap:   false,
		accounting:     &accounting,
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["focus_version"] != "1.4" || doc["dataset"] != "CostAndUsage" || doc["revision"] != float64(2) {
		t.Fatalf("manifest header: %v", doc)
	}
	if doc["charge_date"] != "2026-04-15" || doc["billing_period"] != "2026-04" || doc["cost_basis"] != "cost" {
		t.Fatalf("manifest dates: %v", doc)
	}
	criteria := doc["applicability_criteria"].([]any)
	if len(criteria) != len(FocusCriteria) {
		t.Fatalf("criteria %v", criteria)
	}
	meta := doc["metadata"].(map[string]any)
	for _, member := range []string{"DataGenerator", "DatasetInstance", "Recency", "Schema"} {
		if meta[member] == nil {
			t.Fatalf("metadata lacks %s", member)
		}
	}
	schema := meta["Schema"].(map[string]any)
	if schema["FocusVersion"] != "1.4" || schema["SchemaId"] == "" || len(schema["ColumnDefinition"].([]any)) != len(focusColumns) {
		t.Fatalf("schema metadata: %v", schema)
	}
	instance := meta["DatasetInstance"].(map[string]any)
	if instance["FocusDatasetId"] != "CostAndUsage" || instance["DatasetInstanceId"] == "" {
		t.Fatalf("dataset instance: %v", instance)
	}
	// Determinism: same inputs → same bytes.
	body2, _ := focusManifest(w, focusSink{Destination: Destination{ID: "s1"}, Name: "Warehouse", costBasis: "cost"}, fc, 2, result{
		parts: parts, rowCount: int64(len(rows)), included: 8, fallbackLines: 1, unpriced: 2,
		incomplete: 1, uncharged: 3, callerExcluded: 4, accounting: &accounting,
	})
	if !bytes.Equal(body, body2) {
		t.Fatal("manifest not deterministic")
	}
}

func TestFocusEmptyDay(t *testing.T) {
	_, parts := buildFocusParts(focusSink{Destination: Destination{ID: "s"}}, time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC), 1, nil)
	if len(parts) != 1 {
		t.Fatalf("empty day parts = %d", len(parts))
	}
	data := readCSV(t, gunzip(t, parts[0].body))
	if len(data) != 1 {
		t.Fatalf("empty day rows = %d, want header only", len(data))
	}
	report, err := focusspec.Validate(bytes.NewReader(gunzip(t, parts[0].body)), FocusCriteria)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		for _, v := range report.Violations {
			t.Logf("violation %s", v)
		}
		t.Fatal("header-only part violates FOCUS")
	}
}

// A provider whose stored configuration lacks options.vendor_id resolves the
// kind's default vendor on both usage and purchase rows: they agree on SkuId
// and the contract's service names.
func TestFocusVendorFallbackAgreement(t *testing.T) {
	fc := testFocusContext("cost")
	usage := focusRow{
		providerID: "prov", connectionName: "custom", providerKind: "openai",
		model: "m", operation: "generation", component: "input_per_million",
		unit: "1000000 Tokens", listUnitPrice: dec("1"), contractedUnitPrice: dec("1"),
		quantity: dec("1"), listCost: dec("1"), contractedCost: dec("1"), attempts: 1,
	}
	usageRow := usage.render(fc)
	purchase := purchaseRow(fc, "prov", "custom", "", "openai", "", "", "day", "2026-04-15 00:00:00", dec("10"))
	get := func(row []string, col string) string { return row[slices.Index(focusColumns, col)] }
	if get(usageRow, "SkuId") != "openai/m/generation/input_per_million" {
		t.Fatalf("usage SkuId %s", get(usageRow, "SkuId"))
	}
	for _, row := range [][]string{usageRow, purchase} {
		if get(row, "ServiceName") != "OpenAI" || get(row, "ServiceProviderName") != "OpenAI" {
			t.Fatalf("service fields disagree: %s %s", get(row, "ServiceName"), get(row, "ServiceProviderName"))
		}
	}
	// A plugin provider keeps "plugin" as the SkuId prefix and the connection
	// name as the service names.
	plugin := focusRow{providerID: "p", connectionName: "Custom plugin", providerKind: "plugin",
		model: "m", operation: "generation", component: "per_request", unit: "Requests",
		listUnitPrice: dec("0.01"), contractedUnitPrice: dec("0.01"),
		quantity: dec("1"), listCost: dec("0.01"), contractedCost: dec("0.01"), attempts: 1}
	row := plugin.render(fc)
	if get(row, "SkuId") != "plugin/m/generation/per_request" || get(row, "ServiceName") != "Custom plugin" {
		t.Fatalf("plugin row: %s %s", get(row, "SkuId"), get(row, "ServiceName"))
	}
}
