package protocols

import (
	"encoding/json"
	"math"
)

// cohereTokens projects integral JSON numbers exactly. Cohere also spells token
// counts as decimals, so decoding through float64 can round valid counts. The
// coefficient and exponent are scanned without allocating exponent-sized data.
func cohereTokens(raw json.RawMessage) (int64, bool) {
	space := func(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }
	for len(raw) > 0 && space(raw[0]) {
		raw = raw[1:]
	}
	for len(raw) > 0 && space(raw[len(raw)-1]) {
		raw = raw[:len(raw)-1]
	}
	if len(raw) == 0 {
		return 0, false
	}
	i := 0
	negative := raw[i] == '-'
	if negative {
		i++
	}
	start := i
	digits, first, last := int64(0), int64(-1), int64(-1)
	digit := func(b byte) bool { return b >= '0' && b <= '9' }
	consume := func() {
		if raw[i] != '0' {
			if first < 0 {
				first = digits
			}
			last = digits
		}
		digits++
		i++
	}
	if i == len(raw) || !digit(raw[i]) {
		return 0, false
	}
	if raw[i] == '0' {
		consume()
		if i < len(raw) && digit(raw[i]) {
			return 0, false
		}
	} else {
		for i < len(raw) && digit(raw[i]) {
			consume()
		}
	}
	fraction := int64(0)
	if i < len(raw) && raw[i] == '.' {
		i++
		for i < len(raw) && digit(raw[i]) {
			consume()
			fraction++
		}
		if fraction == 0 {
			return 0, false
		}
	}
	end := i
	exponent, exponentLarge := int64(0), false
	if i < len(raw) && (raw[i] == 'e' || raw[i] == 'E') {
		i++
		minus := false
		if i < len(raw) && (raw[i] == '+' || raw[i] == '-') {
			minus = raw[i] == '-'
			i++
		}
		if i == len(raw) || !digit(raw[i]) {
			return 0, false
		}
		// A nonzero coefficient cannot become an int64 when an exponent is
		// larger than the whole spelling plus int64's maximum digit count.
		bound := int64(len(raw)) + 19
		for i < len(raw) && digit(raw[i]) {
			n := int64(raw[i] - '0')
			if exponent > (bound-n)/10 {
				exponentLarge = true
			} else if !exponentLarge {
				exponent = exponent*10 + n
			}
			i++
		}
		if minus {
			exponent = -exponent
		}
	}
	if i != len(raw) {
		return 0, false
	}
	if first < 0 {
		return 0, true
	}
	if negative || exponentLarge {
		return 0, false
	}
	power := exponent - fraction + digits - last - 1
	if power < 0 || last-first+1+power > 19 {
		return 0, false
	}
	var result int64
	ordinal := int64(0)
	for _, b := range raw[start:end] {
		if b == '.' {
			continue
		}
		if ordinal >= first && ordinal <= last {
			n := int64(b - '0')
			if result > (math.MaxInt64-n)/10 {
				return 0, false
			}
			result = result*10 + n
		}
		ordinal++
	}
	for ; power > 0; power-- {
		if result > math.MaxInt64/10 {
			return 0, false
		}
		result *= 10
	}
	return result, true
}
