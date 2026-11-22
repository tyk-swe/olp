package protocols

import (
	"encoding/json"
	"math"
)

func isJSONSpace(b byte) bool  { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }
func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

type decimalSpan struct {
	negative            bool
	start, end          int
	digits, first, last int64
	fraction            int64
	exponent            int64
	exponentLarge       bool
}

func scanCohereDecimal(raw []byte) (decimalSpan, bool) {
	i := 0
	negative := raw[i] == '-'
	if negative {
		i++
	}
	start := i
	digits, first, last := int64(0), int64(-1), int64(-1)
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
	if i == len(raw) || !isASCIIDigit(raw[i]) {
		return decimalSpan{}, false
	}
	if raw[i] == '0' {
		consume()
		if i < len(raw) && isASCIIDigit(raw[i]) {
			return decimalSpan{}, false
		}
	} else {
		for i < len(raw) && isASCIIDigit(raw[i]) {
			consume()
		}
	}
	fraction := int64(0)
	if i < len(raw) && raw[i] == '.' {
		i++
		for i < len(raw) && isASCIIDigit(raw[i]) {
			consume()
			fraction++
		}
		if fraction == 0 {
			return decimalSpan{}, false
		}
	}
	end := i
	exponent, exponentLarge, next, ok := scanCohereExponent(raw, i)
	if !ok || next != len(raw) {
		return decimalSpan{}, false
	}
	return decimalSpan{negative: negative, start: start, end: end, digits: digits, first: first, last: last, fraction: fraction, exponent: exponent, exponentLarge: exponentLarge}, true
}

func scanCohereExponent(raw []byte, i int) (int64, bool, int, bool) {
	if i == len(raw) || (raw[i] != 'e' && raw[i] != 'E') {
		return 0, false, i, true
	}
	i++
	minus := false
	if i < len(raw) && (raw[i] == '+' || raw[i] == '-') {
		minus = raw[i] == '-'
		i++
	}
	if i == len(raw) || !isASCIIDigit(raw[i]) {
		return 0, false, i, false
	}
	// A nonzero coefficient cannot become an int64 when an exponent is
	// larger than the whole spelling plus int64's maximum digit count.
	bound := int64(len(raw)) + 19
	exponent, exponentLarge := int64(0), false
	for i < len(raw) && isASCIIDigit(raw[i]) {
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
	return exponent, exponentLarge, i, true
}

// cohereTokens projects integral JSON numbers exactly. Cohere also spells token
// counts as decimals, so decoding through float64 can round valid counts. The
// coefficient and exponent are scanned without allocating exponent-sized data.
func cohereTokens(raw json.RawMessage) (int64, bool) {
	for len(raw) > 0 && isJSONSpace(raw[0]) {
		raw = raw[1:]
	}
	for len(raw) > 0 && isJSONSpace(raw[len(raw)-1]) {
		raw = raw[:len(raw)-1]
	}
	if len(raw) == 0 {
		return 0, false
	}
	span, ok := scanCohereDecimal(raw)
	if !ok {
		return 0, false
	}
	if span.first < 0 {
		return 0, true
	}
	if span.negative || span.exponentLarge {
		return 0, false
	}
	power := span.exponent - span.fraction + span.digits - span.last - 1
	if power < 0 || span.last-span.first+1+power > 19 {
		return 0, false
	}
	var result int64
	ordinal := int64(0)
	for _, b := range raw[span.start:span.end] {
		if b == '.' {
			continue
		}
		if ordinal >= span.first && ordinal <= span.last {
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
