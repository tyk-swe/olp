package focus

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/export/destination"
)

type completeness struct {
	RawDetailUnavailable       bool  `json:"raw_detail_unavailable"`
	IncludedAttempts           int64 `json:"included_attempts"`
	FallbackLineAttempts       int64 `json:"fallback_line_attempts"`
	UnpricedAttempts           int64 `json:"unpriced_attempts"`
	IncompleteAttempts         int64 `json:"incomplete_attempts"`
	UnchargedAttempts          int64 `json:"uncharged_attempts"`
	CallerPaidExcludedAttempts int64 `json:"caller_paid_excluded_attempts"`
	IngestionGap               bool  `json:"ingestion_gap"`
}

type CommitmentTotals struct {
	PurchasedCost string `json:"purchased_cost"`
	AllocatedCost string `json:"allocated_cost"`
	UnusedCost    string `json:"unused_cost"`
	Provisional   bool   `json:"provisional"`
	Complete      bool   `json:"complete"`
}

type facts struct {
	completeness completeness
	accounting   *string
	commitments  *CommitmentTotals
}

func (f facts) complete() bool {
	c := f.completeness
	return !c.RawDetailUnavailable && !c.IngestionGap && c.IncompleteAttempts == 0 && c.UnpricedAttempts == 0 &&
		(f.commitments == nil || f.commitments.Complete && !f.commitments.Provisional)
}

type manifest struct {
	FocusVersion          string                `json:"focus_version"`
	Dataset               string                `json:"dataset"`
	CostBasis             destination.CostBasis `json:"cost_basis"`
	ChargeDate            string                `json:"charge_date"`
	BillingPeriod         string                `json:"billing_period"`
	Revision              int64                 `json:"revision"`
	GeneratedAt           string                `json:"generated_at"`
	InstallationID        string                `json:"installation_id"`
	SinkID                string                `json:"sink_id"`
	Currency              string                `json:"currency"`
	ApplicabilityCriteria []string              `json:"applicability_criteria"`
	CorrectionHandling    string                `json:"correction_handling"`
	Parts                 []part                `json:"parts"`
	Totals                manifestTotals        `json:"totals"`
	CommitmentTotals      *CommitmentTotals     `json:"commitment_totals"`
	AccountingTotals      accountingTotals      `json:"accounting_totals"`
	Completeness          completeness          `json:"completeness"`
	Metadata              metadata              `json:"metadata"`
}

type manifestTotals struct {
	Billed     string `json:"billed_cost"`
	Effective  string `json:"effective_cost"`
	List       string `json:"list_cost"`
	Contracted string `json:"contracted_cost"`
}

type accountingTotals struct {
	Cost   *string `json:"cost"`
	Charge *string `json:"charge"`
}

type metadata struct {
	DataGenerator   string          `json:"DataGenerator"`
	DatasetInstance datasetInstance `json:"DatasetInstance"`
	Recency         recency         `json:"Recency"`
	Schema          schema          `json:"Schema"`
}

type datasetInstance struct {
	DatasetInstanceID   string `json:"DatasetInstanceId"`
	DatasetInstanceName string `json:"DatasetInstanceName"`
	FocusDatasetID      string `json:"FocusDatasetId"`
}

type recency struct {
	DatasetInstanceID          string `json:"DatasetInstanceId"`
	RecencyLastUpdateDate      string `json:"RecencyLastUpdateDate"`
	DatasetInstanceLastUpdated string `json:"DatasetInstanceLastUpdated"`
	DatasetInstanceComplete    bool   `json:"DatasetInstanceComplete"`
}

type schema struct {
	SchemaID             string             `json:"SchemaId"`
	FocusVersion         string             `json:"FocusVersion"`
	DataGeneratorVersion string             `json:"DataGeneratorVersion"`
	CreationDate         string             `json:"CreationDate"`
	DatasetInstanceID    string             `json:"DatasetInstanceId"`
	ColumnDefinition     []columnDefinition `json:"ColumnDefinition"`
}

type columnDefinition struct {
	ColumnName       string `json:"ColumnName"`
	DataType         string `json:"DataType"`
	StringMaxLength  int    `json:"StringMaxLength,omitempty"`
	StringEncoding   string `json:"StringEncoding,omitempty"`
	NumericPrecision int    `json:"NumericPrecision,omitempty"`
	NumberScale      int    `json:"NumberScale,omitempty"`
}

var (
	schemaID          = newSchemaID()
	columnDefinitions = newColumnDefinitions()
)

func newColumnDefinitions() []columnDefinition {
	definitions := make([]columnDefinition, len(columns))
	for i, column := range columns {
		definitions[i] = columnDefinitionFor(column)
	}
	return definitions
}

func columnDefinitionFor(column string) columnDefinition {
	switch column {
	case "BillingPeriodEnd", "BillingPeriodStart", "ChargePeriodEnd", "ChargePeriodStart":
		return columnDefinition{ColumnName: column, DataType: "DATETIME"}
	case "BilledCost", "CommitmentDiscountQuantity", "ConsumedQuantity", "ContractedCost", "ContractedUnitPrice",
		"EffectiveCost", "ListCost", "ListUnitPrice", "PricingQuantity":
		return columnDefinition{ColumnName: column, DataType: "DECIMAL", NumericPrecision: 30, NumberScale: 12}
	case "Tags":
		return columnDefinition{ColumnName: column, DataType: "JSON"}
	case "x_Attempts":
		return columnDefinition{ColumnName: column, DataType: "INTEGER"}
	default:
		return columnDefinition{ColumnName: column, DataType: "STRING", StringMaxLength: 1024, StringEncoding: "UTF-8"}
	}
}

func newSchemaID() string {
	sum := sha256.Sum256([]byte(strings.Join(columns, ",")))
	h := hex.EncodeToString(sum[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

func buildManifest(d dataset, revision int64, generatedAt time.Time, generator string, parts []part, sum totals, f facts) ([]byte, error) {
	stamp := generatedAt.UTC().Format(timestampLayout)
	instanceID := d.sinkID + "-" + d.installationID
	accounting := accountingTotals{Cost: f.accounting}
	if d.basis.billsCallers() {
		accounting = accountingTotals{Charge: f.accounting}
	}
	return json.Marshal(manifest{
		FocusVersion:          focusVersion,
		Dataset:               datasetName,
		CostBasis:             d.basis.name,
		ChargeDate:            d.day.Format(time.DateOnly),
		BillingPeriod:         d.day.Format("2006-01"),
		Revision:              revision,
		GeneratedAt:           stamp,
		InstallationID:        d.installationID,
		SinkID:                d.sinkID,
		Currency:              d.currency,
		ApplicabilityCriteria: applicabilityCriteria,
		CorrectionHandling:    correctionHandling,
		Parts:                 parts,
		Totals: manifestTotals{
			Billed:     amount(sum.billed),
			Effective:  amount(sum.effective),
			List:       amount(sum.list),
			Contracted: amount(sum.contracted),
		},
		CommitmentTotals: f.commitments,
		AccountingTotals: accounting,
		Completeness:     f.completeness,
		Metadata: metadata{
			DataGenerator: "OLP " + generator,
			DatasetInstance: datasetInstance{
				DatasetInstanceID:   instanceID,
				DatasetInstanceName: d.sinkName + " Cost and Usage Daily",
				FocusDatasetID:      datasetName,
			},
			Recency: recency{
				DatasetInstanceID:          instanceID,
				RecencyLastUpdateDate:      stamp,
				DatasetInstanceLastUpdated: stamp,
				DatasetInstanceComplete:    f.complete(),
			},
			Schema: schema{
				SchemaID:             schemaID,
				FocusVersion:         focusVersion,
				DataGeneratorVersion: generator,
				CreationDate:         stamp,
				DatasetInstanceID:    instanceID,
				ColumnDefinition:     columnDefinitions,
			},
		},
	})
}

func amount(d decimal.Decimal) string {
	if d.IsZero() {
		return "0"
	}
	return d.String()
}
