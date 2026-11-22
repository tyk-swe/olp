package admission

import (
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/limits"
)

const costGrace = 5 * time.Minute

type Request struct {
	Lookup     string
	LeaseTTL   time.Duration
	Rate       RateLimits
	Cost       *CostBudget
	Membership *Membership
}

type Membership struct {
	Member     string
	Generation int64
}

type RateLimits struct {
	RequestsPerMinute     *int64
	TokensPerMinute       *int64
	InputTokensPerMinute  *int64
	OutputTokensPerMinute *int64
	MaxConcurrency        *int64
	TrackConcurrency      bool
	Tokens                int64
	InputTokens           int64
	OutputTokens          int64
	Report                bool
	Share                 Share
	CreditCombined        bool
}

type Share struct {
	Class             string
	Percent           int64
	SaturationPercent int64
}

type CostBudget struct {
	Owner     limits.Owner
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

func Budget(owner limits.Owner, daily, weekly, monthly *string, increases string) *CostBudget {
	if daily == nil && weekly == nil && monthly == nil {
		return nil
	}
	return &CostBudget{Owner: owner, Daily: daily, Weekly: weekly, Monthly: monthly, Increases: increases}
}

func (r RateLimits) Limited() bool {
	return r.RequestsPerMinute != nil || r.TokensPerMinute != nil ||
		r.InputTokensPerMinute != nil || r.OutputTokensPerMinute != nil || r.MaxConcurrency != nil
}

func (r RateLimits) split() bool {
	return r.InputTokensPerMinute != nil || r.OutputTokensPerMinute != nil
}

func (r RateLimits) tokenLimited() bool {
	return r.TokensPerMinute != nil || r.split()
}

func (r RateLimits) applies() bool { return r.Limited() || r.TrackConcurrency }

func (r RateLimits) reports() bool {
	return r.Report && (r.RequestsPerMinute != nil || r.TokensPerMinute != nil)
}

func (r Request) HasHardLimits() bool { return r.Rate.Limited() || r.Cost != nil }

func (s Share) divides() bool { return s.Class != "" }

func (s Share) valid() bool {
	return len(s.Class) <= 16 && strings.Trim(s.Class, "abcdefghijklmnopqrstuvwxyz") == "" &&
		s.Percent >= 0 && s.Percent <= 100 && s.SaturationPercent >= 1 && s.SaturationPercent <= 100
}

func invalid(reason string) error { return &limits.InvalidRequestError{Reason: reason} }

func (r Request) validate() error {
	if !limits.ValidLookup(r.Lookup) {
		return invalid("lookup ID must be 8-40 ASCII letters, digits, or underscores")
	}
	if r.LeaseTTL.Milliseconds() < 1 {
		return invalid("lease TTL must be at least one millisecond")
	}
	if err := r.Rate.validate(); err != nil {
		return err
	}
	if r.Membership != nil && (r.Membership.Member == "" || len(r.Membership.Member) > 128 || r.Membership.Generation < 0 || r.Membership.Generation > limits.MaxCounter) {
		return invalid("membership needs a member of at most 128 characters and a non-negative generation")
	}
	if r.Cost == nil {
		return nil
	}
	return r.Cost.Validate()
}

func (r RateLimits) validate() error {
	for _, limit := range [...]struct {
		value *int64
		name  string
	}{
		{r.RequestsPerMinute, "requests per minute"},
		{r.TokensPerMinute, "tokens per minute"},
		{r.InputTokensPerMinute, "input tokens per minute"},
		{r.OutputTokensPerMinute, "output tokens per minute"},
		{r.MaxConcurrency, "max concurrency"},
	} {
		if limit.value == nil {
			continue
		}
		if *limit.value < 1 {
			return invalid(limit.name + " must be positive")
		}
		if *limit.value > limits.MaxCounter {
			return invalid(limit.name + " exceeds the Valkey Lua integer range")
		}
	}
	switch {
	case r.Tokens < 0:
		return invalid("requested tokens cannot be negative")
	case r.Tokens > limits.MaxCounter:
		return invalid("requested tokens exceed the Valkey Lua integer range")
	case r.InputTokens < 0 || r.OutputTokens < 0:
		return invalid("requested input and output tokens cannot be negative")
	case r.InputTokens > limits.MaxCounter || r.OutputTokens > limits.MaxCounter:
		return invalid("requested input and output tokens exceed the Valkey Lua integer range")
	case r.tokenLimited() && r.Tokens < 1:
		return invalid("requested tokens must be positive when a token limit applies")
	case r.split() && (r.InputTokens > r.Tokens || r.OutputTokens > r.Tokens):
		return invalid("requested input and output tokens cannot exceed the requested tokens when a split token limit applies")
	case r.Share.divides() && !r.Share.valid():
		return invalid("a share names a lowercase class, a percent from 0 to 100 and a saturation from 1 to 100")
	}
	return nil
}

func (b CostBudget) Validate() error {
	if _, err := uuid.Parse(b.Owner.ID); err != nil {
		return invalid("cost owner ID must be a UUID")
	}
	if !b.Owner.Basis.Valid() {
		return invalid(`cost basis must be "cost" or "charge"`)
	}
	if b.Daily == nil && b.Weekly == nil && b.Monthly == nil {
		return invalid("a cost budget needs a daily, weekly or monthly limit")
	}
	for _, limit := range [...]struct {
		value *string
		name  string
	}{{b.Daily, "daily"}, {b.Monthly, "monthly"}, {b.Weekly, "weekly"}} {
		if limit.value != nil && !limits.ValidCostLimit(*limit.value) {
			return invalid(limit.name + " cost limit must be a positive decimal with at most 12 integer and 12 fractional digits")
		}
	}
	if b.Estimate == nil {
		return nil
	}
	return b.Estimate.validate()
}

func (e Estimate) validate() error {
	if !limits.ValidCostLimit(e.Amount) {
		return invalid("cost estimate must be a positive decimal with at most 12 integer and 12 fractional digits")
	}
	if _, err := uuid.Parse(e.RequestID); err != nil {
		return invalid("a cost estimate needs the request ID it will be accounted under")
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
