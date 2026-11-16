package access

import "testing"

func TestValidateBudgetTimeZone(t *testing.T) {
	t.Parallel()
	for _, zone := range []string{"UTC", "Europe/Berlin"} {
		if err := ValidateBudgetTimeZone(zone); err != nil {
			t.Errorf("%q rejected: %v", zone, err)
		}
	}
	for _, zone := range []string{"", "Local", "/usr/share/zoneinfo/UTC", "../UTC", "Not/AZone"} {
		if err := ValidateBudgetTimeZone(zone); err == nil {
			t.Errorf("%q accepted", zone)
		}
	}
}
