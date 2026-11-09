package focus

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/vendors"
)

const (
	PartRowLimit       = 50000
	focusVersion       = "1.4"
	datasetName        = "CostAndUsage"
	timestampLayout    = "2006-01-02T15:04:05Z"
	correctionHandling = "Late facts restate the day's dataset as a new revision that replaces the previous revision in place."
)

var columns = []string{
	"BilledCost", "BillingAccountId", "BillingAccountName", "BillingCurrency",
	"BillingPeriodEnd", "BillingPeriodStart", "ChargeCategory", "ChargeClass",
	"ChargeDescription", "ChargeFrequency", "ChargePeriodEnd", "ChargePeriodStart",
	"CommitmentDiscountCategory", "CommitmentDiscountId", "CommitmentDiscountName",
	"CommitmentDiscountQuantity", "CommitmentDiscountStatus", "CommitmentDiscountType",
	"CommitmentDiscountUnit", "ConsumedQuantity", "ConsumedUnit", "ContractedCost",
	"ContractedUnitPrice", "EffectiveCost", "HostProviderName", "InvoiceIssuerName",
	"ListCost", "ListUnitPrice", "PricingCategory", "PricingQuantity", "PricingUnit",
	"ResourceId", "ResourceName", "ResourceType", "ServiceCategory", "ServiceName", "ServiceProviderName", "ServiceSubcategory",
	"SkuId", "SkuMeter", "SkuPriceDetails", "SkuPriceId", "SubAccountId",
	"SubAccountName", "Tags", "x_ApiKeyId", "x_Attempts", "x_ConnectionId",
	"x_ConnectionName", "x_Model", "x_Operation", "x_PriceComponent", "x_PriceTier",
	"x_Route",
}

var applicabilityCriteria = []string{
	"ACCOUNT_NAMING_SUPPORTED",
	"COMMITMENT_DISCOUNT_SUPPORTED",
	"MULTIPLE_PRICING_CATEGORIES_SUPPORTED",
	"NEGOTIATED_PRICING_SUPPORTED",
	"PUBLIC_PRICE_LIST_SUPPORTED",
	"SUB_ACCOUNT_SUPPORTED",
	"TAGGING_SUPPORTED",
	"UNIT_PRICING_SUPPORTED",
	"USAGE_MEASUREMENT_SUPPORTED",
}

var skuMeters = map[string]string{
	"input_per_million":                "Input Tokens",
	"cached_input_per_million":         "Cached Input Tokens",
	"cache_write_input_per_million":    "Cache Write Input Tokens",
	"cache_write_5m_input_per_million": "Cache Write 5m Input Tokens",
	"cache_write_1h_input_per_million": "Cache Write 1h Input Tokens",
	"output_per_million":               "Output Tokens",
	"audio_input_per_million":          "Audio Input Tokens",
	"cached_audio_input_per_million":   "Cached Audio Input Tokens",
	"audio_output_per_million":         "Audio Output Tokens",
	"image_input_per_million":          "Image Input Tokens",
	"cached_image_input_per_million":   "Cached Image Input Tokens",
	"image_output_per_million":         "Image Output Tokens",
	"unit_price":                       "Media Units",
	"per_request":                      "Requests",
	"attempt":                          "Attempts",
}

type dataset struct {
	sinkID, sinkName                 string
	installationID, installationName string
	currency                         string
	day                              time.Time
	basis                            basis
}

type usageRow struct {
	consumer, consumerName             string
	providerID, connectionName         string
	vendorID, providerKind             string
	model, operation, route            string
	apiKeyID, attribution              string
	committed                          bool
	scheduleID, commitmentPeriod       string
	allocated                          decimal.NullDecimal
	periodWeight, latestAmount         decimal.NullDecimal
	component                          string
	tier                               *string
	unit                               string
	listUnitPrice, contractedUnitPrice decimal.Decimal
	quantity, listCost, contractedCost decimal.Decimal
	attempts                           int64
}

type purchase struct {
	providerID, connectionName, vendorID, providerKind string
	scheduleID, period                                 string
	bucketStart                                        time.Time
	amount, unused                                     decimal.Decimal
}

func (d dataset) monthStart() time.Time {
	return time.Date(d.day.Year(), d.day.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func (d dataset) billing() map[string]string {
	return map[string]string{
		"BillingAccountId":   d.installationID,
		"BillingAccountName": d.installationName,
		"BillingCurrency":    d.currency,
		"BillingPeriodStart": d.monthStart().Format(timestampLayout),
		"BillingPeriodEnd":   d.monthStart().AddDate(0, 1, 0).Format(timestampLayout),
		"ServiceCategory":    "AI and Machine Learning",
		"ServiceSubcategory": "Generative AI",
	}
}

func (r usageRow) allocation(b basis) (decimal.Decimal, bool) {
	switch {
	case !r.committed || b.billsCallers():
		return decimal.Decimal{}, false
	case r.allocated.Valid:
		return r.allocated.Decimal, true
	case r.latestAmount.Valid && r.periodWeight.Valid && !r.periodWeight.Decimal.IsZero():
		return r.listCost.Mul(r.latestAmount.Decimal).Div(r.periodWeight.Decimal).Round(12), true
	default:
		return decimal.Decimal{}, false
	}
}

func render(r usageRow, d dataset) []string {
	service, provider := serviceNames(r.vendorID, r.providerKind, r.connectionName)
	listCost, listUnitPrice := r.listCost, r.listUnitPrice
	seller := provider
	if d.basis.billsCallers() {
		listCost, listUnitPrice, seller = r.contractedCost, r.contractedUnitPrice, d.installationName
	}
	consumed, consumedUnit := consumedQuantity(r.quantity, r.unit)
	skuID := strings.Join([]string{vendorID(r.vendorID, r.providerKind), r.model, r.operation, r.component}, "/")
	tier := ""
	priceID := skuID + "@" + listUnitPrice.String()
	if r.tier != nil {
		tier = *r.tier
		priceID += "@above=" + tier
	}
	usage := map[string]string{
		"ChargeCategory":      "Usage",
		"ChargeFrequency":     "Usage-Based",
		"ChargePeriodStart":   d.day.Format(timestampLayout),
		"ChargePeriodEnd":     d.day.AddDate(0, 0, 1).Format(timestampLayout),
		"ChargeDescription":   fmt.Sprintf("%s for %s %s via %s", skuMeter(r.component), r.model, r.operation, r.connectionName),
		"PricingQuantity":     r.quantity.String(),
		"PricingUnit":         r.unit,
		"ListUnitPrice":       listUnitPrice.String(),
		"ContractedUnitPrice": r.contractedUnitPrice.String(),
		"ListCost":            listCost.String(),
		"ContractedCost":      r.contractedCost.String(),
		"ConsumedQuantity":    consumed.String(),
		"ConsumedUnit":        consumedUnit,
		"ServiceName":         service,
		"HostProviderName":    provider,
		"ServiceProviderName": seller,
		"InvoiceIssuerName":   seller,
		"SkuId":               skuID,
		"SkuMeter":            skuMeter(r.component),
		"SkuPriceId":          priceID,
		"Tags":                tags(r.attribution),
		"x_ApiKeyId":          r.apiKeyID,
		"x_Attempts":          fmt.Sprint(r.attempts),
		"x_ConnectionId":      r.providerID,
		"x_ConnectionName":    r.connectionName,
		"x_Model":             r.model,
		"x_Operation":         r.operation,
		"x_PriceComponent":    r.component,
		"x_PriceTier":         tier,
		"x_Route":             r.route,
	}
	if r.consumer != "" {
		usage["SubAccountId"], usage["SubAccountName"] = r.consumer, r.consumerName
	}
	return ordered(d.billing(), usage, pricing(r, d))
}

func pricing(r usageRow, d dataset) map[string]string {
	allocated, ok := r.allocation(d.basis)
	if !ok {
		return map[string]string{
			"BilledCost":      r.contractedCost.String(),
			"EffectiveCost":   r.contractedCost.String(),
			"PricingCategory": "Standard",
		}
	}
	commitmentID := fmt.Sprintf("olp:commitment:%s:%s", r.providerID, r.commitmentPeriod)
	if r.scheduleID != "" {
		commitmentID = "olp:commitment:" + r.scheduleID
	}
	return map[string]string{
		"BilledCost":                 "0",
		"EffectiveCost":              allocated.String(),
		"PricingCategory":            "Committed",
		"CommitmentDiscountId":       commitmentID,
		"CommitmentDiscountName":     fmt.Sprintf("%s %s commitment", r.connectionName, r.commitmentPeriod),
		"CommitmentDiscountCategory": "Spend",
		"CommitmentDiscountType":     "Provisioned capacity",
		"CommitmentDiscountStatus":   "Used",
		"CommitmentDiscountQuantity": allocated.String(),
		"CommitmentDiscountUnit":     d.currency,
		"ResourceId":                 "olp:connection:" + r.providerID,
		"ResourceName":               r.connectionName,
		"ResourceType":               "AI connection",
	}
}

func purchaseRows(p purchase, d dataset) [][]string {
	service, provider := serviceNames(p.vendorID, p.providerKind, p.connectionName)
	amount := p.amount.String()
	bought := map[string]string{
		"ChargeCategory":             "Purchase",
		"ChargeFrequency":            "Recurring",
		"ChargePeriodStart":          p.bucketStart.Format(timestampLayout),
		"ChargePeriodEnd":            periodEnd(p.bucketStart, p.period).Format(timestampLayout),
		"ChargeDescription":          fmt.Sprintf("%s %s commitment for %s", p.connectionName, p.period, p.bucketStart.Format(time.DateOnly)),
		"BilledCost":                 amount,
		"EffectiveCost":              "0",
		"ContractedCost":             amount,
		"ListCost":                   amount,
		"ListUnitPrice":              amount,
		"ContractedUnitPrice":        amount,
		"PricingQuantity":            "1",
		"PricingUnit":                "Commitments",
		"PricingCategory":            "Standard",
		"SkuId":                      "olp/commitment/" + p.period,
		"SkuMeter":                   "Provisioned Capacity Commitment",
		"SkuPriceId":                 "olp/commitment/" + p.period + "@" + amount,
		"CommitmentDiscountId":       "olp:commitment:" + p.scheduleID,
		"CommitmentDiscountName":     fmt.Sprintf("%s %s commitment", p.connectionName, p.period),
		"CommitmentDiscountCategory": "Spend",
		"CommitmentDiscountType":     "Provisioned capacity",
		"CommitmentDiscountQuantity": amount,
		"CommitmentDiscountUnit":     d.currency,
		"ServiceName":                service,
		"ServiceProviderName":        provider,
		"HostProviderName":           provider,
		"InvoiceIssuerName":          provider,
		"x_ConnectionId":             p.providerID,
		"x_ConnectionName":           p.connectionName,
		"x_Attempts":                 "0",
	}
	if !p.unused.IsPositive() {
		return [][]string{ordered(d.billing(), bought)}
	}
	unused := map[string]string{
		"ChargeCategory":             "Usage",
		"ChargeFrequency":            "Usage-Based",
		"ChargeDescription":          "Unallocated provisioned capacity commitment",
		"BilledCost":                 "0",
		"EffectiveCost":              p.unused.String(),
		"ListCost":                   "0",
		"ContractedCost":             "0",
		"ListUnitPrice":              "0",
		"ContractedUnitPrice":        "0",
		"PricingQuantity":            "0",
		"PricingCategory":            "Committed",
		"CommitmentDiscountStatus":   "Unused",
		"CommitmentDiscountQuantity": p.unused.String(),
		"ResourceId":                 "olp:connection:" + p.providerID,
		"ResourceName":               p.connectionName,
		"ResourceType":               "AI connection",
		"x_Attempts":                 "",
	}
	return [][]string{ordered(d.billing(), bought), ordered(d.billing(), bought, unused)}
}

func ordered(layers ...map[string]string) []string {
	cells := map[string]string{}
	for _, layer := range layers {
		maps.Copy(cells, layer)
	}
	row := make([]string, len(columns))
	for i, name := range columns {
		row[i] = cells[name]
	}
	return row
}

func periodEnd(start time.Time, period string) time.Time {
	switch period {
	case "hour":
		return start.Add(time.Hour)
	case "day":
		return start.AddDate(0, 0, 1)
	case "week":
		return start.AddDate(0, 0, 7)
	case "month":
		return start.AddDate(0, 1, 0)
	case "year":
		return start.AddDate(1, 0, 0)
	default:
		return start
	}
}

func tags(attribution string) string {
	if attribution == "{}" {
		return ""
	}
	return attribution
}

func skuMeter(component string) string {
	if tool, ok := strings.CutPrefix(component, "tool_calls."); ok {
		return "Tool Calls (" + tool + ")"
	}
	if label, ok := skuMeters[component]; ok {
		return label
	}
	return component
}

func consumedQuantity(quantity decimal.Decimal, unit string) (decimal.Decimal, string) {
	if unit == "1000000 Tokens" {
		return quantity.Mul(decimal.NewFromInt(1000000)), "Tokens"
	}
	return quantity, unit
}

func vendorID(configured, kind string) string {
	switch {
	case kind == "plugin":
		return "plugin"
	case configured != "":
		return configured
	default:
		return vendors.DefaultFor(kind)
	}
}

func serviceNames(configured, kind, connectionName string) (service, provider string) {
	id := vendorID(configured, kind)
	contract, ok := vendors.Lookup(id)
	if id == "plugin" || !ok {
		return connectionName, connectionName
	}
	return cmpOr(contract.Name, connectionName), cmpOr(contract.Maintainer, connectionName)
}

func cmpOr(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

type part struct {
	Key    string `json:"key"`
	Rows   int64  `json:"rows"`
	Bytes  int    `json:"bytes"`
	SHA256 string `json:"sha256"`
	body   []byte
}

func partPrefix(day time.Time) string {
	return fmt.Sprintf("focus/v%s/billing_period=%s/charge_date=%s/", focusVersion, day.Format("2006-01"), day.Format(time.DateOnly))
}

func manifestKey(day time.Time) string {
	return partPrefix(day) + "manifest.json"
}

func buildParts(day time.Time, revision int64, rows [][]string) ([]part, error) {
	chunks := chunk(rows, PartRowLimit)
	parts := make([]part, len(chunks))
	for i, rows := range chunks {
		body, err := gzipCSV(rows)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		parts[i] = part{
			Key:    fmt.Sprintf("%srevision=%04d/part-%05d.csv.gz", partPrefix(day), revision, i),
			Rows:   int64(len(rows)),
			Bytes:  len(body),
			SHA256: hex.EncodeToString(sum[:]),
			body:   body,
		}
	}
	return parts, nil
}

func chunk(rows [][]string, size int) [][][]string {
	if len(rows) <= size {
		return [][][]string{rows}
	}
	return slices.Collect(slices.Chunk(rows, size))
}

func gzipCSV(rows [][]string) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	writer := csv.NewWriter(gz)
	if err := writer.Write(columns); err != nil {
		return nil, err
	}
	if err := writer.WriteAll(rows); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

type totals struct {
	billed, effective, list, contracted decimal.Decimal
}

var (
	billedColumn     = slices.Index(columns, "BilledCost")
	effectiveColumn  = slices.Index(columns, "EffectiveCost")
	listColumn       = slices.Index(columns, "ListCost")
	contractedColumn = slices.Index(columns, "ContractedCost")
)

func sumRows(rows [][]string) (totals, error) {
	sum := totals{}
	for _, row := range rows {
		billed, err := cellAmount(row[billedColumn])
		if err != nil {
			return totals{}, err
		}
		effective, err := cellAmount(row[effectiveColumn])
		if err != nil {
			return totals{}, err
		}
		list, err := cellAmount(row[listColumn])
		if err != nil {
			return totals{}, err
		}
		contracted, err := cellAmount(row[contractedColumn])
		if err != nil {
			return totals{}, err
		}
		sum = totals{
			billed:     sum.billed.Add(billed),
			effective:  sum.effective.Add(effective),
			list:       sum.list.Add(list),
			contracted: sum.contracted.Add(contracted),
		}
	}
	return sum, nil
}

func cellAmount(cell string) (decimal.Decimal, error) {
	if cell == "" {
		return decimal.Decimal{}, nil
	}
	return decimal.NewFromString(cell)
}
