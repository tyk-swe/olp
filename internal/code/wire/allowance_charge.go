package wire

import (
	"encoding/json"
	"math"
	"strconv"

	"github.com/tyk-swe/olp/internal/oif"
)

type chargeTokens struct {
	Input        *int64 `json:"input,omitempty"`
	Output       *int64 `json:"output,omitempty"`
	Reasoning    *int64 `json:"reasoning,omitempty"`
	CacheRead    *int64 `json:"cache_read,omitempty"`
	CacheWrite5m *int64 `json:"cache_write_5m,omitempty"`
	CacheWrite1h *int64 `json:"cache_write_1h,omitempty"`
}

type allowanceCharge struct {
	BillingSource string        `json:"billing_source,omitempty"`
	ListCost      *json.Number  `json:"list_cost,omitempty"`
	Allowance     *json.Number  `json:"allowance,omitempty"`
	Cost          *json.Number  `json:"cost,omitempty"`
	Tokens        *chargeTokens `json:"tokens,omitempty"`
}

func AllowanceCharge(usage oif.Value) json.RawMessage {
	vendorObj := field(usage, "opencode")
	if vendorObj.Kind() != oif.Object {
		return nil
	}
	charge := allowanceCharge{
		BillingSource: billingSource(field(vendorObj, "billing_source")),
		ListCost:      chargeAmount(field(vendorObj, "list_cost")),
		Allowance:     chargeAmount(field(vendorObj, "allowance")),
		Cost:          chargeAmount(field(usage, "cost")),
		Tokens:        reportedTokens(field(vendorObj, "tokens")),
	}
	if charge == (allowanceCharge{}) {
		return nil
	}
	raw, err := json.Marshal(charge)
	if err != nil {
		return nil
	}
	return raw
}

func reportedTokens(tokens oif.Value) *chargeTokens {
	values := chargeTokens{
		Input: count(field(tokens, "input")), Output: count(field(tokens, "output")),
		Reasoning: count(field(tokens, "reasoning")), CacheRead: count(field(tokens, "cache_read")),
		CacheWrite5m: count(field(tokens, "cache_write_5m")), CacheWrite1h: count(field(tokens, "cache_write_1h")),
	}
	if values == (chargeTokens{}) {
		return nil
	}
	return &values
}

func billingSource(value oif.Value) string {
	text, _ := value.Text()
	switch text {
	case "free", "balance", "go", "go-plus", "byok":
		return text
	}
	return ""
}

func chargeAmount(value oif.Value) *json.Number {
	if value.Kind() != oif.Number {
		return nil
	}
	amount, err := strconv.ParseFloat(value.Raw(), 64)
	if err != nil || math.IsInf(amount, 0) || math.IsNaN(amount) || math.Signbit(amount) {
		return nil
	}
	number := json.Number(value.Raw())
	return &number
}
