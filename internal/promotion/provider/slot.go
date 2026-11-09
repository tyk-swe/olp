package provider

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routeidentity"
)

const (
	maxSlots            = 64
	GrantBindingRefused = "A grant backs this credential slot: enroll a grant for it after applying instead of binding a secret."
)

type Slot struct {
	Name          string
	Default       bool
	Position      int
	Enabled       bool
	Priority      int
	Weight        int64
	AllowedModels []string
	AllowedRoutes []string
	Limits        providers.Limits
	Credential    SlotCredential
}

type SlotCredential interface{ slotCredential() }

type Unbound struct{}

type Bound struct{}

type Grant struct{}

func (Unbound) slotCredential() {}

func (Bound) slotCredential() {}

func (Grant) slotCredential() {}

func SlotRef(provider, slot string) promotion.Ref { return promotion.Ref(provider + "/" + slot) }

func referenced(slot Slot) bool {
	_, unbound := slot.Credential.(Unbound)
	return !unbound
}

type slotScope struct {
	provider      string
	configuration providers.Configuration
	at            path
	names         map[string]int
	positions     map[int32]int
}

func parseSlots(entry contract.ConfigurationProviderEntry, configuration providers.Configuration, at path) ([]Slot, error) {
	if len(entry.Slots) < 1 || len(entry.Slots) > maxSlots {
		return nil, access.Invalid(at.field("slots"), "Declare 1–"+strconv.Itoa(maxSlots)+" credential slots per provider.")
	}
	scope := slotScope{
		provider:      entry.Name,
		configuration: configuration,
		at:            at,
		names:         promotion.FirstIndex(entry.Slots, func(slot contract.ConfigurationSlotEntry) string { return slot.Name }),
		positions:     promotion.FirstIndex(entry.Slots, func(slot contract.ConfigurationSlotEntry) int32 { return slot.Position }),
	}
	slots, err := promotion.TryMapIndex(entry.Slots, scope.parse)
	if err != nil {
		return nil, err
	}
	if len(slices.DeleteFunc(slices.Clone(slots), func(slot Slot) bool { return !slot.Default })) != 1 {
		return nil, access.Invalid(at.field("slots"), "Declare exactly one default slot per provider.")
	}
	return slots, credentialRequirement(slots, configuration, at)
}

func (s slotScope) parse(j int, slot contract.ConfigurationSlotEntry) (Slot, error) {
	field := s.at.at("slots", j)
	if err := s.validate(j, slot, field); err != nil {
		return Slot{}, err
	}
	limits, err := promotion.Convert[providers.Limits](slot.Limits)
	if err != nil {
		return Slot{}, err
	}
	if err := providers.ValidLimits(field.field("limits"), limits); err != nil {
		return Slot{}, err
	}
	credential, err := slotCredential(s.provider, slot, s.configuration.Grant(), field.field("credential_ref"))
	if err != nil {
		return Slot{}, err
	}
	return Slot{
		Name:          slot.Name,
		Default:       slot.IsDefault,
		Position:      int(slot.Position),
		Enabled:       slot.Enabled,
		Priority:      int(slot.Priority),
		Weight:        int64(slot.Weight),
		AllowedModels: promotion.NonNil(slot.Restrictions.AllowedModels),
		AllowedRoutes: promotion.NonNil(slot.Restrictions.AllowedRoutes),
		Limits:        limits,
		Credential:    credential,
	}, nil
}

func (s slotScope) validate(j int, slot contract.ConfigurationSlotEntry, field path) error {
	if err := access.ValidText(field.field("name"), slot.Name, 100); err != nil {
		return err
	}
	if s.names[slot.Name] != j {
		return access.Invalid(field.field("name"), "Slot names must be unique per provider.")
	}
	if slot.Position < 0 || slot.Position > 32767 || s.positions[slot.Position] != j {
		return access.Invalid(field.field("position"), "Use unique positions from 0 to 32767.")
	}
	if slot.Priority < 0 || slot.Priority > 32767 {
		return access.Invalid(field.field("priority"), "Use a priority from 0 to 32767.")
	}
	if slot.Weight < 1 || slot.Weight > 1000000 {
		return access.Invalid(field.field("weight"), "Use a weight from 1 to 1000000.")
	}
	return validRestrictions(slot.Restrictions, field)
}

func validRestrictions(restrictions contract.ConfigurationSlotRestrictions, field path) error {
	if len(restrictions.AllowedApiKeys) > 0 {
		return access.Fail(http.StatusUnprocessableEntity, "non_portable_reference", "allowed_api_keys cannot be promoted; re-establish them on the destination after creating keys.")
	}
	if len(restrictions.AllowedRoutes) > 100 || len(restrictions.AllowedModels) > 2000 {
		return access.Invalid(string(field), "Use at most 100 routes and 2000 models per slot.")
	}
	if slices.ContainsFunc(restrictions.AllowedRoutes, func(route string) bool { return !routeidentity.ValidSlug(route) }) {
		return access.Invalid(field.field("allowed_routes"), "Use route slugs.")
	}
	for _, model := range restrictions.AllowedModels {
		if err := providers.ValidModelName(field.field("allowed_models"), model); err != nil {
			return err
		}
	}
	return nil
}

func slotCredential(provider string, slot contract.ConfigurationSlotEntry, grant bool, field string) (SlotCredential, error) {
	want := SlotRef(provider, slot.Name)
	ref, bound := promotion.Present(slot.CredentialRef)
	switch {
	case !bound && grant:
		return nil, access.Invalid(field, "A grant backs each of this provider's credential slots: reference its credential as "+string(want)+".")
	case !bound:
		return Unbound{}, nil
	case promotion.Ref(ref) != want:
		return nil, access.Invalid(field, "Credential references must read "+string(want)+".")
	case len(ref) > maxRef:
		return nil, access.Invalid(field, "Credential references must fit within "+strconv.Itoa(maxRef)+" bytes.")
	case grant:
		return Grant{}, nil
	}
	return Bound{}, nil
}

func credentialRequirement(slots []Slot, configuration providers.Configuration, at path) error {
	hasCredential := slices.ContainsFunc(slots, referenced)
	required := configuration.CredentialRequired()
	if required && !hasCredential {
		return access.Invalid(at.field("slots"), "This authentication mode requires at least one credential slot.")
	}
	if !required && hasCredential {
		return access.Invalid(at.field("slots.credential_ref"), "This authentication mode takes no stored credential.")
	}
	return nil
}

func slotReferences(entry Entry, at path) []reference {
	return slices.Concat(promotion.MapIndex(entry.Slots, func(j int, slot Slot) []reference {
		return promotion.When(referenced(slot), reference{SlotRef(entry.Name, slot.Name), at.at("slots", j).field("credential_ref"), "Credential references must be unique."})
	})...)
}

func renderSlots(entry Entry) ([]contract.ConfigurationSlotEntry, error) {
	sorted := slices.SortedFunc(slices.Values(entry.Slots), func(a, b Slot) int {
		return cmp.Or(cmp.Compare(a.Position, b.Position), cmp.Compare(a.Name, b.Name))
	})
	return promotion.TryMap(sorted, func(slot Slot) (contract.ConfigurationSlotEntry, error) {
		limits, err := promotion.Convert[contract.ConfigurationSlotLimits](slot.Limits)
		if err != nil {
			return contract.ConfigurationSlotEntry{}, err
		}
		return contract.ConfigurationSlotEntry{
			CredentialRef: promotion.Nullable(promotion.PointerIf(string(SlotRef(entry.Name, slot.Name)), referenced(slot))),
			Enabled:       slot.Enabled,
			IsDefault:     slot.Default,
			Limits:        limits,
			Name:          slot.Name,
			Position:      int32(slot.Position),
			Priority:      int32(slot.Priority),
			Restrictions: contract.ConfigurationSlotRestrictions{
				AllowedApiKeys: []string{},
				AllowedModels:  promotion.NonNil(slices.Sorted(slices.Values(slot.AllowedModels))),
				AllowedRoutes:  promotion.NonNil(slices.Sorted(slices.Values(slot.AllowedRoutes))),
			},
			Weight: int32(slot.Weight),
		}, nil
	})
}

func slotClaims(entry Entry) []promotion.Claim {
	return slices.Concat(promotion.Map(entry.Slots, func(slot Slot) []promotion.Claim {
		ref := SlotRef(entry.Name, slot.Name)
		switch slot.Credential.(type) {
		case Bound:
			return []promotion.Claim{{Ref: ref, Accept: acceptSlot(ref)}}
		case Grant:
			return []promotion.Claim{{Ref: ref, Accept: func(promotion.Credential) error {
				return access.Invalid("secret_bindings."+string(ref), GrantBindingRefused)
			}}}
		}
		return nil
	})...)
}

func acceptSlot(ref promotion.Ref) func(promotion.Credential) error {
	return func(credential promotion.Credential) error {
		switch bound := credential.(type) {
		case promotion.External:
			return promotion.ValidateExternal(bound.Reference)
		case promotion.Sealed:
			return validSecret(ref, bound.Secret)
		}
		return nil
	}
}

func validSecret(ref promotion.Ref, secret string) error {
	if providers.ValidCredential(secret) != nil {
		return access.Invalid("secret_bindings."+string(ref), "Use a credential of 1–65536 bytes.")
	}
	if len(secret) > promotion.MaxSecret {
		return access.Invalid("secret_bindings."+string(ref), "Secrets must fit within 64 KiB.")
	}
	return nil
}
