// Package codeadapter is the table of code-mode adapters: the plugin profiles
// each subscription family enrolls through, the client paths and wire
// protocols it serves, the header that carries its upstream credential, and
// the coding clients OLP generates configuration for. A route's pool may mix
// families; each request goes to an account whose family serves its path.
package codeadapter

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

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

// Client channels the controlled qualification resolves on every run. Exact
// versions belong to CI evidence. Codex's channel is codexauth.ClientVersion.
const (
	ClaudeCodeVersion = "latest"
	OpenCodeVersion   = "latest"
)

// Transport is a forwarding transport supported by an endpoint.
type Transport uint8

const (
	HTTP Transport = 1 << iota
	WebSocket
)

// IdentityPolicy selects the existing wire parser's conversation identity.
type IdentityPolicy string

const (
	IdentityCodex  IdentityPolicy = "codex"
	IdentityClient IdentityPolicy = "client"
)

// ObservationPolicy selects subscription observations in addition to usage.
type ObservationPolicy string

const (
	ObserveUsage ObservationPolicy = "usage"
	ObserveCodex ObservationPolicy = "codex_allowance"
)

// Endpoint describes one native operation. Operation names the wire parser's
// operation, independently of either the client or upstream URL.
type Endpoint struct {
	ClientPath, UpstreamPath string
	Protocol                 codemode.Protocol
	Operation                string
	Identity                 IdentityPolicy
	CredentialHeader         string
	Observation              ObservationPolicy
	transports               Transport
}

func (e Endpoint) Supports(transport Transport) bool {
	return transport != 0 && e.transports&transport == transport
}

// Client contains the adapter's settings for a generated client configuration.
type Client struct {
	ID, Provider, URLSuffix string
}

// Vendor is a static adapter declaration. Collections stay private; accessors
// return copies so callers cannot change the registry.
type Vendor struct {
	Adapter codemode.Adapter
	Name    string
	// KeyGrant marks a pasted, non-expiring key rather than a refreshed grant.
	KeyGrant               bool
	profiles               []string
	manifest               func() abi.Manifest
	endpoints              []Endpoint
	modelProtocol          func(model string) codemode.Protocol
	credential             func([]byte, string, string, Endpoint, time.Time) (codemode.Authorization, error)
	extra                  []string
	clients                []Client
	enrollmentNotes, notes []string
	clientNotes            map[string][]string
}

var keyEnrollmentNotes = []string{
	"Enrollment stores the pasted coding-plan key and identifies it by fingerprint; it qualifies no inference, entitlement or allowance.",
	"Hard token budgets require a separately proven operation/model bound; this adapter supplies none.",
}

// vendors is in display order, which orders every list of adapters.
var vendors = []Vendor{
	{
		Adapter: codemode.AdapterCodex, Name: "Codex", profiles: []string{codexauth.ProfileID}, manifest: codexauth.Manifest,
		credential: codexCredential,
		endpoints: []Endpoint{
			{ClientPath: "responses", UpstreamPath: "responses", Protocol: codemode.ProtocolResponses, Operation: "responses", Identity: IdentityCodex, CredentialHeader: "Authorization", Observation: ObserveCodex, transports: HTTP | WebSocket},
			{ClientPath: "responses/compact", UpstreamPath: "responses/compact", Protocol: codemode.ProtocolResponses, Operation: "responses/compact", Identity: IdentityCodex, CredentialHeader: "Authorization", Observation: ObserveCodex, transports: HTTP},
		},
		extra: []string{"Chatgpt-Account-Id"}, clients: []Client{{ID: ClientCodex}},
		enrollmentNotes: []string{
			"Enrollment validates authorization only; live subscription inference and client workflow qualification must be established separately.",
			"Hard token budgets require a separately proven operation/model bound; this authentication adapter supplies none.",
			"ChatGPT cloud tasks, standalone web search, account management and FedRAMP accounts are outside this local code-mode configuration.",
		},
	},
	{
		Adapter: codemode.AdapterOpenCodeGo, Name: "OpenCode Go", profiles: []string{codeplans.OpenCodeGoProfile}, manifest: codeplans.OpenCodeGoManifest, KeyGrant: true,
		credential: keyCredential, modelProtocol: openCodeGoEndpoint,
		endpoints: []Endpoint{
			{ClientPath: "v1/chat/completions", UpstreamPath: "chat/completions", Protocol: codemode.ProtocolChat, Operation: "chat/completions", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: HTTP},
			{ClientPath: "v1/messages", UpstreamPath: "messages", Protocol: codemode.ProtocolMessages, Operation: "messages", Identity: IdentityClient, CredentialHeader: "X-Api-Key", Observation: ObserveUsage, transports: HTTP},
			{ClientPath: "v1/responses", UpstreamPath: "responses", Protocol: codemode.ProtocolResponses, Operation: "responses", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: HTTP},
		},
		clients:         []Client{{ID: ClientOpenCode, Provider: "opencode-go", URLSuffix: "/v1"}, {ID: ClientClaudeCode}},
		enrollmentNotes: keyEnrollmentNotes,
		notes:           []string{"OpenCode Go's terms govern its use; OLP forwards this client's requests unmodified but cannot guarantee that OpenCode accepts pooled use."},
		clientNotes:     map[string][]string{ClientClaudeCode: {"OpenCode Go serves Anthropic Messages only for some models, such as MiniMax and Qwen, so Claude Code is offered only those."}},
	},
	{
		Adapter: codemode.AdapterZAICoding, Name: "GLM Coding Plan", profiles: []string{codeplans.ZAIProfile, codeplans.BigModelProfile}, manifest: codeplans.ZAIManifest, KeyGrant: true,
		credential: keyCredential,
		endpoints: []Endpoint{
			{ClientPath: "v1/messages", UpstreamPath: "anthropic/v1/messages", Protocol: codemode.ProtocolMessages, Operation: "messages", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: HTTP},
			{ClientPath: "v1/chat/completions", UpstreamPath: "coding/paas/v4/chat/completions", Protocol: codemode.ProtocolChat, Operation: "chat/completions", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: HTTP},
		},
		clients:         []Client{{ID: ClientClaudeCode}, {ID: ClientOpenCode, Provider: "zai-coding-plan", URLSuffix: "/v1"}},
		enrollmentNotes: keyEnrollmentNotes,
		notes:           []string{"Z.ai restricts the GLM Coding Plan to its officially supported coding tools; OLP forwards this client's requests unmodified but cannot guarantee that Z.ai accepts pooled use."},
		clientNotes:     map[string][]string{ClientOpenCode: {"Research services require an explicit route research binding to a Z.ai global account. Vision is not supported."}},
	},
	{
		Adapter: codemode.AdapterMiniMaxPlan, Name: "MiniMax M Plan", profiles: []string{codeplans.MiniMaxProfile, codeplans.MiniMaxCNProfile}, manifest: codeplans.MiniMaxManifest, KeyGrant: true,
		credential: keyCredential,
		endpoints: []Endpoint{
			{ClientPath: "v1/messages", UpstreamPath: "anthropic/v1/messages", Protocol: codemode.ProtocolMessages, Operation: "messages", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: HTTP},
			{ClientPath: "v1/chat/completions", UpstreamPath: "v1/chat/completions", Protocol: codemode.ProtocolChat, Operation: "chat/completions", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: HTTP},
		},
		clients:         []Client{{ID: ClientClaudeCode}, {ID: ClientOpenCode, Provider: "minimax-plan", URLSuffix: "/v1"}},
		enrollmentNotes: keyEnrollmentNotes,
		notes:           []string{"MiniMax's terms govern M Plan use; OLP forwards this client's requests unmodified but cannot guarantee that MiniMax accepts pooled use."},
	},
	{
		Adapter: codemode.AdapterKimiCode, Name: "Kimi Code", profiles: []string{codeplans.KimiProfile, codeplans.KimiCNProfile}, manifest: codeplans.KimiManifest, KeyGrant: true,
		credential: keyCredential,
		endpoints: []Endpoint{
			{ClientPath: "v1/messages", UpstreamPath: "v1/messages", Protocol: codemode.ProtocolMessages, Operation: "messages", Identity: IdentityClient, CredentialHeader: "X-Api-Key", Observation: ObserveUsage, transports: HTTP},
			{ClientPath: "v1/chat/completions", UpstreamPath: "v1/chat/completions", Protocol: codemode.ProtocolChat, Operation: "chat/completions", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: HTTP},
		},
		clients:         []Client{{ID: ClientClaudeCode}, {ID: ClientOpenCode, Provider: "kimi-code", URLSuffix: "/v1"}},
		enrollmentNotes: keyEnrollmentNotes,
		notes:           []string{"Kimi's terms forbid altering the client's identity; OLP forwards this client's own User-Agent and requests unmodified but cannot guarantee that Kimi accepts pooled use."},
	},
	{
		Adapter: codemode.AdapterQwenTokenPlan, Name: "Qwen Token Plan", profiles: []string{codeplans.QwenTokenPlanProfile, codeplans.QwenTokenPlanCNProfile}, manifest: codeplans.QwenTokenPlanManifest, KeyGrant: true,
		credential: keyCredential,
		endpoints: []Endpoint{
			{ClientPath: "v1/messages", UpstreamPath: "apps/anthropic/v1/messages", Protocol: codemode.ProtocolMessages, Operation: "messages", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: HTTP},
			{ClientPath: "v1/chat/completions", UpstreamPath: "compatible-mode/v1/chat/completions", Protocol: codemode.ProtocolChat, Operation: "chat/completions", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: HTTP},
		},
		clients:         []Client{{ID: ClientClaudeCode}, {ID: ClientOpenCode, Provider: "qwen-token-plan", URLSuffix: "/v1"}},
		enrollmentNotes: keyEnrollmentNotes,
		notes:           []string{"Alibaba's terms govern Token Plan use; OLP forwards this client's requests unmodified but cannot guarantee that Alibaba accepts pooled use."},
	},
}

func Validate() error { return validate(vendors) }

func validate(registry []Vendor) error {
	paths := map[string]Endpoint{}
	profiles := map[string]bool{}
	adapters := map[codemode.Adapter]bool{}
	for _, v := range registry {
		if v.Adapter == "" || adapters[v.Adapter] || v.credential == nil || v.manifest == nil || len(v.endpoints) == 0 {
			return fmt.Errorf("invalid code adapter %q", v.Adapter)
		}
		adapters[v.Adapter] = true
		for _, profile := range v.profiles {
			if _, ok := v.Address(profile); !ok || profiles[profile] {
				return fmt.Errorf("invalid code profile %q", profile)
			}
			profiles[profile] = true
		}
		seen := map[string]bool{}
		for _, e := range v.endpoints {
			if seen[e.ClientPath] || !e.valid() {
				return fmt.Errorf("invalid code endpoint %s/%s", v.Adapter, e.ClientPath)
			}
			if prior, ok := paths[e.ClientPath]; ok && (prior.Protocol != e.Protocol || prior.Identity != e.Identity || prior.Operation != e.Operation) {
				return fmt.Errorf("inconsistent code request semantics for %s", e.ClientPath)
			}
			seen[e.ClientPath], paths[e.ClientPath] = true, e
		}
	}
	return nil
}

func (e Endpoint) valid() bool {
	if e.ClientPath == "" || e.UpstreamPath == "" || e.CredentialHeader == "" || e.transports == 0 || e.transports & ^(HTTP|WebSocket) != 0 {
		return false
	}
	if e.Identity != IdentityClient && e.Identity != IdentityCodex || e.Identity == IdentityCodex && e.Protocol != codemode.ProtocolResponses {
		return false
	}
	if e.Observation != ObserveUsage && e.Observation != ObserveCodex || e.Observation == ObserveCodex && e.Protocol != codemode.ProtocolResponses {
		return false
	}
	if e.Supports(WebSocket) && (e.Protocol != codemode.ProtocolResponses || e.Operation != "responses") {
		return false
	}
	switch e.Protocol {
	case codemode.ProtocolResponses:
		return e.Operation == "responses" || e.Operation == "responses/compact"
	case codemode.ProtocolMessages:
		return e.Operation == "messages"
	case codemode.ProtocolChat:
		return e.Operation == "chat/completions"
	}
	return false
}

// ForPath returns the shared request semantics of a client path. Account
// selection must resolve the selected vendor's endpoint for forwarding.
func ForPath(path string) (Endpoint, bool) {
	for _, v := range vendors {
		if e, ok := v.Endpoint(path); ok {
			return e, true
		}
	}
	return Endpoint{}, false
}

func (v Vendor) Manifest() abi.Manifest { return v.manifest() }
func (v Vendor) Endpoints() []Endpoint  { return slices.Clone(v.endpoints) }
func (v Vendor) Endpoint(path string) (Endpoint, bool) {
	for _, e := range v.endpoints {
		if e.ClientPath == path {
			return e, true
		}
	}
	return Endpoint{}, false
}
func (v Vendor) Clients() []string {
	clients := make([]string, len(v.clients))
	for i, c := range v.clients {
		clients[i] = c.ID
	}
	return clients
}
func (v Vendor) Client(id string) (Client, bool) {
	for _, c := range v.clients {
		if c.ID == id {
			return c, true
		}
	}
	return Client{}, false
}
func (v Vendor) EnrollmentNotes() []string          { return slices.Clone(v.enrollmentNotes) }
func (v Vendor) Notes() []string                    { return slices.Clone(v.notes) }
func (v Vendor) ClientNotes(client string) []string { return slices.Clone(v.clientNotes[client]) }

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
		if slices.Contains(v.profiles, profileID) {
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

func (v Vendor) Serves(p codemode.Protocol) bool {
	return slices.ContainsFunc(v.endpoints, func(e Endpoint) bool { return e.Protocol == p })
}

func (v Vendor) Supports(client string) bool { _, ok := v.Client(client); return ok }

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

func (v Vendor) ServesModel(p codemode.Protocol, model string) bool {
	if !v.Serves(p) {
		return false
	}
	if v.modelProtocol != nil {
		return v.modelProtocol(model) == p
	}
	return true
}

// Address returns a profile's hosting address, the upstream base its
// requests' paths extend.
func (v Vendor) Address(profileID string) (string, bool) {
	for _, p := range v.Manifest().Profiles {
		if p.ID == profileID && slices.Contains(v.profiles, profileID) {
			return p.Hosting.Address, true
		}
	}
	return "", false
}

// SQL is a SQL expression for the adapter of the provider configuration
// config, a jsonb expression, or NULL when no adapter accepts it.
func SQL(config string) string {
	var b strings.Builder
	b.WriteString("CASE WHEN " + config + "->>'kind'='" + kindPlugin + "' AND " + config + "->>'auth_mode'='" + authGrant + "' THEN CASE " + config + "->>'profile_id'")
	for _, v := range vendors {
		for _, profile := range v.profiles {
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
