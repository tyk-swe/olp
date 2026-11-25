package apikeys

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/keyauthority"
)

func TestKeyValidationNormalizesCostLimits(t *testing.T) {
	keyAmount, routeAmount, userAmount := " 1.25 ", " 2.50 ", " 3.75 "
	source := "header"
	input := keyInput{Name: "key", KeyPolicy: keyauthority.KeyPolicy{
		Scopes: []string{"inference"}, AllowedRoutes: []string{}, DailyCostLimit: &keyAmount,
		RouteLimits:   keyauthority.RouteLimits{"route": {MonthlyCostLimit: &routeAmount}},
		EndUserSource: &source,
		EndUserPolicy: &keyauthority.EndUserPolicy{Defaults: keyauthority.AdmissionLimits{WeeklyCostLimit: &userAmount}},
	}}
	if err := validateAndNormalizeKey(input, false); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, got, want string
	}{
		{"key", *input.DailyCostLimit, "1.25"},
		{"route", *input.RouteLimits["route"].MonthlyCostLimit, "2.50"},
		{"end user", *input.EndUserPolicy.Defaults.WeeklyCostLimit, "3.75"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s cost limit = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestKeyRejectsInvalidEndUserLimits(t *testing.T) {
	invalid := "invalid"
	source := "header"
	digest := strings.Repeat("a", 64)
	for _, tc := range []struct {
		name   string
		policy keyauthority.EndUserPolicy
	}{
		{name: "defaults", policy: keyauthority.EndUserPolicy{Defaults: keyauthority.AdmissionLimits{DailyCostLimit: &invalid}}},
		{name: "override digest", policy: keyauthority.EndUserPolicy{Overrides: map[string]keyauthority.AdmissionLimits{"invalid": {}}}},
		{name: "override limit", policy: keyauthority.EndUserPolicy{Overrides: map[string]keyauthority.AdmissionLimits{digest: {WeeklyCostLimit: &invalid}}}},
		{name: "blocked digest", policy: keyauthority.EndUserPolicy{Blocked: []string{"invalid"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := keyInput{Name: "key", KeyPolicy: keyauthority.KeyPolicy{
				Scopes: []string{"inference"}, AllowedRoutes: []string{},
				EndUserSource: &source, EndUserPolicy: &tc.policy,
			}}
			if err := validateAndNormalizeKey(input, false); err == nil {
				t.Fatal("invalid end-user policy was accepted")
			}
		})
	}
}
