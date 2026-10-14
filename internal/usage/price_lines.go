package usage

import (
	"encoding/json"
	"errors"

	"github.com/shopspring/decimal"
)

var errInvalidLines = errors.New("price lines carry a malformed number")

// priceLine is one SKU line of a priced attempt as olp.price_usage_lines
// writes it: every number arrives as an exact decimal string.
type priceLine struct {
	Component           string  `json:"component"`
	Tier                *string `json:"tier"`
	Unit                string  `json:"unit"`
	Quantity            string  `json:"quantity"`
	ListUnitPrice       string  `json:"list_unit_price"`
	ContractedUnitPrice string  `json:"contracted_unit_price"`
}

// sumPriceLines totals a lines document twice — once on the list unit prices,
// once on the contracted ones — with exact decimal arithmetic. A document that
// does not decode or carries a malformed number is an error.
func sumPriceLines(raw []byte) (list, contracted decimal.Decimal, err error) {
	var lines []priceLine
	if err := json.Unmarshal(raw, &lines); err != nil {
		return decimal.Zero, decimal.Zero, err
	}
	list, contracted = decimal.Zero, decimal.Zero
	for _, line := range lines {
		quantity, qerr := decimal.NewFromString(line.Quantity)
		listPrice, lerr := decimal.NewFromString(line.ListUnitPrice)
		contractedPrice, cerr := decimal.NewFromString(line.ContractedUnitPrice)
		if qerr != nil || lerr != nil || cerr != nil {
			return decimal.Zero, decimal.Zero, errInvalidLines
		}
		list = list.Add(quantity.Mul(listPrice))
		contracted = contracted.Add(quantity.Mul(contractedPrice))
	}
	return list, contracted, nil
}

// costLinesKept is the persistence guard for cost lines: the lines stand only
// when their sums re-derive the stored list and contracted costs at twelve
// decimal places — the same rounding olp.price_usage applies. A stored cost
// without reconstructing lines, or lines beside a missing cost, is dropped so
// a fact never claims a decomposition it does not have.
func costLinesKept(raw []byte, listCost, contractedCost *string) []byte {
	if len(raw) == 0 {
		return nil
	}
	if listCost == nil || contractedCost == nil {
		return nil
	}
	list, contracted, err := sumPriceLines(raw)
	if err != nil {
		return nil
	}
	wantList, err := decimal.NewFromString(*listCost)
	if err != nil {
		return nil
	}
	wantContracted, err := decimal.NewFromString(*contractedCost)
	if err != nil {
		return nil
	}
	if !list.Round(12).Equal(wantList) || !contracted.Round(12).Equal(wantContracted) {
		return nil
	}
	return raw
}

// chargeLinesKept is the charge-side guard: the lines stand only when their
// contracted total re-derives the stored charge.
func chargeLinesKept(raw []byte, charge *string) []byte {
	if len(raw) == 0 {
		return nil
	}
	if charge == nil {
		return nil
	}
	_, contracted, err := sumPriceLines(raw)
	if err != nil {
		return nil
	}
	want, err := decimal.NewFromString(*charge)
	if err != nil {
		return nil
	}
	if !contracted.Round(12).Equal(want) {
		return nil
	}
	return raw
}
