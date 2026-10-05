// Package codeadapter is the table of code-mode adapters: the plugin profiles
// each subscription family enrolls through, the wire protocols its routes
// serve, the header that carries its upstream credential, and the coding
// clients OLP generates configuration for.
package codeadapter

import (
	"errors"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// A code account's connection is a plugin provider that authenticates with a
// grant: connectors.KindPlugin and connectors.AuthGrant.
const (
	kindPlugin = "plugin"
	authGrant  = "grant"
)

// Coding clients with a generated configuration.
const (
	ClientCodex      = "codex"
	ClientClaudeCode = "claude-code"
	ClientOpenCode   = "opencode"
)

// Client releases the controlled qualification runs. Codex's is
// codexauth.ClientVersion.
const (
	ClaudeCodeVersion = "2.1.286"
	OpenCodeVersion   = "1.18.34"
)

// Vendor is one adapter's row of the table.
type Vendor struct {
	Adapter  codemode.Adapter
	Name     string
	Profiles []string
	Manifest func() abi.Manifest
	// KeyGrant marks a pasted, non-expiring key whose principal is
	// codeplans.Principal, rather than a refreshed OAuth grant.
	KeyGrant  bool
	Protocols []codemode.Protocol
	// Extra names the upstream authorization headers besides the credential.
	Extra []string
	// Clients lists the clients with a generated configuration, default first.
	Clients []string
}

var vendors = []Vendor{
	{
		Adapter: codemode.AdapterCodex, Name: "Codex", Profiles: []string{codexauth.ProfileID}, Manifest: codexauth.Manifest,
		Protocols: []codemode.Protocol{codemode.ProtocolResponses}, Extra: []string{"Chatgpt-Account-Id"}, Clients: []string{ClientCodex},
	},
	{
		Adapter: codemode.AdapterOpenCodeGo, Name: "OpenCode Go", Profiles: []string{codeplans.OpenCodeGoProfile}, Manifest: codeplans.OpenCodeGoManifest, KeyGrant: true,
		Protocols: []codemode.Protocol{codemode.ProtocolChat, codemode.ProtocolMessages, codemode.ProtocolResponses}, Clients: []string{ClientOpenCode, ClientClaudeCode},
	},
	{
		Adapter: codemode.AdapterZAICoding, Name: "GLM Coding Plan", Profiles: []string{codeplans.ZAIProfile, codeplans.BigModelProfile}, Manifest: codeplans.ZAIManifest, KeyGrant: true,
		Protocols: []codemode.Protocol{codemode.ProtocolMessages, codemode.ProtocolChat}, Clients: []string{ClientClaudeCode, ClientOpenCode},
	},
}

// Lookup returns an adapter's row.
func Lookup(adapter codemode.Adapter) (Vendor, bool) {
	for _, v := range vendors {
		if v.Adapter == adapter {
			return v, true
		}
	}
	return Vendor{}, false
}

// ForConnection returns the row of a provider connection: a plugin profile
// that authenticates with a grant.
func ForConnection(kind, authMode, profileID string) (Vendor, bool) {
	if kind != kindPlugin || authMode != authGrant {
		return Vendor{}, false
	}
	for _, v := range vendors {
		if slices.Contains(v.Profiles, profileID) {
			return v, true
		}
	}
	return Vendor{}, false
}

// Connection is what a provider configuration says about its adapter.
type Connection struct {
	Kind, AuthMode, ProfileID string
}

// ErrMixed reports connections of more than one adapter.
var ErrMixed = errors.New("Use accounts of one subscription family: Codex, OpenCode Go or GLM Coding Plan.")

// Derive returns the one adapter of connections. Connections no adapter
// accepts are ignored, so a set of only those derives no adapter, and serves
// nothing; more than one adapter is ErrMixed.
func Derive(connections []Connection) (codemode.Adapter, error) {
	var adapter codemode.Adapter
	for _, c := range connections {
		v, ok := ForConnection(c.Kind, c.AuthMode, c.ProfileID)
		if !ok {
			continue
		}
		if adapter != "" && adapter != v.Adapter {
			return "", ErrMixed
		}
		adapter = v.Adapter
	}
	return adapter, nil
}

// Serves reports whether the adapter's routes serve a protocol.
func (v Vendor) Serves(p codemode.Protocol) bool { return slices.Contains(v.Protocols, p) }

// Supports reports whether OLP generates configuration for a client.
func (v Vendor) Supports(client string) bool { return slices.Contains(v.Clients, client) }

// Address returns a profile's hosting address, the upstream base its
// requests' paths extend.
func (v Vendor) Address(profileID string) (string, bool) {
	for _, p := range v.Manifest().Profiles {
		if p.ID == profileID && slices.Contains(v.Profiles, profileID) {
			return p.Hosting.Address, true
		}
	}
	return "", false
}

// CredentialHeader names the header that carries the upstream credential:
// X-Api-Key for OpenCode Go's Anthropic Messages endpoint, as OpenCode's own
// Anthropic SDK sends it, and a bearer Authorization otherwise.
func CredentialHeader(adapter codemode.Adapter, protocol codemode.Protocol) string {
	if adapter == codemode.AdapterOpenCodeGo && protocol == codemode.ProtocolMessages {
		return "X-Api-Key"
	}
	return "Authorization"
}

// SQL is a SQL expression for the adapter of the provider configuration
// config, a jsonb expression, or NULL when no adapter accepts it.
func SQL(config string) string {
	var b strings.Builder
	b.WriteString("CASE WHEN " + config + "->>'kind'='" + kindPlugin + "' AND " + config + "->>'auth_mode'='" + authGrant + "' THEN CASE " + config + "->>'profile_id'")
	for _, v := range vendors {
		for _, profile := range v.Profiles {
			b.WriteString(" WHEN '" + profile + "' THEN '" + string(v.Adapter) + "'")
		}
	}
	b.WriteString(" END END")
	return b.String()
}

// SQLRevision is a SQL expression for the adapter Derive returns for a route
// revision's connections, a jsonb object of provider configurations, or NULL
// when they name none or several.
func SQLRevision(connections string) string {
	return "(SELECT CASE WHEN count(DISTINCT adapter)=1 THEN min(adapter) END FROM (SELECT " + SQL("c.value") + " adapter FROM jsonb_each(" + connections + ") c) adapters)"
}
