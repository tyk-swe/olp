package promotion

import (
	"cmp"
	"slices"

	"github.com/tyk-swe/olp/internal/management/contract"
)

type Item struct {
	Kind   contract.ConfigurationPlanItemKind
	Key    string
	Action contract.ConfigurationPlanItemAction
	Detail string
}

func Conflict(kind contract.ConfigurationPlanItemKind, key, detail string) Item {
	return Item{Kind: kind, Key: key, Action: contract.ConfigurationPlanItemActionConflict, Detail: detail}
}

func Blocker(kind contract.ConfigurationPlanItemKind, key, detail string) Item {
	return Item{Kind: kind, Key: key, Action: contract.ConfigurationPlanItemActionBlocker, Detail: detail}
}

func Result(digest string, items []Item) contract.ConfigurationPlanResponse {
	sorted := slices.SortedFunc(slices.Values(items), func(a, b Item) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Key, b.Key), cmp.Compare(a.Action, b.Action), cmp.Compare(a.Detail, b.Detail))
	})
	return contract.ConfigurationPlanResponse{
		Digest: digest,
		Actions: collect(sorted, func(action contract.ConfigurationPlanItemAction) bool {
			return action != contract.ConfigurationPlanItemActionConflict && action != contract.ConfigurationPlanItemActionBlocker
		}),
		Conflicts: collect(sorted, func(action contract.ConfigurationPlanItemAction) bool {
			return action == contract.ConfigurationPlanItemActionConflict
		}),
		Blockers: collect(sorted, func(action contract.ConfigurationPlanItemAction) bool {
			return action == contract.ConfigurationPlanItemActionBlocker
		}),
	}
}

func collect(items []Item, keep func(contract.ConfigurationPlanItemAction) bool) []contract.ConfigurationPlanItem {
	return Map(Filter(items, func(item Item) bool { return keep(item.Action) }), func(item Item) contract.ConfigurationPlanItem {
		return contract.ConfigurationPlanItem{Kind: item.Kind, Key: item.Key, Action: item.Action, Detail: item.Detail}
	})
}
