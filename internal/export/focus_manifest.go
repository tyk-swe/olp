package export

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shopspring/decimal"
)

// FocusGeneratorVersion is stamped into the manifest metadata's
// DataGeneratorVersion; process/run.go assigns it the build version.
var FocusGeneratorVersion = "dev"

// The manifest is a stable JSON document: encoding/json writes struct fields
// in declaration order, so the same inputs always produce the same bytes.

type focusManifestDoc struct {
	FocusVersion          string              `json:"focus_version"`
	Dataset               string              `json:"dataset"`
	CostBasis             string              `json:"cost_basis"`
	ChargeDate            string              `json:"charge_date"`
	BillingPeriod         string              `json:"billing_period"`
	Revision              int64               `json:"revision"`
	GeneratedAt           string              `json:"generated_at"`
	InstallationID        string              `json:"installation_id"`
	SinkID                string              `json:"sink_id"`
	Currency              string              `json:"currency"`
	ApplicabilityCriteria []string            `json:"applicability_criteria"`
	CorrectionHandling    string              `json:"correction_handling"`
	Parts                 []focusManifestPart `json:"parts"`
	Totals                focusTotals         `json:"totals"`
	AccountingTotals      focusAccounting     `json:"accounting_totals"`
	Completeness          focusCompleteness   `json:"completeness"`
	Metadata              focusMetadata       `json:"metadata"`
}

type focusManifestPart struct {
	Key    string `json:"key"`
	Rows   int64  `json:"rows"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type focusTotals struct {
	Billed     string `json:"billed_cost"`
	Effective  string `json:"effective_cost"`
	List       string `json:"list_cost"`
	Contracted string `json:"contracted_cost"`
}

type focusAccounting struct {
	Cost   *string `json:"cost"`
	Charge *string `json:"charge"`
}

type focusCompleteness struct {
	IncludedAttempts           int64 `json:"included_attempts"`
	FallbackLineAttempts       int64 `json:"fallback_line_attempts"`
	UnpricedAttempts           int64 `json:"unpriced_attempts"`
	IncompleteAttempts         int64 `json:"incomplete_attempts"`
	UnchargedAttempts          int64 `json:"uncharged_attempts"`
	CallerPaidExcludedAttempts int64 `json:"caller_paid_excluded_attempts"`
	IngestionGap               bool  `json:"ingestion_gap"`
}

// The metadata object follows FOCUS 1.4 §5 and the Appendix 8.9 "Examples:
// Metadata" member names (https://focus.finops.org/docs/specification/v1-4/):
// DataGenerator, DatasetInstance{Id,Name,FocusDatasetId},
// Recency{DatasetInstanceId,RecencyLastUpdateDate,DatasetInstanceLastUpdated,
// DatasetInstanceComplete}, and Schema{SchemaId,FocusVersion,
// DataGeneratorVersion,CreationDate,DatasetInstanceId,ColumnDefinition}.
// A daily artifact is a whole dataset instance, so Recency takes the
// non-time-series shape the example gives it.
type focusMetadata struct {
	DataGenerator   string               `json:"DataGenerator"`
	DatasetInstance focusDatasetInstance `json:"DatasetInstance"`
	Recency         focusRecency         `json:"Recency"`
	Schema          focusSchema          `json:"Schema"`
}

type focusDatasetInstance struct {
	DatasetInstanceID   string `json:"DatasetInstanceId"`
	DatasetInstanceName string `json:"DatasetInstanceName"`
	FocusDatasetID      string `json:"FocusDatasetId"`
}

type focusRecency struct {
	DatasetInstanceID          string `json:"DatasetInstanceId"`
	RecencyLastUpdateDate      string `json:"RecencyLastUpdateDate"`
	DatasetInstanceLastUpdated string `json:"DatasetInstanceLastUpdated"`
	DatasetInstanceComplete    bool   `json:"DatasetInstanceComplete"`
}

type focusSchema struct {
	SchemaID             string                  `json:"SchemaId"`
	FocusVersion         string                  `json:"FocusVersion"`
	DataGeneratorVersion string                  `json:"DataGeneratorVersion"`
	CreationDate         string                  `json:"CreationDate"`
	DatasetInstanceID    string                  `json:"DatasetInstanceId"`
	ColumnDefinition     []focusColumnDefinition `json:"ColumnDefinition"`
}

type focusColumnDefinition struct {
	ColumnName       string `json:"ColumnName"`
	DataType         string `json:"DataType"`
	StringMaxLength  int    `json:"StringMaxLength,omitempty"`
	StringEncoding   string `json:"StringEncoding,omitempty"`
	NumericPrecision int    `json:"NumericPrecision,omitempty"`
	NumberScale      int    `json:"NumberScale,omitempty"`
}

// columnTypes assigns each FOCUS column its §5 schema DataType.
func focusColumnDefinitions() []focusColumnDefinition {
	datetimes := map[string]bool{"BillingPeriodEnd": true, "BillingPeriodStart": true,
		"ChargePeriodEnd": true, "ChargePeriodStart": true}
	decimals := map[string]bool{"BilledCost": true, "CommitmentDiscountQuantity": true,
		"ConsumedQuantity": true, "ContractedCost": true, "ContractedUnitPrice": true,
		"EffectiveCost": true, "ListCost": true, "ListUnitPrice": true, "PricingQuantity": true}
	definitions := make([]focusColumnDefinition, 0, len(focusColumns))
	for _, column := range focusColumns {
		switch {
		case datetimes[column]:
			definitions = append(definitions, focusColumnDefinition{ColumnName: column, DataType: "DATETIME"})
		case decimals[column]:
			definitions = append(definitions, focusColumnDefinition{ColumnName: column, DataType: "DECIMAL", NumericPrecision: 30, NumberScale: 12})
		case column == "Tags":
			definitions = append(definitions, focusColumnDefinition{ColumnName: column, DataType: "JSON"})
		case column == "x_Attempts":
			definitions = append(definitions, focusColumnDefinition{ColumnName: column, DataType: "INTEGER"})
		default:
			definitions = append(definitions, focusColumnDefinition{ColumnName: column, DataType: "STRING", StringMaxLength: 1024, StringEncoding: "UTF-8"})
		}
	}
	return definitions
}

// focusSchemaID is a stable identifier derived from the column set so a
// schema change yields a new id without any registry.
func focusSchemaID() string {
	sum := sha256.Sum256([]byte(strings.Join(focusColumns, ",")))
	h := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// focusManifest serializes the manifest document.
func focusManifest(w *FocusWorker, sink focusSink, fc *focusContext, revision int64, r result) ([]byte, error) {
	now := w.now().UTC()
	parts := make([]focusManifestPart, 0, len(r.parts))
	for _, part := range r.parts {
		parts = append(parts, focusManifestPart{Key: part.key, Rows: part.rows, Bytes: part.bytes, SHA256: part.sha256hex})
	}
	currency := fc.currency
	accounting := focusAccounting{}
	if sink.costBasis == "cost" {
		accounting.Cost = r.accounting
	} else {
		accounting.Charge = r.accounting
	}
	instanceID := sink.ID + "-" + fc.installationID
	doc := focusManifestDoc{
		FocusVersion:          "1.4",
		Dataset:               "CostAndUsage",
		CostBasis:             sink.costBasis,
		ChargeDate:            fc.day.Format("2006-01-02"),
		BillingPeriod:         fc.day.Format("2006-01"),
		Revision:              revision,
		GeneratedAt:           now.Format("2006-01-02T15:04:05Z"),
		InstallationID:        fc.installationID,
		SinkID:                sink.ID,
		Currency:              currency,
		ApplicabilityCriteria: FocusCriteria,
		CorrectionHandling:    focusCorrectionHandling,
		Parts:                 parts,
		Totals: focusTotals{
			Billed:     decimalOrZero(r.totals.billed),
			Effective:  decimalOrZero(r.totals.effective),
			List:       decimalOrZero(r.totals.list),
			Contracted: decimalOrZero(r.totals.contracted),
		},
		AccountingTotals: accounting,
		Completeness: focusCompleteness{
			IncludedAttempts:           r.included,
			FallbackLineAttempts:       r.fallbackLines,
			UnpricedAttempts:           r.unpriced,
			IncompleteAttempts:         r.incomplete,
			UnchargedAttempts:          r.uncharged,
			CallerPaidExcludedAttempts: r.callerExcluded,
			IngestionGap:               r.ingestionGap,
		},
		Metadata: focusMetadata{
			DataGenerator: "OLP " + FocusGeneratorVersion,
			DatasetInstance: focusDatasetInstance{
				DatasetInstanceID:   instanceID,
				DatasetInstanceName: sink.Name + " Cost and Usage Daily",
				FocusDatasetID:      "CostAndUsage",
			},
			Recency: focusRecency{
				DatasetInstanceID:          instanceID,
				RecencyLastUpdateDate:      now.Format("2006-01-02T15:04:05Z"),
				DatasetInstanceLastUpdated: now.Format("2006-01-02T15:04:05Z"),
				DatasetInstanceComplete:    !r.ingestionGap,
			},
			Schema: focusSchema{
				SchemaID:             focusSchemaID(),
				FocusVersion:         "1.4",
				DataGeneratorVersion: FocusGeneratorVersion,
				CreationDate:         now.Format("2006-01-02T15:04:05Z"),
				DatasetInstanceID:    instanceID,
				ColumnDefinition:     focusColumnDefinitions(),
			},
		},
	}
	return json.Marshal(doc)
}

func decimalOrZero(d decimal.Decimal) string {
	if d.IsZero() {
		return "0"
	}
	return d.String()
}
