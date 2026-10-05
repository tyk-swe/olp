package catalog

import (
	"regexp"
	"strconv"
	"time"
)

// Decimal is an exact non-negative decimal string with at most 12 fractional
// digits and no redundant zeros, as prices are stored.
type Decimal string

var decimal = regexp.MustCompile(`^(0|[1-9][0-9]{0,11})(\.[0-9]{0,11}[1-9])?$`)

// Valid reports whether the decimal is in canonical form.
func (d Decimal) Valid() bool { return decimal.MatchString(string(d)) }

// Float64 is the decimal's approximate value.
func (d Decimal) Float64() (float64, error) { return strconv.ParseFloat(string(d), 64) }

// String is the decimal text.
func (d Decimal) String() string { return string(d) }

// Date is a calendar date, YYYY-MM-DD.
type Date string

// Valid reports whether the date names a calendar day.
func (d Date) Valid() bool {
	parsed, err := time.Parse(time.DateOnly, string(d))
	return err == nil && parsed.Format(time.DateOnly) == string(d)
}

// Time is the start of the date in UTC.
func (d Date) Time() time.Time {
	parsed, _ := time.Parse(time.DateOnly, string(d))
	return parsed
}
