package pool

import (
	"maps"
	"slices"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/code"
)

type Input struct {
	ProjectID      string           `json:"project_id"`
	Name           string           `json:"name"`
	Kind           code.PoolKind    `json:"kind"`
	OwnerUserID    *string          `json:"owner_user_id"`
	AccountIDs     []string         `json:"account_ids"`
	AccountWeights map[string]int64 `json:"account_weights"`
	APIKeyIDs      []string         `json:"api_key_ids"`
}

type Draft struct {
	projectID      string
	name           string
	ownership      code.PoolOwnership
	accountIDs     []string
	accountWeights map[string]int64
	apiKeyIDs      []string
}

func ParseDraft(in Input) (Draft, error) {
	if in.AccountIDs == nil || in.APIKeyIDs == nil || len(in.AccountIDs) > 100 || len(in.APIKeyIDs) > 100 {
		return Draft{}, access.Invalid("assignments", "Use at most 100 accounts and keys.")
	}
	weightKeys := slices.Sorted(maps.Keys(in.AccountWeights))
	ids, err := access.ParseUUIDs(slices.Concat([]string{in.ProjectID}, in.AccountIDs, in.APIKeyIDs, weightKeys))
	if err != nil {
		return Draft{}, err
	}
	owner, err := access.ParseOptionalUUID(in.OwnerUserID)
	if err != nil {
		return Draft{}, err
	}
	accountEnd := 1 + len(in.AccountIDs)
	keyEnd := accountEnd + len(in.APIKeyIDs)
	canonicalWeights := ids[keyEnd:]
	if len(slices.Compact(slices.Sorted(slices.Values(canonicalWeights)))) != len(weightKeys) {
		return Draft{}, access.Invalid("account_weights", "Assign each account weight once.")
	}
	weights := maps.Collect(func(yield func(string, int64) bool) {
		for index, key := range weightKeys {
			if !yield(canonicalWeights[index], in.AccountWeights[key]) {
				return
			}
		}
	})
	if err := access.ValidText("name", in.Name, 100); err != nil {
		return Draft{}, err
	}
	ownership, err := code.ParsePoolOwnership(in.Kind, owner)
	if err != nil {
		return Draft{}, access.Invalid("kind", "Personal pools require an owner; shared pools have none.")
	}
	accountIDs := ids[1:accountEnd]
	for account, weight := range weights {
		if !slices.Contains(accountIDs, account) || weight < 1 || weight > 2147483647 {
			return Draft{}, access.Invalid("account_weights", "Weights must be between 1 and 2147483647 and belong to assigned accounts.")
		}
	}
	return Draft{
		projectID:      ids[0],
		name:           in.Name,
		ownership:      ownership,
		accountIDs:     accountIDs,
		accountWeights: weights,
		apiKeyIDs:      ids[accountEnd:keyEnd],
	}, nil
}

func (in Input) Validate() error {
	_, err := ParseDraft(in)
	return err
}

func (d Draft) ProjectID() string    { return d.projectID }
func (d Draft) AccountIDs() []string { return slices.Clone(d.accountIDs) }
