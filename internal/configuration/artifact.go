package configuration

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/tyk-swe/olp/internal/cache"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/internal/usage"
)

const APIVersion = "openllmproxy.dev/config/v2"
const LegacyAPIVersion = "openllmproxy.dev/config/v1"

type Document struct {
	GuardrailPolicies    []GuardrailPolicyEntry     `json:"guardrail_policies,omitempty"`
	GuardrailAttachments []GuardrailAttachmentEntry `json:"guardrail_attachments,omitempty"`
	MCPServers           []MCPServerEntry           `json:"mcp_servers,omitempty"`
	Sinks                []SinkEntry                `json:"sinks,omitempty"`
	Guardrails           []GuardrailEntry           `json:"guardrails,omitempty"`
	SAML                 *access.SAMLDefinition     `json:"saml,omitempty"`
	RequireLocalMFA      *bool                      `json:"require_local_mfa,omitempty"`
	SCIMGroupMappings    []SCIMGroupMapping         `json:"scim_group_mappings,omitempty"`
	// Endpoint preflight runs before the mutation lock; these versions detect races.
	preparedWorkloads  map[string]string
	WorkloadIssuers    []WorkloadIssuerEntry `json:"workload_issuers,omitempty"`
	Organizations      []OrganizationEntry   `json:"organizations,omitempty"`
	BudgetTimeZone     *string               `json:"budget_time_zone,omitempty"`
	InstallationBudget *access.BudgetPolicy  `json:"installation_budget,omitempty"`
	APIVersion         string                `json:"api_version"`
	ExportedAt         string                `json:"exported_at"`
	Projects           []ProjectEntry        `json:"projects"`
	Providers          []ProviderEntry       `json:"providers"`
	Routes             []RouteEntry          `json:"routes"`
	Templates          []TemplateEntry       `json:"templates,omitempty"`
	Pricing            *PricingEntry         `json:"pricing"`
	// Discounts are the scopes the document prices under; scopes the document
	// omits are left untouched on apply.
	Discounts []DiscountEntry `json:"discounts,omitempty"`
	// RateCards are each owner's charge terms; cards the document omits are
	// left untouched on apply.
	RateCards []RateCardEntry `json:"rate_cards,omitempty"`
}

type ProjectEntry struct {
	PublicCatalog        bool                      `json:"public_catalog,omitempty"`
	PublicCatalogPrices  bool                      `json:"public_catalog_prices,omitempty"`
	Organization         *string                   `json:"organization,omitempty"`
	AttributionBudgets   access.AttributionBudgets `json:"attribution_budgets,omitempty"`
	LimitTemplates       *access.LimitTemplates    `json:"limit_templates,omitempty"`
	EndUserLimitTemplate *string                   `json:"end_user_limit_template,omitempty"`
	Budget               *access.BudgetPolicy      `json:"budget,omitempty"`
	RouteGroups          access.RouteGroups        `json:"route_groups,omitempty"`
	AttributionPolicy    *attribution.Policy       `json:"attribution_policy,omitempty"`
	Name                 string                    `json:"name"`
	EndUserDefaults      *access.AdmissionLimits   `json:"end_user_defaults,omitempty"`
}

type CapabilityEntry = providers.CapabilityInput

type ModelEntry = providers.DraftModel

type Restrictions = providers.SlotRestrictions

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
	NetworkCredentialRef *string                 `json:"network_credential_ref,omitempty"`
	BillingImport        *BillingImportEntry     `json:"billing_import,omitempty"`
	Name                 string                  `json:"name"`
	Project              *string                 `json:"project"`
	Configuration        providers.Configuration `json:"configuration"`
	Models               []ModelEntry            `json:"models"`
	Slots                []SlotEntry             `json:"slots"`
}

type TargetEntry struct {
	Provider      string          `json:"provider"`
	ProviderModel string          `json:"provider_model"`
	Priority      int             `json:"priority"`
	Weight        int64           `json:"weight"`
	TimeoutMS     int             `json:"timeout_ms"`
	Tags          []string        `json:"tags,omitempty"`
	Shadow        *routing.Shadow `json:"shadow,omitempty"`
}

func targetEntry(t routing.PublishedTarget) TargetEntry {
	return TargetEntry{Provider: t.ProviderName, ProviderModel: t.ProviderModel, Priority: t.Priority, Weight: t.Weight, TimeoutMS: int(t.TimeoutMS), Tags: t.Tags, Shadow: t.Shadow}
}

type RouteEntry struct {
	Cache                *cache.Policy       `json:"cache,omitempty"`
	PromptCache          *cache.PromptPolicy `json:"prompt_cache,omitempty"`
	ExposeUpstreamModels bool                `json:"expose_upstream_models,omitempty"`
	CallerCostExempt     bool                `json:"caller_cost_exempt,omitempty"`
	MaxBodyBytes         *int64              `json:"max_body_bytes,omitempty"`
	Slug                 string              `json:"slug"`
	Project              *string             `json:"project"`
	Operations           []string            `json:"operations"`
	OverallTimeoutMS     int                 `json:"overall_timeout_ms"`
	MaxAttempts          int                 `json:"max_attempts"`
	Targets              []TargetEntry       `json:"targets"`
	RoutingPolicy        *routing.Policy     `json:"routing_policy"`
	ContentPolicy        json.RawMessage     `json:"content_policy,omitempty"`
	Fidelity             json.RawMessage     `json:"fidelity"`
	Fallbacks            []routing.Fallback  `json:"fallbacks,omitempty"`
	Selectors            []routing.Selector  `json:"selectors,omitempty"`
	Retry                routing.Retry       `json:"retry,omitempty"`
	Affinity             *routing.Affinity   `json:"affinity,omitempty"`
	Budget               *routing.CostLimits `json:"budget,omitempty"`
	Retired              bool                `json:"retired"`
}

// TemplateEntry is a route template. Apply stores it; provider activation and
// the template's apply operation generate its routes.
type TemplateEntry struct {
	Name             string          `json:"name"`
	Project          *string         `json:"project"`
	ProviderSelector string          `json:"provider_selector"`
	ModelFilter      string          `json:"model_filter"`
	SlugPattern      string          `json:"slug_pattern"`
	OverallTimeoutMS int             `json:"overall_timeout_ms"`
	MaxAttempts      int             `json:"max_attempts"`
	Fidelity         json.RawMessage `json:"fidelity"`
	RoutingPolicy    *routing.Policy `json:"routing_policy"`
	AutoPublish      bool            `json:"auto_publish"`
}

func (t *TemplateEntry) input(projectID *string) routes.TemplateInput {
	return routes.TemplateInput{
		Name: t.Name, ProviderSelector: t.ProviderSelector, ModelFilter: t.ModelFilter, SlugPattern: t.SlugPattern,
		OverallTimeoutMS: t.OverallTimeoutMS, MaxAttempts: t.MaxAttempts, Fidelity: t.Fidelity,
		RoutingPolicy: t.RoutingPolicy, AutoPublish: t.AutoPublish, ProjectID: projectID,
	}
}

// normalize applies what template validation stores, so a document compares
// equal to the installation it was exported from.
func (t *TemplateEntry) normalize() error {
	in := t.input(nil)
	if err := routes.ValidateTemplateInput(&in); err != nil {
		return err
	}
	t.Fidelity, t.RoutingPolicy = in.Fidelity, in.RoutingPolicy
	return nil
}

func (r *RouteEntry) behavior() routing.Behavior {
	return routing.Behavior{Cache: r.Cache, PromptCache: r.PromptCache, CallerCostExempt: r.CallerCostExempt, MaxBodyBytes: r.MaxBodyBytes, Fallbacks: r.Fallbacks, Selectors: r.Selectors, Retry: r.Retry, Affinity: r.Affinity, Budget: r.Budget}
}

func (r *RouteEntry) setBehavior(raw []byte) error {
	behavior, err := routing.DecodeBehavior(raw)
	if err != nil {
		return err
	}
	r.MaxBodyBytes = behavior.MaxBodyBytes
	r.CallerCostExempt = behavior.CallerCostExempt
	r.Cache, r.PromptCache = behavior.Cache, behavior.PromptCache
	r.Fallbacks, r.Selectors, r.Retry, r.Affinity, r.Budget = behavior.Fallbacks, behavior.Selectors, behavior.Retry, behavior.Affinity, behavior.Budget
	return nil
}

type PriceEntry struct {
	VendorID                    *string             `json:"vendor_id"`
	ProviderKind                string              `json:"provider_kind"`
	Provider                    *string             `json:"provider"`
	Model                       string              `json:"model"`
	Operation                   string              `json:"operation"`
	InputPerMillion             *string             `json:"input_per_million"`
	CachedInputPerMillion       *string             `json:"cached_input_per_million"`
	OutputPerMillion            *string             `json:"output_per_million"`
	CacheWriteInputPerMillion   *string             `json:"cache_write_input_per_million"`
	CacheWrite5MInputPerMillion *string             `json:"cache_write_5m_input_per_million"`
	CacheWrite1HInputPerMillion *string             `json:"cache_write_1h_input_per_million"`
	AudioInputPerMillion        *string             `json:"audio_input_per_million"`
	CachedAudioInputPerMillion  *string             `json:"cached_audio_input_per_million"`
	AudioOutputPerMillion       *string             `json:"audio_output_per_million"`
	ImageInputPerMillion        *string             `json:"image_input_per_million"`
	CachedImageInputPerMillion  *string             `json:"cached_image_input_per_million"`
	ImageOutputPerMillion       *string             `json:"image_output_per_million"`
	UnitPrice                   *string             `json:"unit_price"`
	PerRequest                  *string             `json:"per_request"`
	BatchMultiplier             *string             `json:"batch_multiplier"`
	ToolCalls                   map[string]string   `json:"tool_calls"`
	Tiers                       []usage.PriceTier   `json:"tiers"`
	Windows                     *usage.PriceWindows `json:"windows"`
	Currency                    string              `json:"currency"`
}

type PricingEntry struct {
	EffectiveAt string       `json:"effective_at"`
	Prices      []PriceEntry `json:"prices"`
}

// DiscountEntry is one discount scope in the document; providers are named,
// as prices scope them.
type DiscountEntry struct {
	ProviderKind string  `json:"provider_kind"`
	VendorID     *string `json:"vendor_id,omitempty"`
	Provider     *string `json:"provider,omitempty"`
	Multiplier   string  `json:"multiplier"`
}

// RateCardEntry is the in-force revision of one owner's card.
type RateCardEntry struct {
	Owner  CardOwnerEntry `json:"owner"`
	Markup string         `json:"markup"`
	Prices []PriceEntry   `json:"prices,omitempty"`
}

// CardOwnerEntry references a card owner by kind and name.
type CardOwnerEntry struct {
	Kind         string  `json:"kind"`
	Organization *string `json:"organization,omitempty"`
	Project      *string `json:"project,omitempty"`
}

// priceEntryFromUsage carries one stored price into the configuration document.
func priceEntryFromUsage(price usage.Price) PriceEntry {
	return PriceEntry{
		VendorID: price.VendorID, ProviderKind: price.ProviderKind, Model: price.Model, Operation: price.Operation,
		InputPerMillion: price.InputPerMillion, CachedInputPerMillion: price.CachedInputPerMillion,
		OutputPerMillion: price.OutputPerMillion, CacheWriteInputPerMillion: price.CacheWriteInputPerMillion,
		CacheWrite5MInputPerMillion: price.CacheWrite5MInputPerMillion,
		CacheWrite1HInputPerMillion: price.CacheWrite1HInputPerMillion,
		AudioInputPerMillion:        price.AudioInputPerMillion,
		CachedAudioInputPerMillion:  price.CachedAudioInputPerMillion,
		AudioOutputPerMillion:       price.AudioOutputPerMillion,
		ImageInputPerMillion:        price.ImageInputPerMillion,
		CachedImageInputPerMillion:  price.CachedImageInputPerMillion,
		ImageOutputPerMillion:       price.ImageOutputPerMillion,
		UnitPrice:                   price.UnitPrice,
		PerRequest:                  price.PerRequest, BatchMultiplier: price.BatchMultiplier,
		ToolCalls: price.ToolCalls, Tiers: price.Tiers, Windows: price.Windows,
		Currency: price.Currency}
}

// usagePrice carries one document entry into the usage price list.
func (e PriceEntry) usagePrice() usage.Price {
	return usage.Price{
		VendorID: e.VendorID, ProviderKind: e.ProviderKind, Model: e.Model, Operation: e.Operation,
		InputPerMillion: e.InputPerMillion, CachedInputPerMillion: e.CachedInputPerMillion,
		OutputPerMillion: e.OutputPerMillion, CacheWriteInputPerMillion: e.CacheWriteInputPerMillion,
		CacheWrite5MInputPerMillion: e.CacheWrite5MInputPerMillion,
		CacheWrite1HInputPerMillion: e.CacheWrite1HInputPerMillion,
		AudioInputPerMillion:        e.AudioInputPerMillion,
		CachedAudioInputPerMillion:  e.CachedAudioInputPerMillion,
		AudioOutputPerMillion:       e.AudioOutputPerMillion,
		ImageInputPerMillion:        e.ImageInputPerMillion,
		CachedImageInputPerMillion:  e.CachedImageInputPerMillion,
		ImageOutputPerMillion:       e.ImageOutputPerMillion,
		UnitPrice:                   e.UnitPrice,
		PerRequest:                  e.PerRequest, BatchMultiplier: e.BatchMultiplier,
		ToolCalls: e.ToolCalls, Tiers: e.Tiers, Windows: e.Windows,
		Currency: e.Currency}
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
	// An omitted fidelity is strict, so a document states every route's mode.
	if fidelity, err := routing.DecodeFidelity(r.Fidelity); err == nil {
		r.Fidelity, _ = json.Marshal(fidelity)
	}
	if r.Operations == nil {
		r.Operations = []string{}
	}
	slices.Sort(r.Operations)
	if r.Targets == nil {
		r.Targets = []TargetEntry{}
	}
	for i := range r.Targets {
		if len(r.Targets[i].Tags) == 0 {
			r.Targets[i].Tags = nil
		}
		slices.Sort(r.Targets[i].Tags)
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
	slices.SortFunc(d.MCPServers, func(a, b MCPServerEntry) int {
		ka, kb := mcpKey(a), mcpKey(b)
		if c := strings.Compare(ka[0], kb[0]); c != 0 {
			return c
		}
		return strings.Compare(ka[1], kb[1])
	})
	slices.SortFunc(d.Sinks, func(a, b SinkEntry) int {
		ka, kb := sinkKey(a), sinkKey(b)
		if c := strings.Compare(ka[0], kb[0]); c != 0 {
			return c
		}
		return strings.Compare(ka[1], kb[1])
	})
	slices.SortFunc(d.GuardrailPolicies, func(a, b GuardrailPolicyEntry) int {
		if c := strings.Compare(strings.ToLower(a.Project), strings.ToLower(b.Project)); c != 0 {
			return c
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	slices.SortFunc(d.GuardrailAttachments, func(a, b GuardrailAttachmentEntry) int {
		if c := strings.Compare(a.Scope, b.Scope); c != 0 {
			return c
		}
		if c := strings.Compare(a.Name, b.Name); c != 0 {
			return c
		}
		return a.Order - b.Order
	})
	slices.SortFunc(d.Guardrails, func(a, b GuardrailEntry) int {
		if order := strings.Compare(strings.ToLower(a.Project), strings.ToLower(b.Project)); order != 0 {
			return order
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	normalizeDocument(d)
	d.ExportedAt = ""
	slices.SortFunc(d.SCIMGroupMappings, func(a, b SCIMGroupMapping) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	for i := range d.SCIMGroupMappings {
		slices.SortFunc(d.SCIMGroupMappings[i].Projects, func(a, b SCIMProjectMapping) int {
			return strings.Compare(strings.ToLower(a.Project), strings.ToLower(b.Project))
		})
	}

	slices.SortFunc(d.WorkloadIssuers, func(a, b WorkloadIssuerEntry) int { return strings.Compare(a.Issuer, b.Issuer) })
	slices.SortFunc(d.Organizations, func(a, b OrganizationEntry) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
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
	for i := range d.Templates {
		_ = d.Templates[i].normalize()
	}
	slices.SortFunc(d.Templates, func(a, b TemplateEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	if len(d.Templates) == 0 {
		d.Templates = nil
	}
	if d.Pricing != nil {
		if d.Pricing.Prices == nil {
			d.Pricing.Prices = []PriceEntry{}
		}
		normalizePriceEntries(d.Pricing.Prices, "")
	}
	slices.SortFunc(d.Discounts, func(a, b DiscountEntry) int {
		return strings.Compare(discountScope(a.ProviderKind, a.Provider, a.VendorID), discountScope(b.ProviderKind, b.Provider, b.VendorID))
	})
	for i := range d.RateCards {
		sortPriceEntries(d.RateCards[i].Prices)
	}
	slices.SortFunc(d.RateCards, func(a, b RateCardEntry) int {
		return strings.Compare(cardKey(a.Owner), cardKey(b.Owner))
	})
}

func normalizePriceEntries(prices []PriceEntry, defaultCurrency string) {
	for i := range prices {
		p := &prices[i]
		p.Model = strings.TrimSpace(p.Model)
		p.Currency = strings.ToUpper(strings.TrimSpace(p.Currency))
		if p.Currency == "" {
			p.Currency = defaultCurrency
		}
		if p.VendorID != nil {
			v := strings.TrimSpace(*p.VendorID)
			p.VendorID = &v
		}
	}
	sortPriceEntries(prices)
}

func sortPriceEntries(prices []PriceEntry) {
	slices.SortFunc(prices, func(a, b PriceEntry) int {
		key := func(p *PriceEntry) string {
			return p.ProviderKind + "\x00" + lower(p.Provider) + "\x00" + lower(p.VendorID) + "\x00" + p.Model + "\x00" + p.Operation
		}
		return strings.Compare(key(&a), key(&b))
	})
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
