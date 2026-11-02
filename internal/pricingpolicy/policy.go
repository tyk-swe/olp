package pricingpolicy

import "strings"

const (
	MaxTiers         = 8
	MaxWindowPeriods = 32
)

func NormalizeAmount(value string) (string, bool) {
	trimmed := strings.TrimSpace(value)
	if !ValidAmount(trimmed) {
		return "", false
	}
	return trimmed, true
}

func ValidAmount(value string) bool {
	integer, fraction, hasFraction := strings.Cut(value, ".")
	if len(integer) < 1 || len(integer) > 12 || !decimalDigits(integer) {
		return false
	}
	if hasFraction && (len(fraction) < 1 || len(fraction) > 12 || !decimalDigits(fraction)) {
		return false
	}
	return true
}

func decimalDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func WindowMinutes(value string, allowEndOfDay bool) (int, bool) {
	if len(value) != 5 || value[2] != ':' || value[0] < '0' || value[0] > '9' ||
		value[1] < '0' || value[1] > '9' || value[3] < '0' || value[3] > '9' || value[4] < '0' || value[4] > '9' {
		return 0, false
	}
	hours := int(value[0]-'0')*10 + int(value[1]-'0')
	minutes := int(value[3]-'0')*10 + int(value[4]-'0')
	if allowEndOfDay && hours == 24 && minutes == 0 {
		return 24 * 60, true
	}
	if hours > 23 || minutes > 59 {
		return 0, false
	}
	return hours*60 + minutes, true
}
