// Package codeadapter is the table of code-mode adapters: the plugin profiles
// each subscription family enrolls through, the client paths and wire
// protocols it serves, the header that carries its upstream credential, and
// the coding clients OLP generates configuration for. A route's pool may mix
// families; each request goes to an account whose family serves its path.
package codeadapter

import (
	"cmp"
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
	// Endpoint, when set, is the one protocol the adapter serves a model on;
	// otherwise each of Protocols serves every model.
	Endpoint func(model string) codemode.Protocol
	// Paths maps each client path under a route to the upstream path that
	// extends the profile's hosting address.
	Paths map[string]string
	// Extra names the upstream authorization headers besides the credential.
	Extra []string
	// Clients lists the clients with a generated configuration, default first.
	Clients []string
}

// vendors is in display order, which orders every list of adapters.
var vendors = []Vendor{
	{
		Adapter: codemode.AdapterCodex, Name: "Codex", Profiles: []string{codexauth.ProfileID}, Manifest: codexauth.Manifest,
		Protocols: []codemode.Protocol{codemode.ProtocolResponses},
		Paths:     map[string]string{"responses": "responses", "responses/compact": "responses/compact"},
		Extra:     []string{"Chatgpt-Account-Id"}, Clients: []string{ClientCodex},
	},
	{
		Adapter: codemode.AdapterOpenCodeGo, Name: "OpenCode Go", Profiles: []string{codeplans.OpenCodeGoProfile}, Manifest: codeplans.OpenCodeGoManifest, KeyGrant: true,
		Protocols: []codemode.Protocol{codemode.ProtocolChat, codemode.ProtocolMessages, codemode.ProtocolResponses}, Endpoint: openCodeGoEndpoint,
		Paths:   map[string]string{"v1/chat/completions": "chat/completions", "v1/messages": "messages", "v1/responses": "responses"},
		Clients: []string{ClientOpenCode, ClientClaudeCode},
	},
	{
		Adapter: codemode.AdapterZAICoding, Name: "GLM Coding Plan", Profiles: []string{codeplans.ZAIProfile, codeplans.BigModelProfile}, Manifest: codeplans.ZAIManifest, KeyGrant: true,
		Protocols: []codemode.Protocol{codemode.ProtocolMessages, codemode.ProtocolChat},
		Paths:     map[string]string{"v1/messages": "anthropic/v1/messages", "v1/chat/completions": "coding/paas/v4/chat/completions"},
		Clients:   []string{ClientClaudeCode, ClientOpenCode},
	},
}

// clientProtocols lists the protocols each client speaks, in the order it
// prefers them: OpenCode sends a model through a provider on the first one the
// provider's adapter serves it on.
var clientProtocols = map[string][]codemode.Protocol{
	ClientCodex:      {codemode.ProtocolResponses},
	ClientClaudeCode: {codemode.ProtocolMessages},
	ClientOpenCode:   {codemode.ProtocolChat, codemode.ProtocolMessages, codemode.ProtocolResponses},
}

// openCodeGoEndpoint is the protocol OpenCode Go serves a model on: Messages
// for MiniMax and Qwen, Responses for GPT and Grok, and Chat Completions for
// the rest.
func openCodeGoEndpoint(model string) codemode.Protocol {
	switch model = strings.ToLower(model); {
	case strings.HasPrefix(model, "minimax"), strings.HasPrefix(model, "qwen"):
		return codemode.ProtocolMessages
	case strings.HasPrefix(model, "gpt"), strings.HasPrefix(model, "grok"):
		return codemode.ProtocolResponses
	}
	return codemode.ProtocolChat
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

// Compare orders adapters as the table does, unknown ones last.
func Compare(a, b codemode.Adapter) int {
	return cmp.Compare(rank(a), rank(b))
}

func rank(adapter codemode.Adapter) int {
	i := slices.IndexFunc(vendors, func(v Vendor) bool { return v.Adapter == adapter })
	if i < 0 {
		return len(vendors)
	}
	return i
}

// Serves reports whether the adapter's routes serve a protocol.
func (v Vendor) Serves(p codemode.Protocol) bool { return slices.Contains(v.Protocols, p) }

// Supports reports whether OLP generates configuration for a client.
func (v Vendor) Supports(client string) bool { return slices.Contains(v.Clients, client) }

// Reaches reports whether a client OLP generates configuration for reaches a
// model through the adapter: the client speaks a protocol the adapter serves
// the model on.
func (v Vendor) Reaches(client, model string) bool {
	_, ok := v.Protocol(client, model)
	return ok
}

// Protocol returns the protocol a client OLP generates configuration for sends
// a model on through the adapter: the first the client speaks that the adapter
// serves the model on.
func (v Vendor) Protocol(client, model string) (codemode.Protocol, bool) {
	if v.Supports(client) {
		for _, p := range clientProtocols[client] {
			if v.ServesModel(p, model) {
				return p, true
			}
		}
	}
	return "", false
}

// ServesModel reports whether the adapter serves a model on a protocol.
func (v Vendor) ServesModel(p codemode.Protocol, model string) bool {
	if v.Endpoint != nil {
		return v.Endpoint(model) == p
	}
	return v.Serves(p)
}

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

// SQLAdapters is a SQL expression for the adapters of a route revision's
// connections, a jsonb object of provider configurations: a jsonb array in
// table order, empty when the revision is absent or names none.
func SQLAdapters(connections string) string {
	order := make([]string, len(vendors))
	for i, v := range vendors {
		order[i] = "'" + string(v.Adapter) + "'"
	}
	return "(SELECT coalesce(jsonb_agg(adapter ORDER BY array_position(ARRAY[" + strings.Join(order, ",") + "],adapter)),'[]'::jsonb) FROM (SELECT DISTINCT " + SQL("c.value") +
		" adapter FROM jsonb_each(" + connections + ") c) adapters WHERE adapter IS NOT NULL)"
}
