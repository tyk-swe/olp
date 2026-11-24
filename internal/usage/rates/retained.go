package rates

import (
	"encoding/json"
	"math/big"

	"github.com/shopspring/decimal"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	storageGiBDayDivisor    int64 = (1 << 30) * 86400
	cacheTokenHourDivisor   int64 = 1_000_000 * 3600
	retainedQuantityScale   int32 = 36
	trainingTokensDivisor   int64 = 1_000_000
	trainingUnitLabel             = "1000000 Training Tokens"
	storageUnitLabel              = "GiB Day"
	cacheStorageUnitLabel         = "1000000 Token Hours"
	containerSessionUnitLab       = "Session"
)

func DecodeContainerRates(p *Price, raw *string) error {
	if raw == nil {
		return nil
	}
	return json.Unmarshal([]byte(*raw), &p.ContainerPerSession)
}

// Retained quantities remain exact rationals until the single monetary rounding.
// GiB means 2^30 bytes; token-hours mean 3600 token-seconds.
func (r *priceRates) retainedTotal(base *big.Int, u usage.AttemptUsage, acc *detailAccumulator) (*big.Rat, bool) {
	if usage.ValidateRetainedUsage(u) != nil {
		return nil, false
	}
	total := new(big.Rat).SetFrac(base, new(big.Int).Exp(bigTen, big.NewInt(18), nil))
	add := func(component, unit string, tier *string, quantity *string, rate *string, divisor int64) bool {
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
		if acc != nil {
			acc.addComponent(component)
			if q.Sign() > 0 {
				qDec, qErr := decimal.NewFromString(*quantity)
				rDec, rErr := decimal.NewFromString(*rate)
				if qErr != nil || rErr != nil {
					return false
				}
				qty := qDec
				if divisor != 1 {
					qty = qDec.DivRound(decimal.NewFromInt(divisor), retainedQuantityScale)
				}
				acc.addLine(component, tier, unit, qty, rDec)
			}
		}
		return true
	}
	p := r.retained
	var training *string
	if u.TrainingTokens != nil {
		v := big.NewInt(*u.TrainingTokens).String()
		training = &v
	}
	if !add("training_per_million", trainingUnitLabel, nil, training, p.TrainingPerMillion, trainingTokensDivisor) ||
		!add("storage_per_gib_day", storageUnitLabel, nil, u.StorageByteSeconds, p.StoragePerGiBDay, storageGiBDayDivisor) ||
		!add("cache_storage_per_million_token_hour", cacheStorageUnitLabel, nil, u.CacheTokenSeconds, p.CacheStoragePerMillionTokenHour, cacheTokenHourDivisor) {
		return nil, false
	}
	for tier, count := range u.ContainerSessions {
		rate, ok := p.ContainerPerSession[tier]
		if !ok {
			return nil, false
		}
		q := big.NewInt(count).String()
		tierCopy := tier
		if !add("container_per_session."+tier, containerSessionUnitLab, &tierCopy, &q, &rate, 1) {
			return nil, false
		}
	}
	return total, true
}

func roundRetainedCost(base *big.Rat, window, batch, discount *big.Int) Cost {
	total := new(big.Rat).Set(base)
	if window != nil {
		total.Mul(total, new(big.Rat).SetFrac(window, costUnit))
	}
	if batch != nil {
		total.Mul(total, new(big.Rat).SetFrac(batch, costUnit))
	}
	if discount != nil {
		total.Mul(total, new(big.Rat).SetFrac(discount, costUnit))
	}
	total.Mul(total, new(big.Rat).SetInt(costUnit))
	total.Add(total, big.NewRat(1, 2))
	return Cost{units: new(big.Int).Quo(total.Num(), total.Denom())}
}
