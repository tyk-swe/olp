package retrievalbinding

import (
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/retrieval"
)

const (
	maxBindings  = 512
	maxKeyGrants = 2000
)

type Binding struct {
	Project        string
	Name           string
	Route          string
	TargetProvider string
	Backend        string
	Resource       string
	Fields         retrieval.Fields
	KeyGrants      []string
	Enabled        bool
}

type Desired struct {
	Bindings []Binding
	stages   map[string]stage
}

func Parse(document contract.ConfigurationDocument, names promotion.Names) (Desired, error) {
	entries := promotion.ValueOf(document.RetrievalBindings)
	if len(entries) > maxBindings {
		return Desired{}, access.Invalid("retrieval_bindings", "Declare at most 512 retrieval bindings.")
	}
	first := promotion.FirstIndex(entries, entryKey)
	bindings, err := promotion.TryMapIndex(entries, func(i int, entry contract.ConfigurationRetrievalBinding) (Binding, error) {
		return parseEntry(entry, first[entryKey(entry)] != i)
	})
	if err != nil {
		return Desired{}, err
	}
	sorted := slices.SortedFunc(slices.Values(bindings), func(a, b Binding) int { return cmp.Compare(key(a), key(b)) })
	return Desired{
		Bindings: sorted,
		stages: maps.Collect(func(yield func(string, stage) bool) {
			for _, binding := range sorted {
				if !yield(key(binding), stageOf(document, names, binding)) {
					return
				}
			}
		}),
	}, nil
}

func parseEntry(entry contract.ConfigurationRetrievalBinding, duplicate bool) (Binding, error) {
	binding := Binding{
		Project:        strings.TrimSpace(entry.Project),
		Name:           strings.TrimSpace(entry.Name),
		Route:          entry.Route,
		TargetProvider: entry.TargetProvider,
		Backend:        string(entry.Backend),
		Resource:       entry.BackendResource,
		Fields:         fieldsOf(entry.Fields),
		KeyGrants:      append([]string{}, slices.Compact(slices.Sorted(slices.Values(entry.KeyGrants)))...),
		Enabled:        entry.Enabled,
	}
	if !promotion.ValidLabel(binding.Project) || !promotion.ValidLabel(binding.Name) || !promotion.ValidLabel(binding.TargetProvider) || binding.Route == "" || len(binding.Route) > 128 || duplicate || !retrieval.ResourceValid(binding.Backend, binding.Resource) {
		return Binding{}, invalidIdentity()
	}
	if err := retrieval.ValidateFields(binding.Backend, binding.Fields); err != nil {
		return Binding{}, access.Invalid("retrieval_bindings.fields", err.Error())
	}
	if len(entry.KeyGrants) > maxKeyGrants {
		return Binding{}, access.Invalid("retrieval_bindings.key_grants", "Use at most 2000 explicit key names.")
	}
	if !slices.ContainsFunc(binding.KeyGrants, func(name string) bool { return !promotion.ValidLabel(name) }) {
		return binding, nil
	}
	return Binding{}, access.Invalid("retrieval_bindings.key_grants", "Use nonempty exact project-local API-key names.")
}

func Render(bindings []Binding) *[]contract.ConfigurationRetrievalBinding {
	return promotion.Optional(promotion.Map(slices.SortedFunc(slices.Values(bindings), func(a, b Binding) int { return cmp.Compare(key(a), key(b)) }), renderBinding))
}

func renderBinding(binding Binding) contract.ConfigurationRetrievalBinding {
	return contract.ConfigurationRetrievalBinding{
		Project:         binding.Project,
		Name:            binding.Name,
		Route:           binding.Route,
		TargetProvider:  binding.TargetProvider,
		Backend:         contract.ConfigurationRetrievalBindingBackend(binding.Backend),
		BackendResource: binding.Resource,
		Fields: contract.RetrievalBindingFields{
			Text:     promotion.NonZero(binding.Fields.Text),
			Id:       promotion.NonZero(binding.Fields.ID),
			Title:    promotion.NonZero(binding.Fields.Title),
			Url:      promotion.NonZero(binding.Fields.URL),
			Search:   promotion.Optional(binding.Fields.Search),
			Metadata: promotion.OptionalMap(binding.Fields.Metadata),
		},
		KeyGrants: append([]string{}, slices.Sorted(slices.Values(binding.KeyGrants))...),
		Enabled:   binding.Enabled,
	}
}

func fieldsOf(fields contract.RetrievalBindingFields) retrieval.Fields {
	return retrieval.Fields{
		Text:     promotion.ValueOf(fields.Text),
		ID:       promotion.ValueOf(fields.Id),
		Title:    promotion.ValueOf(fields.Title),
		URL:      promotion.ValueOf(fields.Url),
		Search:   promotion.ValueOf(fields.Search),
		Metadata: promotion.ValueOf(fields.Metadata),
	}
}

func key(binding Binding) string {
	return strings.ToLower(binding.Project) + "\x00" + strings.ToLower(binding.Name)
}

func entryKey(entry contract.ConfigurationRetrievalBinding) string {
	return key(Binding{Project: strings.TrimSpace(entry.Project), Name: strings.TrimSpace(entry.Name)})
}

func invalidIdentity() error {
	return access.Invalid("retrieval_bindings", "Use unique project/name pairs with an explicit route, provider and native backend resource.")
}
