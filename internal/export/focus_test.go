package export

import (
	"bytes"
	"compress/gzip"
	"encoding/csv"
	"encoding/json"
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

// Every non-committed usage row: billed = effective = contracted.

// Committed row: billed 0, effective = list × amount/weight = 6×10/2 = 30? — wait, that is >0

// Tiered row carries the tier price id.

// Purchase row.

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

// A plugin provider keeps "plugin" as the SkuId prefix and the connection
// name as the service names.

func TestUnusedCommitmentConformance(t *testing.T) {
	fc := testFocusContext("cost")
	purchase := purchaseRow(fc, "capacity", "Reserved connection", "openai", "openai", "", "", "day", "2026-04-15 00:00:00", dec("1.000000000001"))
	unused := unusedRow(purchase, dec("1.000000000001"))
	for _, k := range []string{"SubAccountId", "SubAccountName", "ConsumedQuantity", "ConsumedUnit", "x_Route", "x_ApiKeyId", "x_Attempts"} {
		if v := cell(unused, k); v != "" {
			t.Errorf("unused %s=%q", k, v)
		}
	}
	if cell(unused, "EffectiveCost") != cell(purchase, "BilledCost") {
		t.Fatal("unused commitment did not reconcile")
	}
	for _, k := range []string{"ResourceId", "ResourceName", "ResourceType", "CommitmentDiscountId"} {
		if cell(unused, k) == "" {
			t.Fatalf("missing %s", k)
		}
	}
	var csvData bytes.Buffer
	writer := csv.NewWriter(&csvData)
	_ = writer.Write(focusColumns)
	_ = writer.Write(purchase)
	_ = writer.Write(unused)
	writer.Flush()
	report, err := focusspec.Validate(bytes.NewReader(csvData.Bytes()), FocusCriteria)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK() {
		t.Fatalf("unused conformance: %v", report.Violations)
	}
}
