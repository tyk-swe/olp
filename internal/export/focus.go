package export

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/tyk-swe/olp/internal/vendors"
)

// focusColumns is the fixed CostAndUsage column set, in header order. A FOCUS
// CSV writes every column every row; null is the empty field.
var focusColumns = []string{
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

// FocusCriteria is the applicability-criteria set the FOCUS datasets declare.
// The manifest carries it and tests pass it to focusspec.Validate.
var FocusCriteria = []string{
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

// FocusPartRowLimit caps one CSV part's data rows. It is a var so tests can
// exercise part rotation without generating fifty thousand rows.
var FocusPartRowLimit = 50000

// The correction-handling declaration the manifest repeats: late facts
// restate the day as a new revision replacing the previous one.
const focusCorrectionHandling = "Late facts restate the day's dataset as a new revision that replaces the previous revision in place."

// skuMeters maps a price-line component to the FOCUS SkuMeter label.
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

// skuMeter renders one component's meter label; tool fees and unknown
// components carry their own names.
func skuMeter(component string) string {
	if strings.HasPrefix(component, "tool_calls.") {
		return "Tool Calls (" + strings.TrimPrefix(component, "tool_calls.") + ")"
	}
	if label, ok := skuMeters[component]; ok {
		return label
	}
	return component
}

// consumedQuantity converts a pricing quantity into natural units; the token
// lines are priced per million so a millionth of a quantity scales back up.
func consumedQuantity(quantity decimal.Decimal, unit string) (decimal.Decimal, string) {
	if unit == "1000000 Tokens" {
		return quantity.Mul(decimal.NewFromInt(1000000)), "Tokens"
	}
	return quantity, unit
}

// focusRow is one grouped dataset row before rendering: the group's identity
// and its resolved display fields.
type focusRow struct {
	consumer, consumerName     string
	providerID, connectionName string
	vendorID, providerKind     string
	providerProject            string
	model, operation, route    string
	apiKeyID                   string
	attribution                string
	committed                  bool
	commitmentID               string
	allocatedCost              *decimal.Decimal
	commitmentPeriod           string
	commitmentBucket           string
	component                  string
	tier                       *string
	unit                       string
	listUnitPrice              decimal.Decimal
	contractedUnitPrice        decimal.Decimal
	quantity                   decimal.Decimal
	listCost, contractedCost   decimal.Decimal
	// periodWeight/latestAmount come from the shared commitment-shares CTE.
	periodWeight, latestAmount *decimal.Decimal
	attempts                   int64
	fallbackAttempts           int64
}

// vendor resolves the SkuId prefix: an explicit vendor option, else the
// kind's default vendor, else the kind itself. Plugin providers use "plugin".
// vendor is the SkuId prefix, resolving exactly the way serviceFieldsFor does.
func (r *focusRow) vendor() string {
	return vendorIDFor(r.vendorID, r.providerKind)
}

// vendorIDFor is the shared chain: a stored options.vendor_id, else the
// kind's default vendor, else the kind itself; plugin providers have no
// vendor at all.
func vendorIDFor(vendorID, kind string) string {
	if kind == "plugin" {
		return "plugin"
	}
	if vendorID != "" {
		return vendorID
	}
	return vendors.DefaultFor(kind)
}

func (r *focusRow) skuID() string {
	return strings.Join([]string{r.vendor(), r.model, r.operation, r.component}, "/")
}

func (r *focusRow) skuPriceID(costBasis string) string {
	price := r.listUnitPrice
	if costBasis == "charge" {
		price = r.contractedUnitPrice
	}
	id := r.skuID() + "@" + price.String()
	if r.tier != nil {
		id += "@above=" + *r.tier
	}
	return id
}

// serviceFields resolves the three provider-name columns: the vendor
// contract's name and maintainer, falling back to the connection. It is the
// same chain purchaseRow uses, so usage and purchase rows agree.
func (r *focusRow) serviceFields() (service, provider string) {
	return serviceFieldsFor(r.vendorID, r.providerKind, r.connectionName)
}

// focusContext carries the dataset's static fields: installation, currency,
// day bounds and month bounds every row shares.
type focusContext struct {
	installationID, installationName string
	currency                         string
	day                              time.Time
	monthStart, monthEnd             time.Time
	costBasis                        string
}

// render writes one Usage row's 51 fields in header order.
func (r *focusRow) render(ctx *focusContext) []string {
	row := make([]string, len(focusColumns))
	set := func(name string, value string) {
		row[slices.Index(focusColumns, name)] = value
	}
	service, provider := r.serviceFields()
	consumed, consumedUnit := consumedQuantity(r.quantity, r.unit)
	dayStart := ctx.day.UTC().Format("2006-01-02T15:04:05Z")
	dayEnd := ctx.day.UTC().AddDate(0, 0, 1).Format("2006-01-02T15:04:05Z")

	set("BillingAccountId", ctx.installationID)
	set("BillingAccountName", ctx.installationName)
	set("BillingCurrency", ctx.currency)
	set("BillingPeriodStart", ctx.monthStart.Format("2006-01-02T15:04:05Z"))
	set("BillingPeriodEnd", ctx.monthEnd.Format("2006-01-02T15:04:05Z"))
	set("ChargeCategory", "Usage")
	set("ChargeFrequency", "Usage-Based")
	set("ChargePeriodStart", dayStart)
	set("ChargePeriodEnd", dayEnd)
	set("ChargeDescription", fmt.Sprintf("%s for %s %s via %s", skuMeter(r.component), r.model, r.operation, r.connectionName))
	set("PricingQuantity", r.quantity.String())
	set("PricingUnit", r.unit)
	set("ListUnitPrice", r.listUnitPrice.String())
	set("ContractedUnitPrice", r.contractedUnitPrice.String())
	set("ListCost", r.listCost.String())
	set("ContractedCost", r.contractedCost.String())
	set("ConsumedQuantity", consumed.String())
	set("ConsumedUnit", consumedUnit)
	set("ServiceCategory", "AI and Machine Learning")
	set("ServiceSubcategory", "Generative AI")
	set("ServiceName", service)
	set("HostProviderName", provider)
	set("SkuId", r.skuID())
	set("SkuMeter", skuMeter(r.component))
	set("SkuPriceId", r.skuPriceID(ctx.costBasis))
	if r.consumer != "" {
		set("SubAccountId", r.consumer)
		set("SubAccountName", r.consumerName)
	}
	if r.attribution != "" && r.attribution != "{}" {
		set("Tags", r.attribution)
	}
	set("x_ApiKeyId", r.apiKeyID)
	set("x_Attempts", fmt.Sprint(r.attempts))
	set("x_ConnectionId", r.providerID)
	set("x_ConnectionName", r.connectionName)
	set("x_Model", r.model)
	set("x_Operation", r.operation)
	set("x_PriceComponent", r.component)
	if r.tier != nil {
		set("x_PriceTier", *r.tier)
	}
	set("x_Route", r.route)

	invoiceIssuer := provider
	if ctx.costBasis == "charge" {
		// Charge rows describe what the operator billed callers, so the
		// installation is the issuer.
		invoiceIssuer = ctx.installationName
		set("ListCost", r.contractedCost.String())
		set("ListUnitPrice", r.contractedUnitPrice.String())
		set("BilledCost", r.contractedCost.String())
		set("EffectiveCost", r.contractedCost.String())
		set("PricingCategory", "Standard")
	} else if r.committed && (r.allocatedCost != nil || r.latestAmount != nil && r.periodWeight != nil && !r.periodWeight.IsZero()) {
		// Committed rows bill nothing at list; their effective cost is the
		// group's ListCost share of the bucket's amount.
		allocated := decimal.Zero
		if r.allocatedCost != nil {
			allocated = *r.allocatedCost
		} else {
			allocated = r.listCost.Mul(*r.latestAmount).Div(*r.periodWeight).Round(12)
		}
		set("BilledCost", "0")
		set("EffectiveCost", allocated.String())
		set("PricingCategory", "Committed")
		set("CommitmentDiscountId", fmt.Sprintf("olp:commitment:%s:%s", r.providerID, r.commitmentPeriod))
		if r.commitmentID != "" {
			set("CommitmentDiscountId", "olp:commitment:"+r.commitmentID)
		}
		set("ResourceId", "olp:connection:"+r.providerID)
		set("ResourceName", r.connectionName)
		set("ResourceType", "AI connection")
		set("CommitmentDiscountName", fmt.Sprintf("%s %s commitment", r.connectionName, r.commitmentPeriod))
		set("CommitmentDiscountCategory", "Spend")
		set("CommitmentDiscountType", "Provisioned capacity")
		set("CommitmentDiscountStatus", "Used")
		set("CommitmentDiscountQuantity", allocated.String())
		set("CommitmentDiscountUnit", ctx.currency)
	} else {
		set("BilledCost", r.contractedCost.String())
		set("EffectiveCost", r.contractedCost.String())
		set("PricingCategory", "Standard")
	}
	set("InvoiceIssuerName", invoiceIssuer)
	set("ServiceProviderName", provider)
	if ctx.costBasis == "charge" {
		set("ServiceProviderName", ctx.installationName)
	}
	return row
}

// purchaseRow renders the commitment Purchase row for one bucket.
func purchaseRow(ctx *focusContext, providerID, connectionName, vendorID, kind, providerProject, providerProjectName, period, bucketStart string, amount decimal.Decimal) []string {
	row := make([]string, len(focusColumns))
	set := func(name, value string) { row[slices.Index(focusColumns, name)] = value }
	bucket, _ := time.Parse("2006-01-02 15:04:05", bucketStart)
	end := bucket
	switch period {
	case "hour":
		end = bucket.Add(time.Hour)
	case "day":
		end = bucket.AddDate(0, 0, 1)
	case "week":
		end = bucket.AddDate(0, 0, 7)
	case "month":
		end = bucket.AddDate(0, 1, 0)
	case "year":
		end = bucket.AddDate(1, 0, 0)
	}
	service, provider := serviceFieldsFor(vendorID, kind, connectionName)
	set("BillingAccountId", ctx.installationID)
	set("BillingAccountName", ctx.installationName)
	set("BillingCurrency", ctx.currency)
	set("BillingPeriodStart", ctx.monthStart.Format("2006-01-02T15:04:05Z"))
	set("BillingPeriodEnd", ctx.monthEnd.Format("2006-01-02T15:04:05Z"))
	set("ChargeCategory", "Purchase")
	set("ChargeFrequency", "Recurring")
	set("ChargePeriodStart", bucket.UTC().Format("2006-01-02T15:04:05Z"))
	set("ChargePeriodEnd", end.UTC().Format("2006-01-02T15:04:05Z"))
	set("ChargeDescription", fmt.Sprintf("%s %s commitment for %s", connectionName, period, bucket.Format("2006-01-02")))
	set("BilledCost", amount.String())
	set("EffectiveCost", "0")
	set("ContractedCost", amount.String())
	set("ListCost", amount.String())
	set("ListUnitPrice", amount.String())
	set("ContractedUnitPrice", amount.String())
	set("PricingQuantity", "1")
	set("PricingUnit", "Commitments")
	set("PricingCategory", "Standard")
	set("SkuId", "olp/commitment/"+period)
	set("SkuMeter", "Provisioned Capacity Commitment")
	set("SkuPriceId", "olp/commitment/"+period+"@"+amount.String())
	set("CommitmentDiscountId", fmt.Sprintf("olp:commitment:%s:%s", providerID, period))
	set("CommitmentDiscountName", fmt.Sprintf("%s %s commitment", connectionName, period))
	set("CommitmentDiscountCategory", "Spend")
	set("CommitmentDiscountType", "Provisioned capacity")
	set("CommitmentDiscountQuantity", amount.String())
	set("CommitmentDiscountUnit", ctx.currency)
	set("ServiceCategory", "AI and Machine Learning")
	set("ServiceSubcategory", "Generative AI")
	set("ServiceName", service)
	set("ServiceProviderName", provider)
	set("HostProviderName", provider)
	set("InvoiceIssuerName", provider)
	if providerProject != "" {
		set("SubAccountId", providerProject)
		set("SubAccountName", providerProjectName)
	}
	set("x_ConnectionId", providerID)
	set("x_ConnectionName", connectionName)
	set("x_Attempts", "0")
	return row
}

// serviceFieldsFor resolves service/provider names without a usage row's
// group context (used by purchase rows).
func serviceFieldsFor(vendorID, kind, connectionName string) (service, provider string) {
	id := vendorIDFor(vendorID, kind)
	// A plugin provider has no vendor contract: "plugin" names nothing.
	if id == "plugin" {
		id = ""
	}
	if id != "" {
		if c, ok := vendors.Lookup(id); ok {
			service, provider = c.Name, c.Maintainer
		}
	}
	if service == "" {
		service = connectionName
	}
	if provider == "" {
		provider = connectionName
	}
	return service, provider
}

// unusedRow is installation spend without a consumer or measured consumption.
func unusedRow(purchase []string, amount decimal.Decimal) []string {
	row := slices.Clone(purchase)
	set := func(k, v string) { row[slices.Index(focusColumns, k)] = v }
	set("ChargeCategory", "Usage")
	set("ChargeFrequency", "Usage-Based")
	set("ChargeDescription", "Unallocated provisioned capacity commitment")
	set("BilledCost", "0")
	set("EffectiveCost", amount.String())
	set("ListCost", "0")
	set("ContractedCost", "0")
	set("ListUnitPrice", "0")
	set("ContractedUnitPrice", "0")
	set("PricingQuantity", "0")
	set("PricingCategory", "Committed")
	set("CommitmentDiscountStatus", "Unused")
	set("CommitmentDiscountQuantity", amount.String())
	set("ResourceId", "olp:connection:"+cell(purchase, "x_ConnectionId"))
	set("ResourceName", cell(purchase, "x_ConnectionName"))
	set("ResourceType", "AI connection")
	set("SubAccountId", "")
	set("SubAccountName", "")
	set("x_Attempts", "")
	return row
}
