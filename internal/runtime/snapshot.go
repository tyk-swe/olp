// Package runtime owns the immutable serving snapshot: its schema, digest,
// validation, deterministic target selection, publication, and the gateway
// manager that installs releases and refreshes key authority.
package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
)

// RouteSlug is the published-route identifier carried in model fields.
var RouteSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

// Generation identifies one published runtime configuration.
type Generation struct {
	ID          string    `json:"id"`
	Ordinal     int64     `json:"ordinal"`
	ActivatedAt time.Time `json:"activated_at"`
}

// Capability is one certified (model, operation, surface, mode) tuple.
type Capability struct {
	Model     string `json:"model"`
	Operation string `json:"operation"`
	Surface   string `json:"surface"`
	Mode      string `json:"mode"`
}

// Slot is a credential slot pinned to an exact credential version.
type Slot struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Enabled           bool     `json:"enabled"`
	Priority          int      `json:"priority"`
	Weight            int64    `json:"weight"`
	CredentialID      *string  `json:"credential_id"`
	CredentialVersion *int     `json:"credential_version"`
	AllowedModels     []string `json:"allowed_models,omitempty"`
	AllowedRoutes     []string `json:"allowed_routes,omitempty"`
	AllowedAPIKeys    []string `json:"allowed_api_keys,omitempty"`
	RequestsPerMinute *int64   `json:"requests_per_minute,omitempty"`
	TokensPerMinute   *int64   `json:"tokens_per_minute,omitempty"`
	MaxConcurrency    *int64   `json:"max_concurrency,omitempty"`
}

// Allows applies the published slot restrictions shared by live requests and
// simulation. Credential authority and transient health are checked by callers.
func (s *Slot) Allows(model, route, keyID string) bool {
	return s.Enabled &&
		(len(s.AllowedModels) == 0 || slices.Contains(s.AllowedModels, model)) &&
		(len(s.AllowedRoutes) == 0 || slices.Contains(s.AllowedRoutes, route)) &&
		(len(s.AllowedAPIKeys) == 0 || (keyID != "" && slices.Contains(s.AllowedAPIKeys, keyID)))
}

// Provider is the serving view of one active provider revision.
type Provider struct {
	ID                string                     `json:"id"`
	Name              string                     `json:"name"`
	Kind              string                     `json:"kind"`
	Enabled           bool                       `json:"enabled"`
	ActiveCredential  *string                    `json:"active_credential"`
	Capabilities      []Capability               `json:"capabilities"`
	RevisionID        string                     `json:"revision_id"`
	Endpoint          string                     `json:"endpoint,omitempty"`
	AuthMode          string                     `json:"auth_mode,omitempty"`
	CredentialHeaders []string                   `json:"credential_headers,omitempty"`
	ParameterDefaults map[string]json.RawMessage `json:"parameter_defaults,omitempty"`
	Slots             []Slot                     `json:"slots,omitempty"`
}

// Target is one route attempt candidate.
type Target struct {
	ID            string `json:"id"`
	ProviderID    string `json:"provider_id"`
	ProviderModel string `json:"provider_model"`
	Priority      int    `json:"priority"`
	Weight        int64  `json:"weight"`
	Timeout       int64  `json:"timeout"`
	RoutingID     string `json:"routing_id"`
}

// Route is the latest published revision of one slug.
type Route struct {
	ID             string    `json:"id"`
	Slug           string    `json:"slug"`
	Operations     []string  `json:"operations"`
	OverallTimeout int64     `json:"overall_timeout"`
	MaxAttempts    int       `json:"max_attempts"`
	Targets        []Target  `json:"targets"`
	RoutingID      string    `json:"routing_id"`
	RevisionID     string    `json:"revision_id,omitempty"`
	Revision       int       `json:"revision,omitempty"`
	PublishedAt    time.Time `json:"published_at,omitempty"`
}

// Snapshot is the complete immutable serving configuration.
type Snapshot struct {
	Generation Generation          `json:"generation"`
	Providers  map[string]Provider `json:"providers"`
	Routes     map[string]Route    `json:"routes"`
}

// Digest returns the SHA-256 of the canonical JSON encoding of the serving
// content. The generation identity is excluded so republishing unchanged
// configuration yields the same digest.
func (s *Snapshot) Digest() (string, error) {
	encoded, err := json.Marshal(struct {
		Providers map[string]Provider `json:"providers"`
		Routes    map[string]Route    `json:"routes"`
	}{s.Providers, s.Routes})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// Supports reports whether the provider certified the tuple.
func (p *Provider) Supports(model, operation, surface, mode string) bool {
	for _, c := range p.Capabilities {
		if c.Model == model && c.Operation == operation && c.Surface == surface && c.Mode == mode {
			return true
		}
	}
	return false
}

// Validate rejects snapshots that could not serve safely: dangling references,
// non-positive budgets, malformed identifiers.
func (s *Snapshot) Validate() error {
	if _, err := uuid.Parse(s.Generation.ID); err != nil || s.Generation.Ordinal < 0 {
		return errors.New("generation identity is malformed")
	}
	for id, p := range s.Providers {
		if id != p.ID || !validUUID(p.ID) || p.RevisionID == "" || p.Name == "" || p.Kind == "" {
			return fmt.Errorf("provider %q is malformed", id)
		}
		if p.ActiveCredential != nil && !validUUID(*p.ActiveCredential) {
			return fmt.Errorf("provider %q credential reference is malformed", id)
		}
		seen := map[string]bool{}
		for _, slot := range p.Slots {
			if !validUUID(slot.ID) || seen[slot.ID] || slot.Weight < 1 {
				return fmt.Errorf("provider %q slot is malformed", id)
			}
			seen[slot.ID] = true
			if slot.CredentialID != nil && !validUUID(*slot.CredentialID) {
				return fmt.Errorf("provider %q slot credential reference is malformed", id)
			}
		}
	}
	for slug, r := range s.Routes {
		if slug != r.Slug || !RouteSlug.MatchString(slug) || !validUUID(r.ID) || !validUUID(r.RoutingID) {
			return fmt.Errorf("route %q is malformed", slug)
		}
		if r.OverallTimeout < 1 || r.MaxAttempts < 1 || len(r.Operations) == 0 || len(r.Targets) == 0 {
			return fmt.Errorf("route %q has no serving budget or targets", slug)
		}
		seen := map[string]bool{}
		for _, t := range r.Targets {
			if !validUUID(t.ID) || !validUUID(t.RoutingID) || seen[t.ID] || t.Weight < 1 || t.Timeout < 1 || t.ProviderModel == "" {
				return fmt.Errorf("route %q target is malformed", slug)
			}
			seen[t.ID] = true
			if _, ok := s.Providers[t.ProviderID]; !ok {
				return fmt.Errorf("route %q references unknown provider %s", slug, t.ProviderID)
			}
		}
	}
	return nil
}

func validUUID(value string) bool {
	_, err := uuid.Parse(value)
	return err == nil && len(value) == 36
}
