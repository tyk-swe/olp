package configuration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/runtime"
)

const APIVersion = "openllmproxy.dev/config/v1"

type Document struct {
	APIVersion string          `json:"api_version"`
	ExportedAt string          `json:"exported_at"`
	Projects   []ProjectEntry  `json:"projects"`
	Providers  []ProviderEntry `json:"providers"`
	Routes     []RouteEntry    `json:"routes"`
	Pricing    *PricingEntry   `json:"pricing"`
}

type ProjectEntry struct {
	Name string `json:"name"`
}

type CapabilityEntry struct {
	Operation string `json:"operation"`
	Surface   string `json:"surface"`
	Mode      string `json:"mode"`
}

type ModelEntry struct {
	UpstreamModel string            `json:"upstream_model"`
	DisplayName   string            `json:"display_name"`
	Enabled       bool              `json:"enabled"`
	Capabilities  []CapabilityEntry `json:"capabilities"`
}

type Restrictions struct {
	AllowedAPIKeys []string `json:"allowed_api_keys"`
	AllowedModels  []string `json:"allowed_models"`
	AllowedRoutes  []string `json:"allowed_routes"`
}

type SlotEntry struct {
	Name          string           `json:"name"`
	IsDefault     bool             `json:"is_default"`
	Position      int              `json:"position"`
	Enabled       bool             `json:"enabled"`
	Priority      int              `json:"priority"`
	Weight        int64            `json:"weight"`
	Restrictions  Restrictions     `json:"restrictions"`
	Limits        providers.Limits `json:"limits"`
	CredentialRef *string          `json:"credential_ref"`
}

type ProviderEntry struct {
	Name          string                  `json:"name"`
	Project       *string                 `json:"project"`
	Configuration providers.Configuration `json:"configuration"`
	Models        []ModelEntry            `json:"models"`
	Slots         []SlotEntry             `json:"slots"`
}

type TargetEntry struct {
	Provider      string `json:"provider"`
	ProviderModel string `json:"provider_model"`
	Priority      int    `json:"priority"`
	Weight        int64  `json:"weight"`
	TimeoutMS     int    `json:"timeout_ms"`
}

type RouteEntry struct {
	Slug             string          `json:"slug"`
	Project          *string         `json:"project"`
	Operations       []string        `json:"operations"`
	OverallTimeoutMS int             `json:"overall_timeout_ms"`
	MaxAttempts      int             `json:"max_attempts"`
	Targets          []TargetEntry   `json:"targets"`
	RoutingPolicy    *runtime.Policy `json:"routing_policy"`
	ContentPolicy    json.RawMessage `json:"content_policy"`
	Retired          bool            `json:"retired"`
}

type PriceEntry struct {
	VendorID                    *string `json:"vendor_id"`
	ProviderKind                string  `json:"provider_kind"`
	Provider                    *string `json:"provider"`
	Model                       string  `json:"model"`
	Operation                   string  `json:"operation"`
	InputPerMillion             *string `json:"input_per_million"`
	CachedInputPerMillion       *string `json:"cached_input_per_million"`
	OutputPerMillion            *string `json:"output_per_million"`
	CacheWriteInputPerMillion   *string `json:"cache_write_input_per_million"`
	CacheWrite5MInputPerMillion *string `json:"cache_write_5m_input_per_million"`
	CacheWrite1HInputPerMillion *string `json:"cache_write_1h_input_per_million"`
	UnitPrice                   *string `json:"unit_price"`
	Currency                    string  `json:"currency"`
}

type PricingEntry struct {
	EffectiveAt string       `json:"effective_at"`
	Prices      []PriceEntry `json:"prices"`
}

func canonicalProvider(p *ProviderEntry) {
	p.Configuration.Normalize()
	if p.Models == nil {
		p.Models = []ModelEntry{}
	}
	slices.SortFunc(p.Models, func(a, b ModelEntry) int {
		return strings.Compare(a.UpstreamModel, b.UpstreamModel)
	})
	for j := range p.Models {
		if p.Models[j].DisplayName == "" {
			p.Models[j].DisplayName = p.Models[j].UpstreamModel
		}
		if p.Models[j].Capabilities == nil {
			p.Models[j].Capabilities = []CapabilityEntry{}
		}
		sortCapabilities(p.Models[j].Capabilities)
	}
	if p.Slots == nil {
		p.Slots = []SlotEntry{}
	}
	for j := range p.Slots {
		s := &p.Slots[j]
		s.Restrictions.AllowedAPIKeys = orEmpty(s.Restrictions.AllowedAPIKeys)
		s.Restrictions.AllowedModels = orEmpty(s.Restrictions.AllowedModels)
		s.Restrictions.AllowedRoutes = orEmpty(s.Restrictions.AllowedRoutes)
		for _, list := range [][]string{s.Restrictions.AllowedAPIKeys, s.Restrictions.AllowedModels, s.Restrictions.AllowedRoutes} {
			slices.Sort(list)
		}
	}
	slices.SortFunc(p.Slots, func(a, b SlotEntry) int {
		if a.Position != b.Position {
			return a.Position - b.Position
		}
		return strings.Compare(a.Name, b.Name)
	})
}

func canonicalRoute(r *RouteEntry) {
	if r.Operations == nil {
		r.Operations = []string{}
	}
	slices.Sort(r.Operations)
	if r.Targets == nil {
		r.Targets = []TargetEntry{}
	}
	slices.SortFunc(r.Targets, func(a, b TargetEntry) int {
		if c := strings.Compare(strings.ToLower(a.Provider), strings.ToLower(b.Provider)); c != 0 {
			return c
		}
		if c := strings.Compare(a.ProviderModel, b.ProviderModel); c != 0 {
			return c
		}
		return a.Priority - b.Priority
	})
	if len(r.ContentPolicy) > 0 && !bytes.Equal(bytes.TrimSpace(r.ContentPolicy), []byte("null")) {
		if policy, err := contentpolicy.Decode(r.ContentPolicy); err == nil {
			r.ContentPolicy, _ = json.Marshal(policy)
		}
	} else {
		r.ContentPolicy = nil
	}
}

func CredentialRef(provider, slot string) string { return provider + "/" + slot }

func lower(value *string) string {
	if value == nil {
		return ""
	}
	return strings.ToLower(*value)
}

func sortCapabilities(c []CapabilityEntry) {
	slices.SortFunc(c, func(a, b CapabilityEntry) int {
		return strings.Compare(a.Operation+"/"+a.Surface+"/"+a.Mode, b.Operation+"/"+b.Surface+"/"+b.Mode)
	})
}

func (d *Document) canonicalize() {
	normalizeDocument(d)
	d.ExportedAt = ""
	if d.Projects == nil {
		d.Projects = []ProjectEntry{}
	}
	slices.SortFunc(d.Projects, func(a, b ProjectEntry) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	if d.Providers == nil {
		d.Providers = []ProviderEntry{}
	}
	for i := range d.Providers {
		canonicalProvider(&d.Providers[i])
	}
	slices.SortFunc(d.Providers, func(a, b ProviderEntry) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	if d.Routes == nil {
		d.Routes = []RouteEntry{}
	}
	for i := range d.Routes {
		canonicalRoute(&d.Routes[i])
	}
	slices.SortFunc(d.Routes, func(a, b RouteEntry) int {
		return strings.Compare(a.Slug, b.Slug)
	})
	if d.Pricing != nil {
		if d.Pricing.Prices == nil {
			d.Pricing.Prices = []PriceEntry{}
		}
		for i := range d.Pricing.Prices {
			p := &d.Pricing.Prices[i]
			p.Model = strings.TrimSpace(p.Model)
			p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
			if p.VendorID != nil {
				v := strings.TrimSpace(*p.VendorID)
				p.VendorID = &v
			}
		}
		slices.SortFunc(d.Pricing.Prices, func(a, b PriceEntry) int {
			key := func(p *PriceEntry) string {
				return p.ProviderKind + "\x00" + lower(p.Provider) + "\x00" + lower(p.VendorID) + "\x00" + p.Model + "\x00" + p.Operation
			}
			return strings.Compare(key(&a), key(&b))
		})
	}
}

func Digest(d *Document) (string, error) {
	d.canonicalize()
	data, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
