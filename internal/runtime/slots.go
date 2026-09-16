package runtime

import (
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"sort"
)

// SelectSlots is shared by previews and execution. Live revocations, cooldowns
// and capacity are checked by the executor immediately before dispatch.
func SelectSlots(provider Provider, model string, route Route, keyID, operation, surface, mode string, affinity []byte) []Slot {
	routeID, err := uuid.Parse(route.RoutingID)
	if err != nil {
		return nil
	}
	type ranked struct {
		slot  Slot
		score float64
	}
	rows := []ranked{}
	for _, slot := range provider.Slots {
		if !slot.Allows(model, route.Slug, keyID) || connectors.SecretRequired(provider.Connector().AuthMode) && slot.CredentialID == nil {
			continue
		}
		id, err := uuid.Parse(slot.ID)
		if err != nil {
			continue
		}
		rows = append(rows, ranked{slot, Score(routeID, id, slot.Weight, operation, surface, mode, affinity)})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].slot.Priority != rows[j].slot.Priority {
			return rows[i].slot.Priority < rows[j].slot.Priority
		}
		return rows[i].score > rows[j].score
	})
	out := make([]Slot, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.slot)
	}
	return out
}
