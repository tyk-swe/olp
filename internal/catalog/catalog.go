// Package catalog is the signed reference catalog: model facts and list
// prices by vendor, transcribed from each vendor's own documentation with the
// page and time every entry was observed.
//
// The catalog is advisory. Discovery offers its facts, pricing sources map
// its prices into snapshots an operator publishes, and route editing warns
// about retirements; nothing in it certifies a model or prices an attempt on
// its own. Every catalog this package hands out has verified against a
// trusted key: Load is the only constructor of a Signed catalog.
package catalog

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/signing"
)

// APIVersion identifies the catalog document format.
const APIVersion = "openllmproxy.dev/catalog/v2"

// MaxBytes bounds a catalog document.
const MaxBytes = 8 << 20

// Catalog is a reference catalog document.
type Catalog struct {
	APIVersion string `json:"api_version"`
	// PublishedAt orders catalogs: a refresh never accepts an older one.
	PublishedAt time.Time `json:"published_at"`
	// Currency of every price, an ISO 4217 code.
	Currency   string       `json:"currency"`
	Estimation []Estimation `json:"estimation"`
	Vendors    []Vendor     `json:"vendors"`
}

// Provenance is where and when an entry was observed.
type Provenance struct {
	SourceURL  string    `json:"source_url"`
	ObservedAt time.Time `json:"observed_at"`
}

// Estimation is a token-estimation factor for a model family without a public
// tokenizer, measured against the vendor's own token counts.
type Estimation struct {
	Family     string     `json:"family"`
	Factor     Decimal    `json:"factor"`
	Provenance Provenance `json:"provenance"`
}

// Vendor is a vendor's models. ID is an OLP vendor identifier.
type Vendor struct {
	ID     string  `json:"id"`
	Name   string  `json:"name"`
	Models []Model `json:"models"`
}

// Model is a model's documented facts and list prices.
type Model struct {
	// ID is the model the vendor's API is sent.
	ID string `json:"id"`
	// Aliases are other identifiers the vendor resolves to the same model at
	// the same price, such as dated snapshots.
	Aliases []string `json:"aliases,omitempty"`
	// CanonicalModel is an organization/model identity across vendors.
	CanonicalModel   string   `json:"canonical_model,omitempty"`
	ContextLength    *int64   `json:"context_length,omitempty"`
	MaxOutputTokens  *int64   `json:"max_output_tokens,omitempty"`
	InputModalities  []string `json:"input_modalities"`
	OutputModalities []string `json:"output_modalities"`
	// SupportedParameters, when present, are the request parameters the
	// vendor documents for the model.
	SupportedParameters *[]string    `json:"supported_parameters,omitempty"`
	Capabilities        Capabilities `json:"capabilities"`
	Lifecycle           *Lifecycle   `json:"lifecycle,omitempty"`
	Prices              []Price      `json:"prices"`
	Provenance          Provenance   `json:"provenance"`
}

// Capabilities are documented hints; an absent hint is unknown.
type Capabilities struct {
	Tools             *bool `json:"tools,omitempty"`
	StructuredOutputs *bool `json:"structured_outputs,omitempty"`
	Reasoning         *bool `json:"reasoning,omitempty"`
	PromptCaching     *bool `json:"prompt_caching,omitempty"`
}

// Lifecycle is a model's documented deprecation and retirement.
type Lifecycle struct {
	DeprecatedAt *Date      `json:"deprecated_at,omitempty"`
	RetiresAt    *Date      `json:"retires_at,omitempty"`
	Replacement  string     `json:"replacement,omitempty"`
	Provenance   Provenance `json:"provenance"`
}

// Price is an operation's list price in every component OLP prices. A
// component the vendor charges that OLP cannot yet price is named in
// Unrepresentable rather than dropped silently.
type Price struct {
	Operation                   string   `json:"operation"`
	InputPerMillion             *Decimal `json:"input_per_million,omitempty"`
	CachedInputPerMillion       *Decimal `json:"cached_input_per_million,omitempty"`
	CacheWriteInputPerMillion   *Decimal `json:"cache_write_input_per_million,omitempty"`
	CacheWrite5MInputPerMillion *Decimal `json:"cache_write_5m_input_per_million,omitempty"`
	CacheWrite1HInputPerMillion *Decimal `json:"cache_write_1h_input_per_million,omitempty"`
	OutputPerMillion            *Decimal `json:"output_per_million,omitempty"`
	AudioInputPerMillion        *Decimal `json:"audio_input_per_million,omitempty"`
	CachedAudioInputPerMillion  *Decimal `json:"cached_audio_input_per_million,omitempty"`
	AudioOutputPerMillion       *Decimal `json:"audio_output_per_million,omitempty"`
	ImageInputPerMillion        *Decimal `json:"image_input_per_million,omitempty"`
	CachedImageInputPerMillion  *Decimal `json:"cached_image_input_per_million,omitempty"`
	ImageOutputPerMillion       *Decimal `json:"image_output_per_million,omitempty"`
	// UnitPrice is the price of one media unit the operation records: an
	// image, a second of audio or video, a character of speech input, or a
	// search unit.
	UnitPrice       *Decimal           `json:"unit_price,omitempty"`
	PerRequest      *Decimal           `json:"per_request,omitempty"`
	ToolCalls       map[string]Decimal `json:"tool_calls,omitempty"`
	Tiers           []Tier             `json:"tiers,omitempty"`
	Windows         *Windows           `json:"windows,omitempty"`
	BatchMultiplier *Decimal           `json:"batch_multiplier,omitempty"`
	Unrepresentable []Unrepresentable  `json:"unrepresentable"`
	Provenance      Provenance         `json:"provenance"`
}

// Tier is the per-million rate set above an input-token threshold.
type Tier struct {
	AboveInputTokens            int64    `json:"above_input_tokens"`
	InputPerMillion             *Decimal `json:"input_per_million,omitempty"`
	CachedInputPerMillion       *Decimal `json:"cached_input_per_million,omitempty"`
	CacheWriteInputPerMillion   *Decimal `json:"cache_write_input_per_million,omitempty"`
	CacheWrite5MInputPerMillion *Decimal `json:"cache_write_5m_input_per_million,omitempty"`
	CacheWrite1HInputPerMillion *Decimal `json:"cache_write_1h_input_per_million,omitempty"`
	OutputPerMillion            *Decimal `json:"output_per_million,omitempty"`
	AudioInputPerMillion        *Decimal `json:"audio_input_per_million,omitempty"`
	CachedAudioInputPerMillion  *Decimal `json:"cached_audio_input_per_million,omitempty"`
	AudioOutputPerMillion       *Decimal `json:"audio_output_per_million,omitempty"`
	ImageInputPerMillion        *Decimal `json:"image_input_per_million,omitempty"`
	CachedImageInputPerMillion  *Decimal `json:"cached_image_input_per_million,omitempty"`
	ImageOutputPerMillion       *Decimal `json:"image_output_per_million,omitempty"`
}

// Windows are time-of-day multipliers in a named IANA zone.
type Windows struct {
	TimeZone string         `json:"time_zone"`
	Periods  []WindowPeriod `json:"periods"`
}

// WindowPeriod is one multiplier for a set of weekdays over a local
// half-open [start, end) window; "24:00" runs through midnight.
type WindowPeriod struct {
	Days       []int   `json:"days"`
	Start      string  `json:"start"`
	End        string  `json:"end"`
	Multiplier Decimal `json:"multiplier"`
}

// Unrepresentable is a price component OLP cannot yet price, in the vendor's
// own terms.
type Unrepresentable struct {
	Component string `json:"component"`
	Detail    string `json:"detail"`
}

var (
	vendorID    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	modelID     = regexp.MustCompile(`^[A-Za-z0-9@][A-Za-z0-9._:/@+-]{0,255}$`)
	canonical   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}/[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	snakeName   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	currency    = regexp.MustCompile(`^[A-Z]{3}$`)
	modalities  = []string{"audio", "embeddings", "file", "image", "text", "video"}
	familyNames = []string{"anthropic", "gemini", "other"}
)

// Decode parses a catalog document strictly: unknown members, trailing data
// and another format version are refused. Decode does not validate.
func Decode(document []byte) (*Catalog, error) {
	var c Catalog
	if err := signing.DecodeDocument(document, "the catalog", APIVersion, MaxBytes, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks every rule the schema states and the ones it cannot:
// uniqueness, ordering of dates, and provenance no later than publication.
func (c *Catalog) Validate() error {
	if c.APIVersion != APIVersion || !currency.MatchString(c.Currency) || c.PublishedAt.IsZero() {
		return errors.New("the catalog needs its format version, a currency and a publication time")
	}
	families := map[string]bool{}
	for _, e := range c.Estimation {
		if !slices.Contains(familyNames, e.Family) || families[e.Family] {
			return fmt.Errorf("estimation family %q is unknown or repeated", e.Family)
		}
		families[e.Family] = true
		if factor, err := e.Factor.Float64(); err != nil || factor <= 0 {
			return fmt.Errorf("estimation family %s needs a positive factor", e.Family)
		}
		if err := c.validateProvenance(e.Provenance); err != nil {
			return fmt.Errorf("estimation family %s: %w", e.Family, err)
		}
	}
	vendors := map[string]bool{}
	for _, v := range c.Vendors {
		if !vendorID.MatchString(v.ID) || v.Name == "" || vendors[v.ID] {
			return fmt.Errorf("vendor %q is malformed or repeated", v.ID)
		}
		vendors[v.ID] = true
		names := map[string]bool{}
		for i := range v.Models {
			if err := c.validateModel(&v.Models[i], names); err != nil {
				return fmt.Errorf("vendor %s model %s: %w", v.ID, v.Models[i].ID, err)
			}
		}
	}
	return nil
}

func (c *Catalog) validateModel(m *Model, names map[string]bool) error {
	for _, name := range append([]string{m.ID}, m.Aliases...) {
		if !modelID.MatchString(name) || names[name] {
			return fmt.Errorf("identifier %q is malformed or names another model", name)
		}
		names[name] = true
	}
	if m.CanonicalModel != "" && !canonical.MatchString(m.CanonicalModel) {
		return errors.New("canonical_model must be organization/model")
	}
	if m.ContextLength != nil && *m.ContextLength < 1 || m.MaxOutputTokens != nil && *m.MaxOutputTokens < 1 {
		return errors.New("token limits must be positive")
	}
	for _, list := range [][]string{m.InputModalities, m.OutputModalities} {
		if len(list) == 0 {
			return errors.New("input and output modalities are required")
		}
		for _, modality := range list {
			if !slices.Contains(modalities, modality) {
				return fmt.Errorf("modality %q is unknown", modality)
			}
		}
	}
	if m.Lifecycle != nil {
		l := m.Lifecycle
		if l.DeprecatedAt == nil && l.RetiresAt == nil {
			return errors.New("a lifecycle needs a deprecation or retirement date")
		}
		for _, date := range []*Date{l.DeprecatedAt, l.RetiresAt} {
			if date != nil && !date.Valid() {
				return fmt.Errorf("lifecycle date %q is malformed", *date)
			}
		}
		if l.DeprecatedAt != nil && l.RetiresAt != nil && *l.RetiresAt < *l.DeprecatedAt {
			return errors.New("a model retires after its deprecation")
		}
		if err := c.validateProvenance(l.Provenance); err != nil {
			return fmt.Errorf("lifecycle: %w", err)
		}
	}
	if len(m.Prices) == 0 {
		return errors.New("a model needs a list price")
	}
	operations := map[string]bool{}
	for _, p := range m.Prices {
		if !snakeName.MatchString(p.Operation) || operations[p.Operation] {
			return fmt.Errorf("operation %q is malformed or priced twice", p.Operation)
		}
		operations[p.Operation] = true
		if err := c.validatePrice(p); err != nil {
			return fmt.Errorf("%s price: %w", p.Operation, err)
		}
	}
	return c.validateProvenance(m.Provenance)
}

// perMillionRates names each per-million rate pointer in a price or tier so
// dependency, key-set and decimal checks see exactly one list.
func priceRates(p *Price) []*Decimal {
	return []*Decimal{p.InputPerMillion, p.CachedInputPerMillion, p.CacheWriteInputPerMillion,
		p.CacheWrite5MInputPerMillion, p.CacheWrite1HInputPerMillion, p.OutputPerMillion,
		p.AudioInputPerMillion, p.CachedAudioInputPerMillion, p.AudioOutputPerMillion,
		p.ImageInputPerMillion, p.CachedImageInputPerMillion, p.ImageOutputPerMillion}
}

func tierRates(t *Tier) []*Decimal {
	return []*Decimal{t.InputPerMillion, t.CachedInputPerMillion, t.CacheWriteInputPerMillion,
		t.CacheWrite5MInputPerMillion, t.CacheWrite1HInputPerMillion, t.OutputPerMillion,
		t.AudioInputPerMillion, t.CachedAudioInputPerMillion, t.AudioOutputPerMillion,
		t.ImageInputPerMillion, t.CachedImageInputPerMillion, t.ImageOutputPerMillion}
}

// unrepresentableComponents is the closed vocabulary of price components OLP
// cannot yet price. Anything representable by a revision — every per-million
// rate, per-request, tool fees, tiers, windows and batch — may not appear.
var unrepresentableComponents = map[string]bool{
	"ambiguous": true, "batch_rates": true, "cache_storage": true,
	"conditions": true, "free_allowance": true, "holiday_calendar": true,
	"media_units": true, "minimum_charge": true, "plan_pricing": true,
	"prepaid_credits": true, "promotion": true, "regional_uplift": true,
	"service_tier": true, "thinking_mode": true, "tool_usage": true,
	"unpublished_rates": true, "video_input": true,
}

var dayTime = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$|^24:00$`)

func (c *Catalog) validatePrice(p Price) error {
	priced := false
	for _, rate := range append(priceRates(&p), p.UnitPrice, p.PerRequest, p.BatchMultiplier) {
		if rate != nil {
			if !rate.Valid() {
				return fmt.Errorf("rate %q is not a decimal", *rate)
			}
			priced = true
		}
	}
	cached := p.CachedInputPerMillion != nil || p.CacheWriteInputPerMillion != nil || p.CacheWrite5MInputPerMillion != nil || p.CacheWrite1HInputPerMillion != nil
	if cached && p.InputPerMillion == nil {
		return errors.New("a cache rate needs an input rate")
	}
	if len(p.ToolCalls) > 0 {
		if len(p.ToolCalls) > 16 {
			return errors.New("tool_calls takes at most 16 tool fees")
		}
		for tool, fee := range p.ToolCalls {
			if !snakeName.MatchString(tool) {
				return fmt.Errorf("tool name %q is malformed", tool)
			}
			if !fee.Valid() {
				return fmt.Errorf("tool fee %q is not a decimal", string(fee))
			}
		}
		priced = true
	}
	if !priced {
		return errors.New("a price needs at least one rate")
	}
	if err := validateTiers(&p); err != nil {
		return err
	}
	if err := validateWindows(p.Windows); err != nil {
		return err
	}
	if p.Unrepresentable == nil {
		return errors.New("unrepresentable components must be listed, even when there are none")
	}
	seen := map[string]bool{}
	for _, u := range p.Unrepresentable {
		if !unrepresentableComponents[u.Component] {
			return fmt.Errorf("unrepresentable component %q is not a component OLP fails to price", u.Component)
		}
		if u.Detail == "" || seen[u.Component] {
			return fmt.Errorf("unrepresentable component %q is unexplained or repeated", u.Component)
		}
		seen[u.Component] = true
	}
	return c.validateProvenance(p.Provenance)
}

// validateTiers applies the same rules a pricing revision does: one to eight
// tiers, strictly increasing positive thresholds, each naming exactly the
// entry's own per-million rate set.
func validateTiers(p *Price) error {
	if len(p.Tiers) == 0 {
		return nil
	}
	if len(p.Tiers) > 8 {
		return errors.New("tiers take at most 8 thresholds")
	}
	entry := priceRates(p)
	previous := int64(0)
	for index, tier := range p.Tiers {
		if tier.AboveInputTokens <= 0 || tier.AboveInputTokens <= previous && index > 0 {
			return errors.New("tier thresholds must be positive and strictly increasing")
		}
		previous = tier.AboveInputTokens
		rates := tierRates(&p.Tiers[index])
		for i := range entry {
			if (entry[i] == nil) != (rates[i] == nil) {
				return errors.New("a tier names the same per-million rates as its entry")
			}
			if rates[i] != nil && !rates[i].Valid() {
				return fmt.Errorf("tier rate %q is not a decimal", *rates[i])
			}
		}
	}
	return nil
}

// validateWindows applies the pricing-revision window rules: a known IANA
// zone, 1–32 non-overlapping periods each with unique ISO weekdays and
// start < end ("24:00" allowed only at the end).
func validateWindows(windows *Windows) error {
	if windows == nil {
		return nil
	}
	if windows.TimeZone == "" {
		return errors.New("windows need an IANA time zone")
	}
	if _, err := time.LoadLocation(windows.TimeZone); err != nil {
		return fmt.Errorf("time zone %q is unknown", windows.TimeZone)
	}
	if len(windows.Periods) == 0 || len(windows.Periods) > 32 {
		return errors.New("windows take 1–32 periods")
	}
	periods := make([]WindowPeriod, 0, len(windows.Periods))
	for index, period := range windows.Periods {
		if len(period.Days) == 0 {
			return errors.New("a window period needs days")
		}
		seen := map[int]bool{}
		for _, day := range period.Days {
			if day < 1 || day > 7 {
				return errors.New("window days are ISO weekdays 1–7")
			}
			if seen[day] {
				return errors.New("window days repeat")
			}
			seen[day] = true
		}
		if !dayTime.MatchString(period.Start) || period.Start == "24:00" ||
			!dayTime.MatchString(period.End) {
			return fmt.Errorf("period %d times are HH:MM (end may be 24:00)", index)
		}
		if period.End != "24:00" && period.End <= period.Start {
			return fmt.Errorf("period %d ends after it starts", index)
		}
		if !period.Multiplier.Valid() {
			return fmt.Errorf("period %d multiplier is not a decimal", index)
		}
		for _, earlier := range periods {
			shared := false
			for _, day := range period.Days {
				shared = shared || slices.Contains(earlier.Days, day)
			}
			if shared && period.Start < earlier.End && earlier.Start < period.End {
				return fmt.Errorf("period %d overlaps an earlier one", index)
			}
		}
		periods = append(periods, period)
	}
	return nil
}

func (c *Catalog) validateProvenance(p Provenance) error {
	u, err := url.Parse(p.SourceURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("provenance needs an HTTPS source URL")
	}
	if p.ObservedAt.IsZero() || p.ObservedAt.After(c.PublishedAt) {
		return errors.New("provenance needs an observation time no later than publication")
	}
	return nil
}
