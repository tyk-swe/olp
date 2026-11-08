package provider

import (
	"cmp"
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/secrets"
)

const (
	MaxRef       = 200
	maxProviders = 500
	maxModels    = 10000
)

type Deps struct {
	Egress       *egress.Policy
	Unconfined   *plugins.Unconfined
	Keys         *secrets.KeyRing
	Installation string
	Catalogue    Catalogue
}

type Catalogue interface {
	StoreCredential(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, int, error)
	StoreNetworkCredential(ctx context.Context, tx pgx.Tx, providerID, secret string) (string, error)
	CredentialEquals(ctx context.Context, q access.Queryer, credentialID, secret string) (bool, error)
}

type Entry struct {
	Name            string
	Project         *string
	Configuration   providers.Configuration
	Models          []providers.DraftModel
	Slots           []Slot
	Network         bool
	Billing         *Billing
	projectDeclared bool
}

func Parse(entry contract.ConfigurationProviderEntry, names promotion.Names, deps Deps) (Entry, error) {
	parsed, err := parse(entry, "", names, deps.Egress)
	if err != nil {
		return Entry{}, err
	}
	return parsed, distinctRefs([]Entry{parsed}, func(int) path { return "" })
}

func ParseAll(entries []contract.ConfigurationProviderEntry, names promotion.Names, deps Deps) ([]Entry, error) {
	if len(entries) > maxProviders {
		return nil, access.Invalid("providers", "Declare at most "+strconv.Itoa(maxProviders)+" providers.")
	}
	first := firstIndex(entries, func(entry contract.ConfigurationProviderEntry) string { return strings.ToLower(entry.Name) })
	parsed, err := each(entries, func(i int, entry contract.ConfigurationProviderEntry) (Entry, error) {
		at := path("providers").index(i)
		if first[strings.ToLower(entry.Name)] != i {
			return Entry{}, access.Invalid(at.field("name"), "Provider names must be unique.")
		}
		return parse(entry, at, names, deps.Egress)
	})
	if err != nil {
		return nil, err
	}
	if sum(parsed, func(entry Entry) int { return len(entry.Models) }) > maxModels {
		return nil, access.Invalid("providers.models", "Declare at most "+strconv.Itoa(maxModels)+" models in total.")
	}
	return parsed, distinctRefs(parsed, path("providers").index)
}

func parse(entry contract.ConfigurationProviderEntry, at path, names promotion.Names, policy *egress.Policy) (Entry, error) {
	if err := access.ValidText(at.field("name"), entry.Name, 100); err != nil {
		return Entry{}, err
	}
	project, declared := promotion.Present(entry.Project)
	if err := validProject(project, declared, at); err != nil {
		return Entry{}, err
	}
	configuration, err := portableConfiguration(entry.Configuration, at)
	if err != nil {
		return Entry{}, err
	}
	network, err := parseNetwork(entry, configuration, at)
	if err != nil {
		return Entry{}, err
	}
	billing, err := parseBilling(entry, configuration.Kind, at)
	if err != nil {
		return Entry{}, err
	}
	validated, err := validated(configuration, policy)
	if err != nil {
		return Entry{}, err
	}
	models, err := parseModels(entry.Models, at)
	if err != nil {
		return Entry{}, err
	}
	slots, err := parseSlots(entry, validated, at)
	if err != nil {
		return Entry{}, err
	}
	return Entry{
		Name:            entry.Name,
		Project:         optional(project, declared),
		Configuration:   validated,
		Models:          models,
		Slots:           slots,
		Network:         network,
		Billing:         billing,
		projectDeclared: declared && names.Project(project),
	}, nil
}

func validProject(project string, declared bool, at path) error {
	if !declared {
		return nil
	}
	return access.ValidText(at.field("project"), project, 100)
}

func parseModels(models []contract.ConfigurationModelEntry, at path) ([]providers.DraftModel, error) {
	first := firstIndex(models, func(model contract.ConfigurationModelEntry) string { return model.UpstreamModel })
	return each(models, func(j int, model contract.ConfigurationModelEntry) (providers.DraftModel, error) {
		field := at.at("models", j)
		if err := providers.ValidModelName(field.field("upstream_model"), model.UpstreamModel); err != nil {
			return providers.DraftModel{}, err
		}
		if first[model.UpstreamModel] != j {
			return providers.DraftModel{}, access.Invalid(field.field("upstream_model"), "Model names must be unique per provider.")
		}
		display := cmp.Or(model.DisplayName, model.UpstreamModel)
		if err := providers.ValidModelName(field.field("display_name"), display); err != nil {
			return providers.DraftModel{}, err
		}
		capabilities, err := promotion.Convert[[]providers.CapabilityInput](model.Capabilities)
		if err != nil {
			return providers.DraftModel{}, err
		}
		if _, err := providers.ValidCapabilities(capabilities); err != nil {
			return providers.DraftModel{}, err
		}
		return providers.DraftModel{UpstreamModel: model.UpstreamModel, DisplayName: display, Enabled: model.Enabled, Capabilities: orEmpty(capabilities)}, nil
	})
}

type reference struct {
	ref     promotion.Ref
	field   string
	message string
}

func distinctRefs(entries []Entry, root func(int) path) error {
	refs := slices.Concat(mapSlice(entries, func(i int, entry Entry) []reference {
		at := root(i)
		return slices.Concat(
			when(entry.Network, reference{NetworkRef(entry.Name), at.field("network_credential_ref"), "Credential references must be distinct."}),
			when(entry.Billing != nil, reference{BillingRef(entry.Name), at.field("billing_import.credential_ref"), "Credential references must be distinct."}),
			slotReferences(entry, at),
		)
	})...)
	repeated, ok := firstRepeat(refs, func(ref reference) promotion.Ref { return ref.ref })
	if !ok {
		return nil
	}
	return access.Invalid(repeated.field, repeated.message)
}

func Render(entries []Entry) ([]contract.ConfigurationProviderEntry, error) {
	sorted := slices.SortedFunc(slices.Values(entries), func(a, b Entry) int {
		return cmp.Or(cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)), cmp.Compare(a.Name, b.Name))
	})
	rendered, err := each(sorted, func(_ int, entry Entry) (contract.ConfigurationProviderEntry, error) { return render(entry) })
	return orEmpty(rendered), err
}

func render(entry Entry) (contract.ConfigurationProviderEntry, error) {
	normalized := entry.Configuration
	normalized.Normalize()
	configuration, err := promotion.Convert[contract.ProviderConfiguration](normalized)
	if err != nil {
		return contract.ConfigurationProviderEntry{}, err
	}
	models, err := promotion.Convert[[]contract.ConfigurationModelEntry](canonicalModels(entry.Models))
	if err != nil {
		return contract.ConfigurationProviderEntry{}, err
	}
	slots, err := renderSlots(entry)
	if err != nil {
		return contract.ConfigurationProviderEntry{}, err
	}
	billing, err := renderBilling(entry)
	if err != nil {
		return contract.ConfigurationProviderEntry{}, err
	}
	return contract.ConfigurationProviderEntry{
		BillingImport:        billing,
		Configuration:        configuration,
		Models:               orEmpty(models),
		Name:                 entry.Name,
		NetworkCredentialRef: renderNetwork(entry),
		Project:              promotion.Nullable(entry.Project),
		Slots:                slots,
	}, nil
}

func canonicalModels(models []providers.DraftModel) []providers.DraftModel {
	canonical := mapSlice(models, func(_ int, model providers.DraftModel) providers.DraftModel {
		return providers.DraftModel{UpstreamModel: model.UpstreamModel, DisplayName: cmp.Or(model.DisplayName, model.UpstreamModel), Enabled: model.Enabled, Capabilities: canonicalCapabilities(model.Capabilities)}
	})
	return slices.SortedFunc(slices.Values(canonical), func(a, b providers.DraftModel) int { return cmp.Compare(a.UpstreamModel, b.UpstreamModel) })
}

func canonicalCapabilities(capabilities []providers.CapabilityInput) []providers.CapabilityInput {
	return orEmpty(slices.SortedFunc(slices.Values(capabilities), func(a, b providers.CapabilityInput) int {
		return cmp.Compare(a.Operation+"/"+a.Surface+"/"+a.Mode, b.Operation+"/"+b.Surface+"/"+b.Mode)
	}))
}
