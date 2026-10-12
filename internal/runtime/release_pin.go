package runtime

import (
	"encoding/json"
	"errors"
)

// ReleasePin is the historical serving material a durable media job needs.
// Reconstructing it does not establish current authorization or eligibility;
// the feature that owns the job performs those checks before external effects.
type ReleasePin struct {
	Provider Provider
	Route    Route
	Slot     Slot
}

var ErrReleasePin = errors.New("historical serving pin is unavailable")

// ReleasePinIdentity records the admitted target, rather than merely naming
// unrelated entries that happen to be present in one release.
type ReleasePinIdentity struct {
	ProviderID, ProviderRevisionID, RouteSlug, Model, SlotID string
	CredentialID                                             *string
}

// PinnedSlot requires the exact selected slot/version pair. A credential on a
// sibling slot or the provider's default credential cannot replace that pair.
func PinnedSlot(provider *Provider, slotID string, credentialID *string) (Slot, bool) {
	if provider == nil || slotID == "" {
		return Slot{}, false
	}
	for _, slot := range provider.Slots {
		if slot.ID != slotID {
			continue
		}
		if (slot.CredentialID == nil) != (credentialID == nil) || slot.CredentialID != nil && *slot.CredentialID != *credentialID {
			return Slot{}, false
		}
		return slot, true
	}
	return Slot{}, false
}

// DecodeReleasePin owns the serialized release representation. It decodes only
// the selected entries, without compiling today's new-work contracts against
// historical configuration. Reconciliation and reservation share this identity
// check; their current-authority policies remain feature-owned and different.
func DecodeReleasePin(source []byte, identity ReleasePinIdentity) (ReleasePin, error) {
	if identity.ProviderID == "" || identity.ProviderRevisionID == "" || identity.RouteSlug == "" || identity.Model == "" || identity.SlotID == "" {
		return ReleasePin{}, ErrReleasePin
	}
	var stored struct {
		Providers map[string]json.RawMessage `json:"providers"`
		Routes    map[string]json.RawMessage `json:"routes"`
	}
	if json.Unmarshal(source, &stored) != nil {
		return ReleasePin{}, ErrReleasePin
	}
	var pin ReleasePin
	if json.Unmarshal(stored.Providers[identity.ProviderID], &pin.Provider) != nil || pin.Provider.ID != identity.ProviderID || pin.Provider.RevisionID != identity.ProviderRevisionID {
		return ReleasePin{}, ErrReleasePin
	}
	if json.Unmarshal(stored.Routes[identity.RouteSlug], &pin.Route) != nil || pin.Route.Slug != identity.RouteSlug || pin.Route.RevisionID == "" {
		return ReleasePin{}, ErrReleasePin
	}
	matched := false
	for _, target := range pin.Route.Targets {
		if target.ProviderID == identity.ProviderID && target.ProviderModel == identity.Model {
			matched = true
			break
		}
	}
	if !matched {
		return ReleasePin{}, ErrReleasePin
	}
	var ok bool
	pin.Slot, ok = PinnedSlot(&pin.Provider, identity.SlotID, identity.CredentialID)
	if !ok {
		return ReleasePin{}, ErrReleasePin
	}
	return pin, nil
}
