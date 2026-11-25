package limits

import "github.com/google/uuid"

type CostBudget struct {
	Owner     Owner
	Daily     *string
	Weekly    *string
	Monthly   *string
	Increases string
	Estimate  *Estimate
}

type Estimate struct {
	Amount    string
	RequestID string
}

type Boundary struct {
	Owner  Owner
	Budget *CostBudget
}

func Budget(owner Owner, daily, weekly, monthly *string, increases string) *CostBudget {
	if daily == nil && weekly == nil && monthly == nil {
		return nil
	}
	return &CostBudget{Owner: owner, Daily: daily, Weekly: weekly, Monthly: monthly, Increases: increases}
}

func (b CostBudget) Validate() error {
	if _, err := uuid.Parse(b.Owner.ID); err != nil {
		return &InvalidRequestError{Reason: "cost owner ID must be a UUID"}
	}
	if !b.Owner.Basis.Valid() {
		return &InvalidRequestError{Reason: `cost basis must be "cost" or "charge"`}
	}
	if b.Daily == nil && b.Weekly == nil && b.Monthly == nil {
		return &InvalidRequestError{Reason: "a cost budget needs a daily, weekly or monthly limit"}
	}
	for _, limit := range [...]struct {
		value *string
		name  string
	}{{b.Daily, "daily"}, {b.Monthly, "monthly"}, {b.Weekly, "weekly"}} {
		if limit.value != nil && !ValidCostLimit(*limit.value) {
			return &InvalidRequestError{Reason: limit.name + " cost limit must be a positive decimal with at most 12 integer and 12 fractional digits"}
		}
	}
	if b.Estimate == nil {
		return nil
	}
	return b.Estimate.validate()
}

func (e Estimate) validate() error {
	if !ValidCostLimit(e.Amount) {
		return &InvalidRequestError{Reason: "cost estimate must be a positive decimal with at most 12 integer and 12 fractional digits"}
	}
	if _, err := uuid.Parse(e.RequestID); err != nil {
		return &InvalidRequestError{Reason: "a cost estimate needs the request ID it will be accounted under"}
	}
	return nil
}

func (b *CostBudget) Holding(amount, requestID string) *CostBudget {
	if b == nil || amount == "" {
		return b
	}
	return &CostBudget{
		Owner: b.Owner, Daily: b.Daily, Weekly: b.Weekly, Monthly: b.Monthly, Increases: b.Increases,
		Estimate: &Estimate{Amount: amount, RequestID: requestID},
	}
}
