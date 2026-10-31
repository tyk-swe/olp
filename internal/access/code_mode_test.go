package access_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
)

func TestCodeConfigurationWritesRequireProjectManagement(t *testing.T) {
	const project = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	principal := access.Principal{Projects: map[string]string{project: "developer"}}
	for name, save := range map[string]func() error{
		"pool": func() error {
			_, err := access.SaveCodePool(t.Context(), nil, principal, project, project, access.CodePoolInput{ProjectID: strings.ToUpper(project), Name: "personal work", Kind: "shared", AccountIDs: []string{}, APIKeyIDs: []string{}}, false)
			return err
		},
		"budget": func() error {
			_, err := access.SaveCodeBudget(t.Context(), nil, principal, project, project, access.CodeBudgetInput{ProjectID: strings.ToUpper(project), DailyTokens: new(int64(100)), Enabled: true}, false)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			var problem *access.Problem
			if err := save(); !errors.As(err, &problem) || problem.Status != 403 {
				t.Fatalf("project member changed coding policy, got %v", err)
			}
		})
	}
}

func TestCodeConfigurationPoolNormalizesAssignments(t *testing.T) {
	const id = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	input, err := (access.CodePoolInput{ProjectID: strings.ToUpper(id), Name: "pool", Kind: "personal", OwnerUserID: new(strings.ToUpper(id)), AccountIDs: []string{strings.ToUpper(id)}, AccountWeights: map[string]int64{id: 3}, APIKeyIDs: []string{strings.ToUpper(id)}}).Validated()
	if err != nil {
		t.Fatal(err)
	}
	if input.ProjectID != id || *input.OwnerUserID != id || input.AccountIDs[0] != id || input.APIKeyIDs[0] != id || input.AccountWeights[id] != 3 {
		t.Fatalf("validated pool changed UUID assignment semantics %+v", input)
	}
}

func TestCodeConfigurationPoolValidatesOwnershipAndWeights(t *testing.T) {
	const id = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	for _, test := range []struct {
		kind  string
		owner *string
		valid bool
	}{
		{"shared", nil, true},
		{"personal", new(id), true},
		{"shared", new(id), false},
		{"personal", nil, false},
		{"unknown", nil, false},
	} {
		t.Run(fmt.Sprintf("%s/owner-%t", test.kind, test.owner != nil), func(t *testing.T) {
			input := access.CodePoolInput{ProjectID: id, Name: "pool", Kind: test.kind, OwnerUserID: test.owner, AccountIDs: []string{}, APIKeyIDs: []string{}}
			if err := input.Validate(); (err == nil) != test.valid {
				t.Fatalf("pool ownership validity %t, error %v", test.valid, err)
			}
		})
	}
	for _, test := range []struct {
		weight int64
		valid  bool
	}{{0, false}, {1, true}, {2147483647, true}, {2147483648, false}} {
		t.Run(fmt.Sprintf("weight-%d", test.weight), func(t *testing.T) {
			input := access.CodePoolInput{ProjectID: id, Name: "pool", Kind: "shared", AccountIDs: []string{id}, APIKeyIDs: []string{}, AccountWeights: map[string]int64{id: test.weight}}
			validated, err := input.Validated()
			if (err == nil) != test.valid || test.valid && validated.AccountWeights[id] != test.weight {
				t.Fatalf("pool weight validity %t or value changed, got %+v, error %v", test.valid, validated, err)
			}
		})
	}
}

func TestCodeConfigurationBudgetRequiresPositiveSafeTokenCaps(t *testing.T) {
	const id = "0192cf87-d4ab-7f2e-a8b1-c2d3e4f50601"
	if err := (access.CodeBudgetInput{ProjectID: id}).Validate(); err == nil {
		t.Fatal("budget without a token cap accepted")
	}
	for _, test := range []struct {
		tokens int64
		valid  bool
	}{{-1, false}, {0, false}, {1, true}, {1<<53 - 1, true}, {1 << 53, false}} {
		for _, daily := range []bool{false, true} {
			t.Run(fmt.Sprintf("tokens-%d/daily-%t", test.tokens, daily), func(t *testing.T) {
				input := func() access.CodeBudgetInput {
					if daily {
						return access.CodeBudgetInput{ProjectID: id, DailyTokens: new(test.tokens)}
					}
					return access.CodeBudgetInput{ProjectID: id, MonthlyTokens: new(test.tokens)}
				}()
				validated, err := input.Validated()
				if (err == nil) != test.valid {
					t.Fatalf("token cap validity %t, error %v", test.valid, err)
				}
				if test.valid && (daily && (validated.DailyTokens == nil || *validated.DailyTokens != test.tokens) || !daily && (validated.MonthlyTokens == nil || *validated.MonthlyTokens != test.tokens)) {
					t.Fatalf("token cap changed during validation %+v", validated)
				}
			})
		}
	}
}
