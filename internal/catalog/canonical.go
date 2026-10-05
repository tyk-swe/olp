package catalog

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// Encode renders a catalog in canonical form: two-space indented JSON with a
// trailing newline, members in declaration order, every list sorted, times
// in UTC to the second, and no HTML escaping. A signature covers these exact
// bytes, and a catalog edit diffs as exactly the entries it changes.
func Encode(c *Catalog) ([]byte, error) {
	normalized := *c
	normalized.PublishedAt = instant(c.PublishedAt)
	normalized.Estimation = slices.Clone(c.Estimation)
	for i := range normalized.Estimation {
		normalized.Estimation[i].Provenance = provenance(normalized.Estimation[i].Provenance)
	}
	slices.SortFunc(normalized.Estimation, func(a, b Estimation) int { return strings.Compare(a.Family, b.Family) })
	normalized.Vendors = slices.Clone(c.Vendors)
	for i := range normalized.Vendors {
		normalized.Vendors[i].Models = normalizeModels(normalized.Vendors[i].Models)
	}
	slices.SortFunc(normalized.Vendors, func(a, b Vendor) int { return strings.Compare(a.ID, b.ID) })
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(normalized); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Canonical decodes, validates and re-encodes a catalog document.
func Canonical(document []byte) ([]byte, error) {
	c, err := Decode(document)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return Encode(c)
}

func normalizeModels(models []Model) []Model {
	out := slices.Clone(models)
	for i := range out {
		m := &out[i]
		m.Aliases = sortedSet(m.Aliases)
		m.InputModalities = sortedSet(m.InputModalities)
		m.OutputModalities = sortedSet(m.OutputModalities)
		if m.SupportedParameters != nil {
			parameters := sortedSet(*m.SupportedParameters)
			if parameters == nil {
				parameters = []string{}
			}
			m.SupportedParameters = &parameters
		}
		if m.Lifecycle != nil {
			lifecycle := *m.Lifecycle
			lifecycle.Provenance = provenance(lifecycle.Provenance)
			m.Lifecycle = &lifecycle
		}
		m.Prices = slices.Clone(m.Prices)
		for j := range m.Prices {
			p := &m.Prices[j]
			p.Unrepresentable = slices.Clone(p.Unrepresentable)
			if p.Unrepresentable == nil {
				p.Unrepresentable = []Unrepresentable{}
			}
			slices.SortFunc(p.Unrepresentable, func(a, b Unrepresentable) int { return strings.Compare(a.Component, b.Component) })
			p.Provenance = provenance(p.Provenance)
		}
		slices.SortFunc(m.Prices, func(a, b Price) int { return strings.Compare(a.Operation, b.Operation) })
		m.Provenance = provenance(m.Provenance)
	}
	slices.SortFunc(out, func(a, b Model) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func sortedSet(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := slices.Clone(values)
	slices.Sort(out)
	return slices.Compact(out)
}

func provenance(p Provenance) Provenance {
	p.ObservedAt = instant(p.ObservedAt)
	return p
}

func instant(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }
