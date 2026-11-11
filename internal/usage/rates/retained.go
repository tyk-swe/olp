package rates

import (
	"encoding/json"
	"math/big"
	"time"

	"github.com/tyk-swe/olp/internal/usage"
)

func DecodeContainerRates(p *Price, raw *string) error {
	if raw == nil {
		return nil
	}
	return json.Unmarshal([]byte(*raw), &p.ContainerPerSession)
}

// Retained quantities remain exact rationals until the single monetary rounding.
// GiB means 2^30 bytes; token-hours mean 3600 token-seconds.
func (r *priceRates) retainedCost(base *big.Int, u usage.AttemptUsage, at time.Time, discount *big.Int) (Cost, bool) {
	if usage.ValidateRetainedUsage(u) != nil {
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
