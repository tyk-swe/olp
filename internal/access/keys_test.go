package access

import (
	"errors"
	"strings"
	"testing"
)

func TestKeyBudgetFormats(t *testing.T) {
	for _, field := range []string{"daily_cost_limit", "monthly_cost_limit"} {
		for _, tc := range []struct {
			value string
			valid bool
		}{
			{"1", true},
			{"01.50", true},
			{"08", true},
			{"1.000000000001", true},
			{"0.000000000001", true},
			{"000000000001.000000000001", true},
			{"999999999999.999999999999", true},
			{" \t01.50\n", true},
			{"", false},
			{" ", false},
			{"0", false},
			{"00.000000000000", false},
			{"-1", false},
			{"+1.00", false},
			{".1", false},
			{"1.", false},
			{"1.2.3", false},
			{"1e2", false},
			{"1/2", false},
			{"0x10", false},
			{"1 0", false},
			{"١", false},
			{"1000000000000", false},
			{"0000000000001", false},
			{"1.0000000000001", false},
		} {
			t.Run(field+"/"+tc.value, func(t *testing.T) {
				value := tc.value
				input := keyInput{Name: "budget", KeyPolicy: KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{}}}
				if field == "daily_cost_limit" {
					input.DailyCostLimit = &value
				} else {
					input.MonthlyCostLimit = &value
				}
				err := validateKey(input, false)
				if tc.valid {
					if err != nil {
						t.Fatalf("rejected supported budget %q: %v", tc.value, err)
					}
					if value != strings.TrimSpace(tc.value) {
						t.Fatalf("budget lost precision: got %q, want %q", value, strings.TrimSpace(tc.value))
					}
					return
				}
				var p *Problem
				if !errors.As(err, &p) || p.Status != 422 || p.Field != field {
					t.Fatalf("budget %q: got %v, want a validation error for %s", tc.value, err, field)
				}
			})
		}
	}
}
