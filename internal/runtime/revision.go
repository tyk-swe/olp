package runtime

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

// RevisionModel is the published shape of one enabled model inside a provider
// revision. Provider activation writes it; publication reads it.
type RevisionModel struct {
	ID            string               `json:"id"`
	UpstreamModel string               `json:"upstream_model"`
	DisplayName   string               `json:"display_name"`
	Capabilities  []RevisionCapability `json:"capabilities"`
}

// RevisionCapability records whether a tuple was certified when activated.
type RevisionCapability struct {
	Operation   string     `json:"operation"`
	Surface     string     `json:"surface"`
	Mode        string     `json:"mode"`
	Source      string     `json:"source"`
	CertifiedAt *time.Time `json:"certified_at,omitempty"`
}

// RevisionSlot is the published shape of one credential slot.
type RevisionSlot struct {
	Slot
	Default bool `json:"default"`
}

// Configuration is the subset of a provider configuration the gateway needs.
type Configuration struct {
	ProfileID       string `json:"profile_id,omitempty"`
	ProfileRevision string `json:"profile_revision,omitempty"`
	Kind            string `json:"kind"`
	AuthMode        string `json:"auth_mode"`
	Endpoint        string `json:"endpoint"`
	CloudRegion     string `json:"cloud_region"`
	CloudProject    string `json:"cloud_project"`
	Deployment      string `json:"deployment"`
	APIVersion      string `json:"api_version"`
	Options         struct {
		Network           *egress.ConnectionOptions        `json:"network,omitempty"`
		SemanticHeaders   map[string]string                `json:"semantic_headers,omitempty"`
		QuerySettings     map[string]string                `json:"query_settings,omitempty"`
		OperationDefaults map[string]connectors.DefaultSet `json:"operation_defaults,omitempty"`
		Bindings          map[string]connectors.Binding    `json:"bindings,omitempty"`
		Models            map[string]json.RawMessage       `json:"models"`
		CredentialHeaders []string                         `json:"credential_headers"`
		Limits            *Limits                          `json:"limits"`
		ParameterDefaults map[string]json.RawMessage       `json:"parameter_defaults"`
		VendorID          string                           `json:"vendor_id"`
		PluginOptions     map[string]string                `json:"plugin_options,omitempty"`
	} `json:"options"`
}

// PublishedTarget is the stored shape of one route revision target. The
// provider model identity doubles as the routing identity so affinity survives
// revisions that keep the same target.
type PublishedTarget struct {
	ID              string `json:"id"`
	ProviderModelID string `json:"provider_model_id"`
	ProviderID      string `json:"provider_id"`
	ProviderName    string `json:"provider_name"`
	ProviderModel   string `json:"provider_model"`
	Priority        int    `json:"priority"`
	Weight          int64  `json:"weight"`
	TimeoutMS       int64  `json:"timeout_ms"`
	Position        int    `json:"position"`
}

// ProviderRevision contains scanned metadata and the stored documents of one
// provider revision. The caller selects the revision and its current state,
// and the manifest of the plugin a plugin provider's revision pins, which
// PluginManifestColumn selects.
type ProviderRevision struct {
	ID, RevisionID, Name, State  string
	ProjectID                    *string
	Configuration, Models, Slots []byte
	PluginManifest               []byte
}

// PluginManifestColumn selects the manifest of the plugin that the provider
// revision r pins, or NULL. A pinned plugin stays installed.
const PluginManifestColumn = "(SELECT pl.manifest FROM olp.plugins pl WHERE r.configuration->>'kind'='plugin' AND pl.digest=r.configuration->>'profile_revision')"

// DecodeProviderRevision reconstructs a serving provider without consulting
// current authority or normalizing stored limits. Publication owns empty-limit
// normalization; retained resources preserve the admitted revision's limits.
func DecodeProviderRevision(revision ProviderRevision) (Provider, error) {
	var cfg Configuration
	var models []RevisionModel
	var slots []RevisionSlot
	err := json.Unmarshal(revision.Configuration, &cfg)
	if err == nil {
		err = json.Unmarshal(revision.Models, &models)
	}
	if err == nil {
		err = json.Unmarshal(revision.Slots, &slots)
	}
	var plugin *connectors.PluginProfile
	if err == nil && cfg.Kind == connectors.KindPlugin {
		plugin, err = connectors.DecodePluginProfile(cfg.ProfileRevision, revision.PluginManifest, cfg.ProfileID)
	}
	if err != nil {
		return Provider{}, fmt.Errorf("provider %s revision: %w", revision.ID, err)
	}
	provider := Provider{
		ID: revision.ID, RevisionID: revision.RevisionID, Name: revision.Name,
		Enabled: revision.State == "active", ProjectID: revision.ProjectID,
		Network: cfg.Options.Network, Plugin: plugin, PluginOptions: cfg.Options.PluginOptions,
		ProfileID: cfg.ProfileID, ProfileRevision: cfg.ProfileRevision,
		SemanticHeaders: cfg.Options.SemanticHeaders, QuerySettings: cfg.Options.QuerySettings,
		OperationDefaults: cfg.Options.OperationDefaults, Bindings: cfg.Options.Bindings,
		Kind: cfg.Kind, AuthMode: cfg.AuthMode, Endpoint: cfg.Endpoint,
		CloudRegion: cfg.CloudRegion, CloudProject: cfg.CloudProject, Deployment: cfg.Deployment, APIVersion: cfg.APIVersion,
		Models: cfg.Options.Models, CredentialHeaders: cfg.Options.CredentialHeaders,
		ParameterDefaults: cfg.Options.ParameterDefaults, VendorID: cfg.Options.VendorID,
		Limits: cfg.Options.Limits, Capabilities: []Capability{},
	}
	for _, model := range models {
		for _, capability := range model.Capabilities {
			if capability.Source == "certified" {
				provider.Capabilities = append(provider.Capabilities, Capability{Model: model.UpstreamModel, Operation: capability.Operation, Surface: capability.Surface, Mode: capability.Mode})
			}
		}
	}
	for _, slot := range slots {
		provider.Slots = append(provider.Slots, slot.Slot)
		if slot.Default {
			provider.ActiveCredential = slot.CredentialID
			provider.DefaultSlotID = slot.ID
		}
	}
	return provider, nil
}

// RouteRevision contains scanned metadata and the stored documents of one
// route revision. Target array order is retained; the position field does not
// reorder it.
type RouteRevision struct {
	ID, Slug, RevisionID                                 string
	Revision                                             int
	OverallTimeout                                       int64
	MaxAttempts                                          int
	PublishedAt                                          time.Time
	ProjectID                                            *string
	Operations, Targets, Policy, ContentPolicy, Fidelity []byte
}

// DecodeRouteRevision reconstructs a route without checking ownership,
// operation eligibility, or current publication authority.
func DecodeRouteRevision(revision RouteRevision) (Route, error) {
	route := Route{
		ID: revision.ID, Slug: revision.Slug, RevisionID: revision.RevisionID,
		Revision: revision.Revision, OverallTimeout: revision.OverallTimeout,
		MaxAttempts: revision.MaxAttempts, PublishedAt: revision.PublishedAt.UTC(),
		ProjectID: revision.ProjectID, RoutingID: revision.ID,
	}
	var targets []PublishedTarget
	err := json.Unmarshal(revision.Operations, &route.Operations)
	if err == nil {
		err = json.Unmarshal(revision.Targets, &targets)
	}
	if err != nil {
		return Route{}, fmt.Errorf("route %s revision: %w", revision.Slug, err)
	}
	if len(revision.Policy) > 0 {
		if err = json.Unmarshal(revision.Policy, &route.Policy); err != nil {
			return Route{}, fmt.Errorf("route %s policy: %w", revision.Slug, err)
		}
	}
	if len(revision.ContentPolicy) > 0 {
		if err = json.Unmarshal(revision.ContentPolicy, &route.ContentPolicy); err != nil {
			return Route{}, fmt.Errorf("route %s content policy: %w", revision.Slug, err)
		}
	}
	route.Fidelity, err = DecodeFidelity(revision.Fidelity)
	if err != nil {
		return Route{}, fmt.Errorf("route %s fidelity: %w", revision.Slug, err)
	}
	for _, target := range targets {
		route.Targets = append(route.Targets, Target{ID: target.ID, ProviderID: target.ProviderID, ProviderModel: target.ProviderModel, Priority: target.Priority, Weight: target.Weight, Timeout: target.TimeoutMS, RoutingID: target.ProviderModelID})
	}
	return route, nil
}
