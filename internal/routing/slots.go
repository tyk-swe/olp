package routing

import (
	"cmp"
	"slices"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
)

// SelectSlots is shared by previews and execution. Live revocations, cooldowns
// and capacity are checked by the executor immediately before dispatch.
func SelectSlots(provider Provider, model string, route Route, keyID, operation, surface, mode string, affinity []byte) []Slot {
	routeID, err := uuid.Parse(route.RoutingID)
	if err != nil {
		return nil
	}
	type ranked struct {
		index    int
		priority int
		score    float64
	}
	secretRequired := connectors.SecretRequired(provider.Connector().AuthMode)
	rows := make([]ranked, 0, len(provider.Slots))
	for i := range provider.Slots {
		slot := &provider.Slots[i]
		if !slot.Allows(model, route.Slug, keyID) || secretRequired && slot.CredentialID == nil {
			continue
		}
		id, err := uuid.Parse(slot.ID)
		if err != nil {
			continue
		}
		rows = append(rows, ranked{i, slot.Priority, Score(routeID, id, slot.Weight, operation, surface, mode, affinity)})
	}
	slices.SortStableFunc(rows, func(a, b ranked) int {
		if order := cmp.Compare(a.priority, b.priority); order != 0 {
			return order
		}
		return cmp.Compare(b.score, a.score)
	})
	out := make([]Slot, len(rows))
	for i, row := range rows {
		out[i] = provider.Slots[row.index]
	}
	return out
}
