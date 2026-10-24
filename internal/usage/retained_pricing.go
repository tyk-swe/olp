package usage

import (
	"encoding/json"
	"errors"
	"math/big"
	"regexp"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

func stringJSON(value any) string {
	data, _ := json.Marshal(value)
	if string(data) == "null" {
		return "{}"
	}
	return string(data)
}
func decodeContainerRates(p *Price, raw *string) error {
	if raw == nil {
		return nil
	}
	return json.Unmarshal([]byte(*raw), &p.ContainerPerSession)
}
func retainedValues(u AttemptUsage) bool {
	return u.InvocationCount != nil || u.TrainingTokens != nil || u.StorageByteSeconds != nil || u.CacheTokenSeconds != nil || len(u.ContainerSessions) > 0
}
func validateRetainedUsage(u AttemptUsage) error {
	if u.InvocationCount != nil && *u.InvocationCount < 0 {
		return errors.New("negative invocation count")
	}
	if u.TrainingTokens != nil && *u.TrainingTokens < 0 {
		return errors.New("negative training tokens")
	}
	for _, quantity := range []*string{u.StorageByteSeconds, u.CacheTokenSeconds} {
		if quantity == nil {
			continue
		}
		if len(*quantity) > 128 {
			return errors.New("retained quantity too large")
		}
		if !retainedQuantityPattern.MatchString(*quantity) {
			return errors.New("invalid retained quantity")
		}
	}
	if len(u.ContainerSessions) > 16 {
		return errors.New("too many memory tiers")
	}
	for tier, count := range u.ContainerSessions {
		if !memoryTierPattern.MatchString(tier) || count < 0 {
			return errors.New("invalid container memory tier or count")
		}
	}
	return nil
}

// Retained quantities remain exact rationals until the single monetary rounding.
// GiB means 2^30 bytes; token-hours mean 3600 token-seconds.
func (r *priceRates) retainedCost(base *big.Int, u AttemptUsage, at time.Time, discount *big.Int) (Cost, bool) {
	if validateRetainedUsage(u) != nil {
		return Cost{}, false
	}
	total := new(big.Rat).SetFrac(base, new(big.Int).Exp(bigTen, big.NewInt(18), nil))
	add := func(quantity *string, rate *string, divisor int64) bool {
		if quantity == nil {
			return true
		}
		if rate == nil {
			return false
		}
		q, ok := new(big.Rat).SetString(*quantity)
		if !ok || q.Sign() < 0 {
			return false
		}
		v, ok := new(big.Rat).SetString(*rate)
		if !ok || v.Sign() < 0 {
			return false
		}
		total.Add(total, new(big.Rat).Quo(new(big.Rat).Mul(q, v), new(big.Rat).SetInt64(divisor)))
		return true
	}
	p := r.retained
	var training *string
	if u.TrainingTokens != nil {
		v := big.NewInt(*u.TrainingTokens).String()
		training = &v
	}
	if !add(training, p.TrainingPerMillion, 1_000_000) || !add(u.StorageByteSeconds, p.StoragePerGiBDay, (1<<30)*86400) || !add(u.CacheTokenSeconds, p.CacheStoragePerMillionTokenHour, 1_000_000*3600) {
		return Cost{}, false
	}
	for tier, count := range u.ContainerSessions {
		rate, ok := p.ContainerPerSession[tier]
		if !ok {
			return Cost{}, false
		}
		q := big.NewInt(count).String()
		if !add(&q, &rate, 1) {
			return Cost{}, false
		}
	}
	if w := r.windowAt(at); w != nil {
		total.Mul(total, new(big.Rat).SetFrac(w, costUnit))
	}
	if u.Batch {
		if r.batch == nil {
			return Cost{}, false
		}
		total.Mul(total, new(big.Rat).SetFrac(r.batch, costUnit))
	}
	if discount != nil {
		total.Mul(total, new(big.Rat).SetFrac(discount, costUnit))
	}
	total.Mul(total, new(big.Rat).SetInt(costUnit))
	total.Add(total, big.NewRat(1, 2))
	return clampCost(new(big.Int).Quo(total.Num(), total.Denom())), true
}

var memoryTierPattern = regexp.MustCompile(`^[1-9][0-9]{0,4}g$`)

func normalizeContainerRates(rates map[string]string) error {
	if len(rates) > 16 {
		return access.Invalid("container_per_session", "At most sixteen memory tiers may be priced.")
	}
	for tier, fee := range rates {
		if !memoryTierPattern.MatchString(tier) {
			return access.Invalid("container_per_session", "Use a native memory tier such as 1g or 4g.")
		}
		normalized, err := normalizeDecimal(fee)
		if err != nil {
			return err
		}
		rates[tier] = normalized
	}
	return nil
}

var retainedQuantityPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)?$`)

func optionalJSON(value any) any {
	data, _ := json.Marshal(value)
	if string(data) == "null" {
		return nil
	}
	return string(data)
}
