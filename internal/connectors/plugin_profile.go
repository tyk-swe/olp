package connectors

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/plugin/abi"
)

const pluginHosting = "plugin"

var pluginDialects = []string{"openai-chat", "openai-responses", "anthropic-messages", "gemini-generate-content"}

func PluginDialects() []string { return slices.Clone(pluginDialects) }

type Plugin struct {
	Digest     string `json:"digest"`
	Name       string `json:"name"`
	Version    string `json:"version"`
	Unconfined bool   `json:"unconfined,omitempty"`
}

type PluginProfile struct {
	profile  Profile
	hosting  hosting
	declared abi.Profile
	origins  []string
	patterns map[string]*regexp.Regexp
}

func NewPluginProfile(digest string, manifest abi.Manifest, id string) (*PluginProfile, error) {
	return declaredProfile(Plugin{Digest: digest}, manifest, id)
}

func NewUnconfinedPluginProfile(digest string, manifest abi.Manifest, id string) (*PluginProfile, error) {
	return declaredProfile(Plugin{Digest: digest, Unconfined: true}, manifest, id)
}

func declaredProfile(plugin Plugin, manifest abi.Manifest, id string) (*PluginProfile, error) {
	plugin.Name, plugin.Version = manifest.Name, manifest.Version
	for _, declared := range manifest.Profiles {
		if declared.ID == id {
			return newPluginProfile(plugin, manifest.Origins, declared)
		}
	}
	return nil, fmt.Errorf("plugin %s declares no profile %q", manifest.Name, id)
}

type InstalledPlugin struct {
	Manifest   abi.Manifest `json:"manifest"`
	Unconfined bool         `json:"unconfined"`
}

func DecodePluginProfile(digest string, installed []byte, id string) (*PluginProfile, error) {
	if installed == nil {
		return nil, fmt.Errorf("plugin %s is not installed", digest)
	}
	var plugin InstalledPlugin
	if err := json.Unmarshal(installed, &plugin); err != nil {
		return nil, err
	}
	return declaredProfile(Plugin{Digest: digest, Unconfined: plugin.Unconfined}, plugin.Manifest, id)
}

func (p *PluginProfile) Unconfined() bool { return p.profile.Plugin.Unconfined }

func ValidatePluginProfile(declared abi.Profile) error {
	_, err := newPluginProfile(Plugin{}, nil, declared)
	return err
}

type ProfileError struct {
	Field   string
	Message string
}

func (e *ProfileError) Error() string { return e.Field + ": " + e.Message }

func newPluginProfile(plugin Plugin, origins []string, declared abi.Profile) (*PluginProfile, error) {
	// OLP keys some behaviour on built-in profile IDs, such as Gemini
	// Interactions' own contract, which must never reach a plugin's profile.
	if slices.ContainsFunc(builtIns, func(p Profile) bool { return p.ID == declared.ID }) {
		return nil, &ProfileError{Field: "id", Message: fmt.Sprintf("Identify the profile with an ID of its own: %s is a built-in profile's.", declared.ID)}
	}
	base, ok := dialectProfile(declared.Dialect)
	if !ok {
		return nil, &ProfileError{Field: "dialect", Message: "Serve one of the dialects plugin profiles can serve: " + strings.Join(pluginDialects, ", ") + "."}
	}
	patterns, err := parseOptions(declared.Options)
	if err != nil {
		return nil, err
	}
	if err = parseGrant(declared.Grant); err != nil {
		return nil, err
	}
	placed, err := parseHosting(declared, base)
	if err != nil {
		return nil, err
	}
	authentication := AuthStaticCredential
	if declared.Grant != nil {
		authentication = AuthGrant
	}
	transport := "http"
	if declared.CarriesTraffic {
		transport = pluginTransport
	}
	p := Profile{
		ID: declared.ID, Revision: plugin.Digest, Label: declared.Label, Kind: KindPlugin,
		Dialect: declared.Dialect, DialectRevision: base.DialectRevision, Hosting: pluginHosting,
		Authentication: []AuthMode{authentication}, Transport: transport, Operations: []string{"generation"},
		SemanticHeaders: slices.Clone(base.SemanticHeaders), QuerySettings: slices.Clone(base.QuerySettings),
		Plugin: &plugin, ModelDiscovery: declared.Hosting.Discovery != nil, OptionsSchema: optionsSchema(declared.Options),
		Strict: placed.envelope == nil && len(placed.rewrites) == 0 && !placed.forceStreaming && !declared.CarriesTraffic,
	}
	return &PluginProfile{profile: withOperationMetadata(p), hosting: placed, declared: declared, origins: slices.Clone(origins), patterns: patterns}, nil
}

func dialectProfile(dialect string) (Profile, bool) {
	if !slices.Contains(pluginDialects, dialect) {
		return Profile{}, false
	}
	for _, p := range builtIns {
		if p.Dialect == dialect {
			return p, true
		}
	}
	return Profile{}, false
}

func (p *PluginProfile) Profile() Profile { return cloneProfile(p.profile) }

type pluginProfileJSON struct {
	Plugin  Plugin      `json:"plugin"`
	Origins []string    `json:"origins"`
	Profile abi.Profile `json:"profile"`
}

func (p *PluginProfile) MarshalJSON() ([]byte, error) {
	return json.Marshal(pluginProfileJSON{Plugin: *p.profile.Plugin, Origins: p.origins, Profile: p.declared})
}

func (p *PluginProfile) UnmarshalJSON(data []byte) error {
	var stored pluginProfileJSON
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	decoded, err := newPluginProfile(stored.Plugin, stored.Origins, stored.Profile)
	if err != nil {
		return err
	}
	*p = *decoded
	return nil
}
