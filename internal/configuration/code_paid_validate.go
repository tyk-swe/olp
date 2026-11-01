package configuration

import (
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
)

func codePaidCredentialRefs(code *CodeConfiguration) []string {
	return slices.Collect(func(yield func(string) bool) {
		for _, route := range code.Routes {
			for _, target := range route.Draft.allPaidTargets() {
				if !yield(target.CredentialRef) {
					return
				}
			}
			if route.Published == nil {
				continue
			}
			for _, target := range route.Published.Policy.allPaidTargets() {
				if !yield(target.CredentialRef) {
					return
				}
			}
			for _, connection := range route.Published.PaidConnections {
				for _, slot := range connection.Provider.Slots {
					if slot.CredentialRef != nil && !yield(*slot.CredentialRef) {
						return
					}
				}
			}
		}
	})
}
func codePaidKeyRefs(code *CodeConfiguration) []string {
	return slices.Collect(func(yield func(string) bool) {
		for _, route := range code.Routes {
			if route.Published == nil {
				continue
			}
			for _, connection := range route.Published.PaidConnections {
				for _, refs := range connection.SlotKeyRefs {
					for _, ref := range refs {
						if !yield(ref) {
							return
						}
					}
				}
			}
		}
	})
}
func validateCodePaidPublication(route CodeRouteEntry) error {
	if route.Published == nil {
		return nil
	}
	publication := route.Published
	names := slices.Collect(maps.Keys(publication.PaidConnections))
	for name, connection := range publication.PaidConnections {
		entry := connection.Provider
		if name != entry.Name || entry.Project == nil || !strings.EqualFold(*entry.Project, route.Project) || entry.BillingImport != nil || !slices.ContainsFunc(slices.Collect(maps.Values(publication.Policy.allPaidTargets())), func(target CodePaidTarget) bool { return target.Provider == name }) {
			return access.Invalid("code.paid_connections", "Frozen providers must name the published paid targets in the same project.")
		}
		for slot, refs := range connection.SlotKeyRefs {
			if !slices.ContainsFunc(entry.Slots, func(s SlotEntry) bool { return s.Name == slot }) {
				return access.Invalid("code.paid_connections.slot_key_refs", "Key restrictions must name a frozen slot.")
			}
			for _, ref := range refs {
				if access.ValidText("code", ref, 300) != nil {
					return access.Invalid("code.paid_connections.slot_key_refs", "Name explicit destination key references.")
				}
			}
		}
	}
	for _, target := range publication.Policy.allPaidTargets() {
		if !slices.Contains(names, target.Provider) {
			return access.Invalid("code.paid_connections", "Freeze every published paid provider.")
		}
		slots := publication.PaidConnections[target.Provider].Provider.Slots
		if !slices.ContainsFunc(slots, func(slot SlotEntry) bool {
			return slot.Name == target.Slot && slot.CredentialRef != nil && *slot.CredentialRef == target.CredentialRef && slot.Enabled
		}) {
			return access.Invalid("code.paid_connections", "Freeze the enabled target slot with its explicit credential reference.")
		}
	}
	return nil
}
