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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"time"
)

// APIVersion identifies the catalog document format.
const APIVersion = "openllmproxy.dev/catalog/v1"

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
	// UnitPrice is the price of one media unit the operation records: an
	// image, a second of audio or video, a character of speech input, or a
	// search unit.
	UnitPrice       *Decimal          `json:"unit_price,omitempty"`
	Unrepresentable []Unrepresentable `json:"unrepresentable"`
	Provenance      Provenance        `json:"provenance"`
}

// Unrepresentable is a price component OLP cannot yet price, in the vendor's
// own terms.
type Unrepresentable struct {
	Component string `json:"component"`
	Detail    string `json:"detail"`
}

var (
	vendorID    = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	modelID     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,255}$`)
	canonical   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}/[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	component   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	operation   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	currency    = regexp.MustCompile(`^[A-Z]{3}$`)
	modalities  = []string{"audio", "embeddings", "file", "image", "text", "video"}
	familyNames = []string{"anthropic", "gemini", "other"}
)

// Decode parses a catalog document strictly: unknown members, trailing data
// and another format version are refused. Decode does not validate.
func Decode(document []byte) (*Catalog, error) {
	if len(document) > MaxBytes {
		return nil, errors.New("the catalog exceeds its size limit")
	}
	var version struct {
		APIVersion string `json:"api_version"`
	}
	if err := json.Unmarshal(document, &version); err != nil {
		return nil, fmt.Errorf("the catalog is not a JSON object: %w", err)
	}
	if version.APIVersion != APIVersion {
		return nil, fmt.Errorf("%w: %q", ErrUnsupportedVersion, version.APIVersion)
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var c Catalog
	if err := decoder.Decode(&c); err != nil {
		return nil, fmt.Errorf("the catalog is malformed: %w", err)
	}
	if decoder.Decode(new(json.RawMessage)) != io.EOF {
		return nil, errors.New("the catalog has data after its document")
	}
	return &c, nil
}

// ErrUnsupportedVersion reports a catalog in a format this build cannot read.
var ErrUnsupportedVersion = errors.New("the catalog format version is not supported")

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
		if !operation.MatchString(p.Operation) || operations[p.Operation] {
			return fmt.Errorf("operation %q is malformed or priced twice", p.Operation)
		}
		operations[p.Operation] = true
		if err := c.validatePrice(p); err != nil {
			return fmt.Errorf("%s price: %w", p.Operation, err)
		}
	}
	return c.validateProvenance(m.Provenance)
}

func (c *Catalog) validatePrice(p Price) error {
	rates := []*Decimal{p.InputPerMillion, p.CachedInputPerMillion, p.CacheWriteInputPerMillion, p.CacheWrite5MInputPerMillion, p.CacheWrite1HInputPerMillion, p.OutputPerMillion, p.UnitPrice}
	priced := false
	for _, rate := range rates {
		if rate != nil {
			if !rate.Valid() {
				return fmt.Errorf("rate %q is not a decimal", *rate)
			}
			priced = true
		}
	}
	if !priced {
		return errors.New("a price needs at least one rate")
	}
	cached := p.CachedInputPerMillion != nil || p.CacheWriteInputPerMillion != nil || p.CacheWrite5MInputPerMillion != nil || p.CacheWrite1HInputPerMillion != nil
	if cached && p.InputPerMillion == nil {
		return errors.New("a cache rate needs an input rate")
	}
	if p.Unrepresentable == nil {
		return errors.New("unrepresentable components must be listed, even when there are none")
	}
	seen := map[string]bool{}
	for _, u := range p.Unrepresentable {
		if !component.MatchString(u.Component) || u.Detail == "" || seen[u.Component] {
			return fmt.Errorf("unrepresentable component %q is malformed, unexplained or repeated", u.Component)
		}
		seen[u.Component] = true
	}
	return c.validateProvenance(p.Provenance)
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
