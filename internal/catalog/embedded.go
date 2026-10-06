package catalog

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/tyk-swe/olp/internal/signing"
)

// The catalog this release ships and its detached signature.
var (
	//go:embed catalog.json
	document []byte
	//go:embed catalog.json.sig
	signature []byte
)

// Signed is a catalog whose signature verified against a trusted key.
type Signed struct {
	Catalog *Catalog
	// SHA256 is the hex digest of the signed document bytes.
	SHA256 string
	// KeyID is the trusted key that verified the signature.
	KeyID string
	// index finds a vendor's models by identifier and alias, byCanonical by
	// canonical model; the first model to name a canonical model keeps it.
	index, byCanonical map[string]map[string]*Model
}

// Match is how a lookup name matched a catalog model.
type Match string

// The ways a name matches a model.
const (
	MatchID        Match = "id"
	MatchAlias     Match = "alias"
	MatchCanonical Match = "canonical_model"
)

// Load verifies a catalog document against its signature with keys, then
// decodes and validates it. It is the only way to obtain a Signed catalog.
func Load(document, signature []byte, keys signing.Keyring) (*Signed, error) {
	keyID, err := keys.Verify(document, signature)
	if err != nil {
		return nil, fmt.Errorf("reference catalog signature: %w", err)
	}
	c, err := Decode(document)
	if err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("reference catalog: %w", err)
	}
	digest := sha256.Sum256(document)
	signed := &Signed{Catalog: c, SHA256: hex.EncodeToString(digest[:]), KeyID: keyID,
		index: map[string]map[string]*Model{}, byCanonical: map[string]map[string]*Model{}}
	for i := range c.Vendors {
		vendor := &c.Vendors[i]
		names, canonicals := map[string]*Model{}, map[string]*Model{}
		for j := range vendor.Models {
			model := &vendor.Models[j]
			names[model.ID] = model
			for _, alias := range model.Aliases {
				names[alias] = model
			}
			if _, named := canonicals[model.CanonicalModel]; !named && model.CanonicalModel != "" {
				canonicals[model.CanonicalModel] = model
			}
		}
		signed.index[vendor.ID], signed.byCanonical[vendor.ID] = names, canonicals
	}
	return signed, nil
}

// Embedded is the catalog this release ships, verified against the keys this
// build trusts. A process refuses to start when it does not verify.
var Embedded = sync.OnceValues(func() (*Signed, error) {
	return Load(document, signature, signing.Trusted())
})

// Source is the provenance tag of facts taken from this catalog.
func (s *Signed) Source() string { return "catalog@" + s.SHA256 }

// Lookup finds a vendor's model by the first name that matches its
// identifier, an alias, or its canonical model.
func (s *Signed) Lookup(vendorID string, names ...string) (*Model, Match, bool) {
	models := s.index[vendorID]
	for _, name := range names {
		if name == "" {
			continue
		}
		if model, ok := models[name]; ok {
			if model.ID == name {
				return model, MatchID, true
			}
			return model, MatchAlias, true
		}
	}
	for _, name := range names {
		if model, ok := s.byCanonical[vendorID][name]; ok {
			return model, MatchCanonical, true
		}
	}
	return nil, "", false
}

// Lifecycle is the documented deprecation and retirement of a vendor's model,
// or nil when the catalog documents none. A nil catalog documents nothing.
func (s *Signed) Lifecycle(vendorID string, names ...string) *LifecycleView {
	if s == nil {
		return nil
	}
	if model, _, ok := s.Lookup(vendorID, names...); ok {
		return model.Lifecycle.View()
	}
	return nil
}

// LifecycleView is a lifecycle as the management API shows it.
type LifecycleView struct {
	DeprecatedAt *string `json:"deprecated_at"`
	RetiresAt    *string `json:"retires_at"`
	Replacement  *string `json:"replacement"`
	// Source is the vendor page that documents the dates.
	Source string `json:"source"`
}

// View is the lifecycle as the management API shows it, or nil.
func (l *Lifecycle) View() *LifecycleView {
	if l == nil {
		return nil
	}
	view := &LifecycleView{Source: l.Provenance.SourceURL}
	if l.DeprecatedAt != nil {
		view.DeprecatedAt = new(string(*l.DeprecatedAt))
	}
	if l.RetiresAt != nil {
		view.RetiresAt = new(string(*l.RetiresAt))
	}
	if l.Replacement != "" {
		view.Replacement = new(l.Replacement)
	}
	return view
}

// ModelCount is the number of models the catalog describes.
func (s *Signed) ModelCount() int {
	n := 0
	for _, vendor := range s.Catalog.Vendors {
		n += len(vendor.Models)
	}
	return n
}

// Summary describes a verified catalog to the management API.
type Summary struct {
	APIVersion  string `json:"api_version"`
	PublishedAt string `json:"published_at"`
	SHA256      string `json:"sha256"`
	KeyID       string `json:"key_id"`
	VendorCount int    `json:"vendor_count"`
	ModelCount  int    `json:"model_count"`
}

// Summary describes the catalog.
func (s *Signed) Summary() Summary {
	return Summary{APIVersion: s.Catalog.APIVersion, PublishedAt: s.Catalog.PublishedAt.UTC().Format(time.RFC3339), SHA256: s.SHA256, KeyID: s.KeyID,
		VendorCount: len(s.Catalog.Vendors), ModelCount: s.ModelCount()}
}
