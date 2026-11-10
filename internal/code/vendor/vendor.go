package vendor

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/antigravityauth"
	"github.com/tyk-swe/olp/internal/claudeauth"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/plugin/abi"
)

const (
	kindPlugin = "plugin"
	authGrant  = "grant"

	ClaudeCodeVersion = "latest"
	OpenCodeVersion   = "latest"
)

type IdentityPolicy string

const (
	IdentityCodex  IdentityPolicy = "codex"
	IdentityClient IdentityPolicy = "client"
)

type ObservationPolicy string

const (
	ObserveUsage ObservationPolicy = "usage"
	ObserveCodex ObservationPolicy = "codex_allowance"
)

type Target string

const TargetAntigravity Target = "antigravity"

func (t Target) Enabled(route code.Route) bool {
	return t == "" || t == TargetAntigravity && route.ChatToAntigravity
}

type Endpoint struct {
	Target                   Target
	ClientPath, UpstreamPath string
	Protocol                 code.Protocol
	Operation                string
	Identity                 IdentityPolicy
	CredentialHeader         string
	Observation              ObservationPolicy
	transports               code.Transport
}

func (e Endpoint) Supports(transport code.Transport) bool {
	return transport != 0 && e.transports&transport == transport
}

func (e Endpoint) Path(stream bool) string {
	if e.Target == TargetAntigravity && stream {
		return "v1internal:streamGenerateContent"
	}
	return e.UpstreamPath
}

type Client struct {
	ID, Provider, URLSuffix string
	Protocol                code.Protocol
}

type Vendor struct {
	Adapter                 code.Adapter
	Name                    string
	KeyGrant                bool
	ExtraUsageAllowed       bool
	SupportsAllowanceCharge bool
	cycleAnchorProfiles     []string
	researchProfiles        []string
	profiles                []string
	manifest                func() abi.Manifest
	endpoints               []Endpoint
	modelProtocol           func(model string) code.Protocol
	credential              func([]byte, string, string, Endpoint, time.Time) (code.Authorization, error)
	extra                   []string
	clients                 []Client
	enrollmentNotes, notes  []string
	clientNotes             map[string][]string
}

var keyEnrollmentNotes = []string{
	"Enrollment stores the pasted coding-plan key and identifies it by fingerprint; it qualifies no inference, entitlement or allowance.",
	"Hard token budgets require a separately proven operation/model bound; this adapter supplies none.",
}

var vendors = []Vendor{
	{
		Adapter: code.AdapterCodex, Name: "Codex", profiles: []string{codexauth.ProfileID}, manifest: codexauth.Manifest,
		credential: codexCredential,
		endpoints: []Endpoint{
			{ClientPath: "responses", UpstreamPath: "responses", Protocol: code.ProtocolResponses, Operation: "responses", Identity: IdentityCodex, CredentialHeader: "Authorization", Observation: ObserveCodex, transports: code.HTTP | code.WebSocket},
			{ClientPath: "responses/compact", UpstreamPath: "responses/compact", Protocol: code.ProtocolResponses, Operation: "responses/compact", Identity: IdentityCodex, CredentialHeader: "Authorization", Observation: ObserveCodex, transports: code.HTTP},
		},
		extra: []string{"Chatgpt-Account-Id"}, clients: []Client{{ID: code.ClientCodex}},
		enrollmentNotes: []string{
			"Enrollment validates authorization only; live subscription inference and client workflow qualification must be established separately.",
			"Hard token budgets require a separately proven operation/model bound; this authentication adapter supplies none.",
			"ChatGPT cloud tasks, standalone web search, account management and FedRAMP accounts are outside this local code-mode configuration.",
		},
	},
	{
		Adapter: code.AdapterClaude, Name: "Claude", profiles: []string{claudeauth.ProfileID}, manifest: claudeauth.Manifest,
		credential: claudeCredential, extra: []string{"Anthropic-Beta"},
		endpoints: []Endpoint{{ClientPath: "v1/messages", UpstreamPath: "v1/messages", Protocol: code.ProtocolMessages, Operation: "messages", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: code.HTTP}},
		clients:   []Client{{ID: code.ClientClaudeCode}},
		enrollmentNotes: []string{
			"Enrollment verifies the account and organization through the OAuth profile endpoint. It does not qualify inference, entitlement or allowance.",
			"Claude subscription forwarding and native tool workflows are fixture-tested only. Live subscription inference is not qualified.",
			"Hard token budgets require a separately proven operation/model bound; this authentication adapter supplies none.",
		},
	},
	{
		Adapter: code.AdapterAntigravity, Name: "Antigravity", profiles: []string{antigravityauth.ProfileID}, manifest: antigravityauth.Manifest,
		credential: antigravityCredential, modelProtocol: antigravityModelProtocol,
		endpoints: []Endpoint{{ClientPath: "v1/chat/completions", UpstreamPath: "v1internal:generateContent", Protocol: code.ProtocolChat, Operation: "chat/completions", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: code.HTTP, Target: TargetAntigravity}},
		clients:   []Client{{ID: code.ClientOpenCode, Provider: "olp-antigravity", URLSuffix: "/v1", Protocol: code.ProtocolChat}},
		enrollmentNotes: []string{
			"Supply an authorized Google desktop OAuth client ID as the provider option. Complete masked enrollment with callback_url and client_secret JSON. Client secrets and refresh tokens remain encrypted.",
			"Enrollment verifies the Google subject and an existing cloudcode project. It does not onboard accounts or establish live inference, entitlement, allowance or hard token bounds.",
		},
		notes: []string{"Explicit chat_to_antigravity policy enables controlled Gemini text and function tools over HTTP or SSE. Signed reasoning, media and server tools are unsupported. Client identity and prompt compatibility remain disabled. Live subscription and OpenCode workflow qualification remain pending."},
	},
	{
		Adapter: code.AdapterOpenCodeGo, Name: "OpenCode Go", profiles: []string{codeplans.OpenCodeGoProfile}, manifest: codeplans.OpenCodeGoManifest,
		KeyGrant: true, SupportsAllowanceCharge: true,
		credential: keyCredential, modelProtocol: openCodeGoEndpoint,
		endpoints: []Endpoint{
			{ClientPath: "v1/chat/completions", UpstreamPath: "chat/completions", Protocol: code.ProtocolChat, Operation: "chat/completions", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: code.HTTP},
			{ClientPath: "v1/messages", UpstreamPath: "messages", Protocol: code.ProtocolMessages, Operation: "messages", Identity: IdentityClient, CredentialHeader: "X-Api-Key", Observation: ObserveUsage, transports: code.HTTP},
			{ClientPath: "v1/responses", UpstreamPath: "responses", Protocol: code.ProtocolResponses, Operation: "responses", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: code.HTTP},
		},
		clients:         []Client{{ID: code.ClientOpenCode, Provider: "opencode-go", URLSuffix: "/v1"}, {ID: code.ClientClaudeCode}},
		enrollmentNotes: keyEnrollmentNotes,
		notes:           []string{"OpenCode Go's terms govern its use; OLP forwards this client's requests unmodified but cannot guarantee that OpenCode accepts pooled use."},
		clientNotes:     map[string][]string{code.ClientClaudeCode: {"OpenCode Go serves Anthropic Messages only for some models, such as MiniMax and Qwen, so Claude Code is offered only those."}},
	},
	{
		Adapter: code.AdapterZAICoding, Name: "GLM Coding Plan", profiles: []string{codeplans.ZAIProfile, codeplans.BigModelProfile}, manifest: codeplans.ZAIManifest,
		KeyGrant: true, researchProfiles: []string{codeplans.ZAIProfile},
		credential: keyCredential,
		endpoints: []Endpoint{
			{ClientPath: "v1/messages", UpstreamPath: "anthropic/v1/messages", Protocol: code.ProtocolMessages, Operation: "messages", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: code.HTTP},
			{ClientPath: "v1/chat/completions", UpstreamPath: "coding/paas/v4/chat/completions", Protocol: code.ProtocolChat, Operation: "chat/completions", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: code.HTTP},
		},
		clients:         []Client{{ID: code.ClientClaudeCode}, {ID: code.ClientOpenCode, Provider: "zai-coding-plan", URLSuffix: "/v1"}},
		enrollmentNotes: keyEnrollmentNotes,
		notes:           []string{"Z.ai restricts the GLM Coding Plan to its officially supported coding tools; OLP forwards this client's requests unmodified but cannot guarantee that Z.ai accepts pooled use.", "Ordering runs on weights plus cooldown; this adapter has no qualified allowance signal."},
		clientNotes:     map[string][]string{code.ClientOpenCode: {"Research services require an explicit route research binding to a Z.ai global account. Vision is not supported."}},
	},
	{
		Adapter: code.AdapterMiniMaxPlan, Name: "MiniMax M Plan", profiles: []string{codeplans.MiniMaxProfile, codeplans.MiniMaxCNProfile}, manifest: codeplans.MiniMaxManifest,
		KeyGrant: true, credential: keyCredential,
		endpoints: []Endpoint{
			{ClientPath: "v1/messages", UpstreamPath: "anthropic/v1/messages", Protocol: code.ProtocolMessages, Operation: "messages", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: code.HTTP},
			{ClientPath: "v1/chat/completions", UpstreamPath: "v1/chat/completions", Protocol: code.ProtocolChat, Operation: "chat/completions", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: code.HTTP},
		},
		clients:         []Client{{ID: code.ClientClaudeCode}, {ID: code.ClientOpenCode, Provider: "minimax-coding-plan", URLSuffix: "/v1", Protocol: code.ProtocolMessages}},
		enrollmentNotes: keyEnrollmentNotes,
		notes:           []string{"MiniMax's terms govern M Plan use; OLP forwards this client's requests unmodified but cannot guarantee that MiniMax accepts pooled use.", "Allowance bands can order new roots from active remains, fixture-proven only. That schema is pinned from the vendor client."},
	},
	{
		Adapter: code.AdapterKimiCode, Name: "Kimi Code", profiles: []string{codeplans.KimiProfile, codeplans.KimiCNProfile}, manifest: codeplans.KimiManifest,
		KeyGrant: true, ExtraUsageAllowed: true, credential: keyCredential,
		endpoints: []Endpoint{
			{ClientPath: "v1/messages", UpstreamPath: "v1/messages", Protocol: code.ProtocolMessages, Operation: "messages", Identity: IdentityClient, CredentialHeader: "X-Api-Key", Observation: ObserveUsage, transports: code.HTTP},
			{ClientPath: "v1/chat/completions", UpstreamPath: "v1/chat/completions", Protocol: code.ProtocolChat, Operation: "chat/completions", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: code.HTTP},
		},
		clients:         []Client{{ID: code.ClientClaudeCode}, {ID: code.ClientOpenCode, Provider: "kimi-code-plan-global", URLSuffix: "/v1"}},
		enrollmentNotes: keyEnrollmentNotes,
		notes:           []string{"Kimi's terms forbid altering the client's identity; OLP forwards this client's own User-Agent and requests unmodified but cannot guarantee that Kimi accepts pooled use."},
	},
	{
		Adapter: code.AdapterQwenTokenPlan, Name: "Qwen Token Plan", profiles: []string{codeplans.QwenTokenPlanProfile, codeplans.QwenTokenPlanCNProfile}, manifest: codeplans.QwenTokenPlanManifest,
		KeyGrant: true, cycleAnchorProfiles: []string{codeplans.QwenTokenPlanProfile, codeplans.QwenTokenPlanCNProfile},
		credential: keyCredential,
		endpoints: []Endpoint{
			{ClientPath: "v1/messages", UpstreamPath: "apps/anthropic/v1/messages", Protocol: code.ProtocolMessages, Operation: "messages", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: code.HTTP},
			{ClientPath: "v1/chat/completions", UpstreamPath: "compatible-mode/v1/chat/completions", Protocol: code.ProtocolChat, Operation: "chat/completions", Identity: IdentityClient, CredentialHeader: "Authorization", Observation: ObserveUsage, transports: code.HTTP},
		},
		clients:         []Client{{ID: code.ClientClaudeCode}, {ID: code.ClientOpenCode, Provider: "alibaba-token-plan", URLSuffix: "/v1"}},
		enrollmentNotes: keyEnrollmentNotes,
		notes:           []string{"Alibaba's terms govern Token Plan use; OLP forwards this client's requests unmodified but cannot guarantee that Alibaba accepts pooled use."},
	},
}

func All() []Vendor { return slices.Clone(vendors) }

func Validate() error { return validate(vendors) }

func validate(registry []Vendor) error {
	paths := map[string]Endpoint{}
	profiles := map[string]bool{}
	adapters := map[code.Adapter]bool{}
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
	if e.Target != "" && (e.Target != TargetAntigravity || e.Protocol != code.ProtocolChat || e.transports != code.HTTP) {
		return false
	}
	if e.ClientPath == "" || e.UpstreamPath == "" || e.CredentialHeader == "" || e.transports == 0 || e.transports & ^(code.HTTP|code.WebSocket) != 0 {
		return false
	}
	if e.Identity != IdentityClient && e.Identity != IdentityCodex || e.Identity == IdentityCodex && e.Protocol != code.ProtocolResponses {
		return false
	}
	if e.Observation != ObserveUsage && e.Observation != ObserveCodex || e.Observation == ObserveCodex && e.Protocol != code.ProtocolResponses {
		return false
	}
	if e.Supports(code.WebSocket) && (e.Protocol != code.ProtocolResponses || e.Operation != "responses") {
		return false
	}
	switch e.Protocol {
	case code.ProtocolResponses:
		return e.Operation == "responses" || e.Operation == "responses/compact"
	case code.ProtocolMessages:
		return e.Operation == "messages"
	case code.ProtocolChat:
		return e.Operation == "chat/completions"
	}
	return false
}

func ForPath(path string) (Endpoint, bool) {
	for _, v := range vendors {
		if e, ok := v.Endpoint(path); ok {
			return e, true
		}
	}
	return Endpoint{}, false
}

func (v Vendor) Manifest() abi.Manifest { return v.manifest() }
func (v Vendor) Profiles() []string     { return slices.Clone(v.profiles) }
func (v Vendor) SupportsCycleAnchor(id string) bool {
	return slices.Contains(v.cycleAnchorProfiles, id)
}
func (v Vendor) SupportsResearch(id string) bool    { return slices.Contains(v.researchProfiles, id) }
func (v Vendor) EnrollmentNotes() []string          { return slices.Clone(v.enrollmentNotes) }
func (v Vendor) Notes() []string                    { return slices.Clone(v.notes) }
func (v Vendor) ClientNotes(client string) []string { return slices.Clone(v.clientNotes[client]) }
func (v Vendor) Endpoints() []Endpoint              { return slices.Clone(v.endpoints) }
func (v Vendor) Supports(client string) bool        { _, ok := v.Client(client); return ok }

func (v Vendor) CompatibilityOriginator(route code.Route) string {
	if v.Adapter != code.AdapterCodex || !route.CodexCLIIdentity {
		return ""
	}
	return "codex_cli_rs"
}

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

var clientProtocols = map[string][]code.Protocol{
	code.ClientCodex:      {code.ProtocolResponses},
	code.ClientClaudeCode: {code.ProtocolMessages},
	code.ClientOpenCode:   {code.ProtocolChat, code.ProtocolMessages, code.ProtocolResponses},
}

func openCodeGoEndpoint(model string) code.Protocol {
	lower := strings.ToLower(model)
	switch {
	case strings.HasPrefix(lower, "minimax"), strings.HasPrefix(lower, "qwen"):
		return code.ProtocolMessages
	case strings.HasPrefix(lower, "gpt"), strings.HasPrefix(lower, "grok"):
		return code.ProtocolResponses
	}
	return code.ProtocolChat
}

func antigravityModelProtocol(model string) code.Protocol {
	if strings.HasPrefix(model, "gemini-") {
		return code.ProtocolChat
	}
	return ""
}

func Lookup(adapter code.Adapter) (Vendor, bool) {
	for _, v := range vendors {
		if v.Adapter == adapter {
			return v, true
		}
	}
	return Vendor{}, false
}

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

type ConnectionIdentity struct {
	Kind      string `json:"kind"`
	AuthMode  string `json:"auth_mode"`
	ProfileID string `json:"profile_id"`
}

func AdaptersForConnections(connections map[string]ConnectionIdentity) []code.Adapter {
	adapters := []code.Adapter{}
	for _, c := range connections {
		if v, ok := ForConnection(c.Kind, c.AuthMode, c.ProfileID); ok && !slices.Contains(adapters, v.Adapter) {
			adapters = append(adapters, v.Adapter)
		}
	}
	slices.SortFunc(adapters, Compare)
	return adapters
}

func Compare(a, b code.Adapter) int {
	return cmp.Compare(rank(a), rank(b))
}

func rank(adapter code.Adapter) int {
	i := slices.IndexFunc(vendors, func(v Vendor) bool { return v.Adapter == adapter })
	if i < 0 {
		return len(vendors)
	}
	return i
}

func (v Vendor) Serves(p code.Protocol) bool {
	return slices.ContainsFunc(v.endpoints, func(e Endpoint) bool { return e.Protocol == p })
}

func (v Vendor) Reaches(client, model string) bool {
	_, ok := v.Protocol(client, model)
	return ok
}

func (v Vendor) Protocol(client, model string) (code.Protocol, bool) {
	c, ok := v.Client(client)
	if !ok {
		return "", false
	}
	protocols := clientProtocols[client]
	if c.Protocol != "" {
		protocols = []code.Protocol{c.Protocol}
	}
	for _, p := range protocols {
		if v.ServesModel(p, model) {
			return p, true
		}
	}
	return "", false
}

func (v Vendor) ServesModel(p code.Protocol, model string) bool {
	if !v.Serves(p) {
		return false
	}
	if v.modelProtocol != nil {
		return v.modelProtocol(model) == p
	}
	return true
}

func (v Vendor) Address(profileID string) (string, bool) {
	for _, p := range v.Manifest().Profiles {
		if p.ID == profileID && slices.Contains(v.profiles, profileID) {
			return p.Hosting.Address, true
		}
	}
	return "", false
}

func RouteEndpoint(route code.Route, adapter Vendor, path string) (Endpoint, bool) {
	if endpoint, ok := adapter.Endpoint(path); ok {
		return endpoint, endpoint.Target.Enabled(route)
	}
	if adapter.Adapter == code.AdapterCodex && (route.MessagesToCodex && path == "v1/messages" || route.ChatToCodex && path == "v1/chat/completions") {
		return adapter.Endpoint("responses")
	}
	return Endpoint{}, false
}

func RouteClientProtocol(route code.Route, adapter Vendor, client, model string) (code.Protocol, bool) {
	if protocol, ok := adapter.Protocol(client, model); ok {
		return protocol, slices.ContainsFunc(adapter.endpoints, func(endpoint Endpoint) bool { return endpoint.Protocol == protocol && endpoint.Target.Enabled(route) })
	}
	if adapter.Adapter != code.AdapterCodex {
		return "", false
	}
	if client == code.ClientClaudeCode && route.MessagesToCodex {
		endpoint, ok := RouteEndpoint(route, adapter, "v1/messages")
		return code.ProtocolMessages, ok && adapter.ServesModel(endpoint.Protocol, model)
	}
	if client == code.ClientOpenCode && route.ChatToCodex {
		endpoint, ok := RouteEndpoint(route, adapter, "v1/chat/completions")
		return code.ProtocolChat, ok && adapter.ServesModel(endpoint.Protocol, model)
	}
	return "", false
}

func RouteClient(route code.Route, adapter Vendor, client string) (Client, bool) {
	if native, ok := adapter.Client(client); ok {
		return native, true
	}
	if route.ChatToCodex && adapter.Adapter == code.AdapterCodex && client == code.ClientOpenCode {
		return Client{ID: code.ClientOpenCode, Provider: "olp-codex", URLSuffix: "/v1", Protocol: code.ProtocolChat}, true
	}
	return Client{}, false
}

func RouteClients(route code.Route, adapter Vendor) []string {
	return slices.Collect(func(yield func(string) bool) {
		for _, client := range adapter.Clients() {
			if !yield(client) {
				return
			}
		}
		if adapter.Adapter != code.AdapterCodex {
			return
		}
		if route.MessagesToCodex && !yield(code.ClientClaudeCode) {
			return
		}
		if route.ChatToCodex {
			yield(code.ClientOpenCode)
		}
	})
}
