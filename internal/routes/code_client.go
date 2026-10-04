package routes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codexauth"
)

// CodeClientConfiguration returns configuration for the latest publication.
// The management composition registers it with the route's read authorization.
func (s *Server) CodeClientConfiguration(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	var project string
	var document []byte
	var adapter *string
	err = s.Access.Pool.QueryRow(r.Context(), `SELECT r.project_id::text,v.document,`+codeadapter.SQLRevision("v.connections")+`
		FROM olp.code_routes r LEFT JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id WHERE r.id=$1`, id).Scan(&project, &document, &adapter)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&project, access.View); err != nil {
		return access.Reply{}, err
	}
	if len(document) == 0 {
		return access.Reply{}, access.Fail(409, "code_route_unpublished", "Publish an enabled code-mode route first.")
	}
	var route codemode.Route
	if err = json.Unmarshal(document, &route); err != nil {
		return access.Reply{}, err
	}
	// A revision whose connections name no adapter serves nothing; its
	// configuration is Codex's, as before adapters existed.
	selected := codemode.AdapterCodex
	if adapter != nil {
		selected = codemode.Adapter(*adapter)
	}
	query := r.URL.Query()
	config, err := ClientConfiguration(route, selected, ClientRequest{GatewayURL: query.Get("gateway_url"), Client: query.Get("client"), Model: query.Get("model"), SmallModel: query.Get("small_model")})
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(config, route.ETag), nil
}

// ClientRequest selects the configuration ClientConfiguration generates. Empty
// fields take the adapter's default client and the route's first model.
type ClientRequest struct {
	GatewayURL, Client, Model, SmallModel string
}

// ClientConfiguration generates a client's configuration for a published
// route, never its mutable draft. GatewayURL is the operator-configured public
// gateway origin and prefix.
func ClientConfiguration(route codemode.Route, adapter codemode.Adapter, in ClientRequest) (codemode.ClientConfiguration, error) {
	var out codemode.ClientConfiguration
	if !route.Enabled || route.RevisionID == "" || !access.RouteSlug.MatchString(route.Slug) {
		return out, access.Fail(409, "code_route_unpublished", "Publish an enabled code-mode route first.")
	}
	if err := codemode.ValidateModels(route.Models); err != nil {
		return out, access.Invalid("models", err.Error())
	}
	vendor, ok := codeadapter.Lookup(adapter)
	if !ok {
		return out, access.Fail(409, "code_route_adapter_unsupported", "This route's accounts have no supported client.")
	}
	client := in.Client
	if client == "" {
		client = vendor.Clients[0]
	}
	if !vendor.Supports(client) {
		names := make([]string, len(vendor.Clients))
		for i, c := range vendor.Clients {
			names[i] = clientNames[c]
		}
		return out, access.Invalid("client", fmt.Sprintf("This route's %s accounts support %s.", vendor.Name, strings.Join(names, " and ")))
	}
	model := in.Model
	if model == "" {
		model = route.Models[0]
	}
	if !slices.Contains(route.Models, model) {
		return out, access.Invalid("model", "Choose a native model admitted by the published route.")
	}
	var small *string
	switch {
	case client == codeadapter.ClientCodex && in.SmallModel != "":
		return out, access.Invalid("small_model", "Codex takes no separate background model.")
	case client != codeadapter.ClientCodex && in.SmallModel == "":
		small = &model
	case client != codeadapter.ClientCodex && !slices.Contains(route.Models, in.SmallModel):
		return out, access.Invalid("small_model", "Choose a native model admitted by the published route.")
	case client != codeadapter.ClientCodex:
		small = &in.SmallModel
	}
	base, err := url.Parse(in.GatewayURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" {
		return out, access.Invalid("gateway_url", "Use the public HTTPS gateway URL without credentials, query or fragment.")
	}
	local := base.Hostname() == "localhost"
	if ip := net.ParseIP(base.Hostname()); ip != nil {
		local = ip.IsLoopback()
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && local) {
		return out, access.Invalid("gateway_url", "Use HTTPS, or HTTP on loopback for local development.")
	}
	base.Path = strings.TrimRight(base.Path, "/") + route.BasePath()
	base.RawPath = ""
	out = codemode.ClientConfiguration{
		RouteSlug: route.Slug, BaseURL: base.String(), NativeModels: slices.Clone(route.Models), Adapter: adapter,
		Client: client, SupportedClients: slices.Clone(vendor.Clients), Model: model, SmallModel: small,
		QualificationGaps: qualificationGaps(adapter, client),
	}
	switch client {
	case codeadapter.ClientCodex:
		out.ClientVersion, out.Format, out.File = codexauth.ClientVersion, "toml", new("$CODEX_HOME/config.toml")
		out.Configuration = codexConfiguration(out.BaseURL, model)
	case codeadapter.ClientClaudeCode:
		out.ClientVersion, out.Format = codeadapter.ClaudeCodeVersion, "shell"
		out.Configuration = claudeCodeConfiguration(route.Slug, vendor.Name, out.BaseURL, model, *small)
	case codeadapter.ClientOpenCode:
		out.ClientVersion, out.Format, out.File = codeadapter.OpenCodeVersion, "json", new("opencode.json")
		out.Configuration, err = openCodeConfiguration(adapter, out.BaseURL, route.Models, model, *small)
	}
	return out, err
}

// CodexClientConfiguration is ClientConfiguration of the Codex client.
func CodexClientConfiguration(route codemode.Route, gatewayURL, model string) (codemode.ClientConfiguration, error) {
	return ClientConfiguration(route, codemode.AdapterCodex, ClientRequest{GatewayURL: gatewayURL, Model: model})
}

var clientNames = map[string]string{codeadapter.ClientCodex: "Codex", codeadapter.ClientClaudeCode: "Claude Code", codeadapter.ClientOpenCode: "OpenCode"}

func codexConfiguration(baseURL, model string) string {
	return fmt.Sprintf(`# Codex %s. Save as $CODEX_HOME/config.toml (default: ~/.codex/config.toml).
# Set OLP_API_KEY to your OLP inference key. No OpenAI login or upstream token is needed.
# To select another model, regenerate with model=... so the WebSocket hint agrees.
model_provider = "olp"
model = %s

[model_providers.olp]
name = "OpenAI"
base_url = %s
env_key = "OLP_API_KEY"
wire_api = "responses"
requires_openai_auth = false
supports_websockets = true
http_headers = { "X-OLP-Code-Model" = %s }
`, codexauth.ClientVersion, strconv.Quote(model), strconv.Quote(baseURL), strconv.Quote(model))
}

// claudeCodeConfiguration sets every model variable to a native model the
// route admits, so background, subagent and auto-mode requests stay on it.
func claudeCodeConfiguration(slug, plan, baseURL, model, small string) string {
	return fmt.Sprintf(`# Claude Code %s for OLP code-mode route %s (%s).
# Source this file in a POSIX shell, such as bash or zsh, before starting claude.
# Set OLP_API_KEY to your OLP inference key first. No Anthropic login or vendor
# key belongs on this machine. To select other models, regenerate this file.
unset ANTHROPIC_API_KEY CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY
export ANTHROPIC_BASE_URL=%s
export ANTHROPIC_AUTH_TOKEN="${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
export ANTHROPIC_MODEL=%s
export ANTHROPIC_DEFAULT_OPUS_MODEL=%s
export ANTHROPIC_DEFAULT_SONNET_MODEL=%s
export ANTHROPIC_DEFAULT_HAIKU_MODEL=%s
export CLAUDE_CODE_SUBAGENT_MODEL=%s
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
`, codeadapter.ClaudeCodeVersion, slug, plan, shellQuote(baseURL), shellQuote(model), shellQuote(model), shellQuote(model), shellQuote(small), shellQuote(model))
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// openCodeProviders are the OpenCode providers whose base URL a configuration
// overrides, keeping OpenCode's own choice of endpoint for each model.
var openCodeProviders = map[codemode.Adapter]string{codemode.AdapterOpenCodeGo: "opencode-go", codemode.AdapterZAICoding: "zai-coding-plan"}

func openCodeConfiguration(adapter codemode.Adapter, baseURL string, models []string, model, small string) (string, error) {
	provider := openCodeProviders[adapter]
	type options struct {
		BaseURL string `json:"baseURL"`
		APIKey  string `json:"apiKey"`
	}
	type settings struct {
		Options   options  `json:"options"`
		Whitelist []string `json:"whitelist"`
	}
	document := struct {
		Schema           string              `json:"$schema"`
		Model            string              `json:"model"`
		SmallModel       string              `json:"small_model"`
		EnabledProviders []string            `json:"enabled_providers"`
		Provider         map[string]settings `json:"provider"`
	}{
		Schema: "https://opencode.ai/config.json", Model: provider + "/" + model, SmallModel: provider + "/" + small, EnabledProviders: []string{provider},
		Provider: map[string]settings{provider: {Options: options{BaseURL: baseURL + "/v1", APIKey: "{env:OLP_API_KEY}"}, Whitelist: slices.Clone(models)}},
	}
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err := encoder.Encode(document)
	return b.String(), err
}

func qualificationGaps(adapter codemode.Adapter, client string) []string {
	if adapter == codemode.AdapterCodex {
		return []string{
			"Enrollment validates authorization only; live subscription inference and client workflow qualification must be established separately.",
			"Hard token budgets require a separately proven operation/model bound; this authentication adapter supplies none.",
			"ChatGPT cloud tasks, standalone web search, account management and FedRAMP accounts are outside this local code-mode configuration.",
		}
	}
	gaps := []string{
		"Enrollment stores the pasted coding-plan key and identifies it by fingerprint; it qualifies no inference, entitlement or allowance.",
		"Hard token budgets require a separately proven operation/model bound; this adapter supplies none.",
	}
	switch adapter {
	case codemode.AdapterZAICoding:
		gaps = append(gaps, "Z.ai restricts the GLM Coding Plan to its officially supported coding tools; OLP forwards this client's requests unmodified but cannot guarantee that Z.ai accepts pooled use.")
	case codemode.AdapterOpenCodeGo:
		gaps = append(gaps, "OpenCode Go's terms govern its use; OLP forwards this client's requests unmodified but cannot guarantee that OpenCode accepts pooled use.")
	}
	switch client {
	case codeadapter.ClientClaudeCode:
		gaps = append(gaps,
			"Token counting and the connectivity probe are not served; Claude Code falls back to local estimates.",
			"Server tools such as web search and web fetch, and claude.ai login features, are outside code mode.",
			"Each generation is limited to ten minutes; raising API_TIMEOUT_MS cannot extend it.")
		if adapter == codemode.AdapterOpenCodeGo {
			gaps = append(gaps, "OpenCode Go serves Anthropic Messages only for some models, such as MiniMax and Qwen; choose one of those for Claude Code.")
		}
	case codeadapter.ClientOpenCode:
		gaps = append(gaps,
			"OpenCode picks each model's endpoint from its own model catalogue; a route model the pinned release does not know is unavailable.",
			"Chat Completions usage settles only from the final usage chunk; a stream without it leaves consumption uncertain.")
		if adapter == codemode.AdapterZAICoding {
			gaps = append(gaps, "Z.ai's vision, web search and web reader MCP servers are separate endpoints this route does not serve.")
		}
	}
	return gaps
}
