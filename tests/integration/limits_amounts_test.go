//go:build integration

package integration_test

import (
	"fmt"
	"math/big"
	"math/rand/v2"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The cost scripts keep an amount as two exact integers, whole units and the
// fraction in units of 10^-12, because Lua cannot hold twelve fractional digits in
// a number. These tests run the helpers of that arithmetic inside Valkey and
// compare every answer with exact big-number arithmetic.

// limAmountHelpers is the cost_pending block the three cost scripts share, read
// from the script itself so that what is tested is what runs.
func limAmountHelpers(t *testing.T) string {
	t.Helper()
	source, err := os.ReadFile("../../internal/limits/scripts/reserve_cost.lua")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	begin, end := strings.Index(text, "-- BEGIN cost_pending"), strings.Index(text, "-- END cost_pending")
	if begin < 0 || end < begin {
		t.Fatal("reserve_cost.lua carries no cost_pending block")
	}
	return text[begin:end]
}

// limAmountDriver exposes the helpers to a script call: one operation on one or
// two amounts, answered in text so that nothing is rounded on the way back.
const limAmountDriver = `
local op = ARGV[1]
local left_hi, left_lo = parse_amount(ARGV[2])
if op == "parse" then
  if left_hi == nil then
    return {"invalid"}
  end
  return {string.format("%d", left_hi), string.format("%d", left_lo), format_amount(left_hi, left_lo)}
end
local right_hi, right_lo = parse_amount(ARGV[3])
if op == "add" then
  return {format_amount(add_amount(left_hi, left_lo, right_hi, right_lo))}
elseif op == "sub" then
  return {format_amount(sub_amount(left_hi, left_lo, right_hi, right_lo))}
end
return {tostring(compare_amount(left_hi, left_lo, right_hi, right_lo))}
`

var (
	limAmountText = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)
	// limUnits is how many units of 10^-12 make one whole unit.
	limUnits = big.NewInt(1_000_000_000_000)
	// limSaturated is the largest amount a script holds: 10^15 whole units, in
	// units of 10^-12.
	limSaturated = new(big.Int).Mul(big.NewInt(1_000_000_000_000_000), limUnits)
)

// limScaled is what the scripts must make of text: the amount in units of 10^-12,
// saturated at 10^15 whole units, or ok = false for text that is not an amount.
func limScaled(text string) (scaled *big.Int, ok bool) {
	if !limAmountText.MatchString(text) {
		return nil, false
	}
	whole, fraction, _ := strings.Cut(text, ".")
	if len(fraction) > 12 {
		fraction = strings.TrimRight(fraction, "0")
		if len(fraction) > 12 {
			return nil, false
		}
	}
	scaled, _ = new(big.Int).SetString(whole+fraction+strings.Repeat("0", 12-len(fraction)), 10)
	if scaled.Cmp(limSaturated) >= 0 {
		return new(big.Int).Set(limSaturated), true
	}
	return scaled, true
}

// limCanonical is the one way a script writes an amount: no leading zeros and no
// trailing fractional zeros.
func limCanonical(scaled *big.Int) string {
	whole, fraction := new(big.Int).DivMod(scaled, limUnits, new(big.Int))
	if fraction.Sign() == 0 {
		return whole.String()
	}
	return whole.String() + "." + strings.TrimRight(fmt.Sprintf("%012d", fraction), "0")
}

// limRandomAmount writes a decimal the way PostgreSQL, a caller or damage might:
// short and long, with leading and trailing zeros, at and around the limbs'
// boundaries.
func limRandomAmount(rng *rand.Rand) string {
	digits := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteByte(byte('0' + rng.IntN(10)))
		}
		return b.String()
	}
	nines := func(n int) string { return strings.Repeat("9", n) }
	switch rng.IntN(8) {
	case 0:
		return nines(1 + rng.IntN(16))
	case 1:
		return "0." + nines(1+rng.IntN(12))
	case 2:
		return nines(1+rng.IntN(15)) + "." + nines(12)
	case 3:
		return "0." + strings.Repeat("0", rng.IntN(12)) + "1"
	}
	whole := digits([]int{1, 1, 2, 3, 5, 11, 12, 13, 14, 15, 16, 17, 25}[rng.IntN(13)])
	if rng.IntN(5) == 0 {
		whole = "000" + whole
	}
	if rng.IntN(4) == 0 {
		return whole
	}
	fraction := digits([]int{1, 2, 3, 6, 11, 12, 12, 12, 13}[rng.IntN(9)])
	if rng.IntN(5) == 0 {
		fraction += strings.Repeat("0", 1+rng.IntN(3))
	}
	return whole + "." + fraction
}

func TestLimitsCostAmountArithmeticIsExactToTheTwelfthDigit(t *testing.T) {
	script := limAmountHelpers(t) + limAmountDriver
	c := limClient(t)
	run := func(args ...string) []string {
		t.Helper()
		items, ok := do(t, c, append([]string{"EVAL", script, "0"}, args...)...).([]any)
		if !ok {
			t.Fatalf("%q answered with something that is not a list", args)
		}
		out := make([]string, len(items))
		for index, item := range items {
			out[index] = limText(t, item)
		}
		return out
	}

	// What is not an amount is refused, and nothing else is: a script that read
	// the wrong thing here would count the wrong sum.
	for _, text := range []string{
		"", ".", "1.", ".5", "-1", "+1", "1e3", "0x10", " 1", "1 ", "1..2", "1.2.3", "1,5", "١٢",
		"0.0000000000001", "12.1234567890123", "NaN", "inf",
	} {
		if got := run("parse", text); len(got) != 1 || got[0] != "invalid" {
			t.Errorf("parse(%q) = %q, want it refused", text, got)
		}
	}

	rng := rand.New(rand.NewPCG(2024, 12))
	amounts := []string{
		"0", "0.0", "00", "1", "0.000000000001", "0.999999999999", "1.000000000000", "0.1", "0.5",
		"999999999999.999999999999", "999999999999999", "999999999999999.999999999999",
		"1000000000000000", "1000000000000000.5", "9007199254740993", "99999999999999999999999999",
		"0.1000000000000", "123456789012.345678901234",
	}
	for range 400 {
		amounts = append(amounts, limRandomAmount(rng))
	}
	for _, text := range amounts {
		scaled, ok := limScaled(text)
		got := run("parse", text)
		if !ok {
			if len(got) != 1 || got[0] != "invalid" {
				t.Errorf("parse(%q) = %q, want it refused", text, got)
			}
			continue
		}
		whole, fraction := new(big.Int).DivMod(scaled, limUnits, new(big.Int))
		if len(got) != 3 || got[0] != whole.String() || got[1] != fraction.String() || got[2] != limCanonical(scaled) {
			t.Errorf("parse(%q) = %q, want whole %s, fraction %s and %q", text, got, whole, fraction, limCanonical(scaled))
		}
	}

	pairs := [][2]string{
		{"0.999999999999", "0.000000000001"}, {"0.000000000001", "0.999999999999"}, {"1", "0.000000000001"},
		{"999999999999.999999999999", "0.000000000001"}, {"0.5", "0.5"}, {"0.5", "0.500000000001"}, {"0", "0"},
		{"999999999999999.999999999999", "0.000000000001"}, {"999999999999999", "1"}, {"1000000000000000", "1"},
		{"1000000000000000", "1000000000000000"}, {"7", "7"}, {"1000000000000", "0.000000000001"},
	}
	for range 1500 {
		pairs = append(pairs, [2]string{limRandomAmount(rng), limRandomAmount(rng)})
	}
	for _, pair := range pairs {
		left, leftOK := limScaled(pair[0])
		right, rightOK := limScaled(pair[1])
		if !leftOK || !rightOK {
			continue
		}
		sum := new(big.Int).Add(left, right)
		if sum.Cmp(limSaturated) >= 0 {
			sum.Set(limSaturated)
		}
		difference := new(big.Int).Sub(left, right)
		if difference.Sign() < 0 {
			difference.SetInt64(0)
		}
		for op, want := range map[string]string{
			"add": limCanonical(sum), "sub": limCanonical(difference), "cmp": strconv.Itoa(left.Cmp(right)),
		} {
			if got := run(op, pair[0], pair[1]); len(got) != 1 || got[0] != want {
				t.Errorf("%s(%q, %q) = %q, want %q", op, pair[0], pair[1], got, want)
			}
		}
		if t.Failed() {
			return
		}
	}
}

// A carry out of the twelfth fractional digit, and the borrow that undoes it.
