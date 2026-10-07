package routes

import (
	"bytes"
	"cmp"
	"context"
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
	var project, revision string
	var document []byte
	err = s.Access.Pool.QueryRow(r.Context(), `SELECT r.project_id::text,coalesce(v.id::text,''),v.document
		FROM olp.code_routes r LEFT JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id WHERE r.id=$1`, id).Scan(&project, &revision, &document)
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
	offers, err := s.codeOffers(r.Context(), revision)
	if err != nil {
		return access.Reply{}, err
	}
	if len(offers) == 0 {
		// The Codex fallback fits a revision whose frozen connections name
		// no adapter, as before adapters existed. When they do, the pool
		// has stopped serving the published subscriptions: no honest
		// configuration exists until an account returns or the route is
		// republished.
		var frozen []codemode.Adapter
		if err = s.Access.Pool.QueryRow(r.Context(), `SELECT `+codeadapter.SQLAdapters("connections")+` FROM olp.code_route_revisions WHERE id=$1`, revision).Scan(&frozen); err != nil {
			return access.Reply{}, err
		}
		if len(frozen) != 0 {
			return access.Reply{}, access.Fail(409, "code_route_unserved", "No pool account serves this route's published subscriptions; restore one or republish.")
		}
		offers = codexOffers(route.Models)
	}
	query := r.URL.Query()
	config, err := ClientConfiguration(route, offers, ClientRequest{GatewayURL: query.Get("gateway_url"), Client: query.Get("client"), Model: query.Get("model"), PlanModel: query.Get("plan_model"), SmallModel: query.Get("small_model")})
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(config, route.ETag), nil
}

// Offers maps each model of a route to the adapters whose accounts serve it,
// in table order.
type Offers map[string][]codemode.Adapter

// codeOffers reads the offers of a revision's pool accounts whose providers
// it froze, the accounts its requests can reach.
func (s *Server) codeOffers(ctx context.Context, revision string) (Offers, error) {
	rows, err := s.Access.Pool.Query(ctx, `SELECT DISTINCT m.model,`+codeadapter.SQL("(v.connections->(a.provider_id::text))")+`
		FROM olp.code_route_revisions v JOIN olp.code_pool_accounts pa ON pa.pool_id=(v.document->>'pool_id')::uuid
		JOIN olp.code_accounts a ON a.id=pa.account_id CROSS JOIN jsonb_array_elements_text(a.models) m(model)
		WHERE v.id=$1 AND v.connections ? a.provider_id::text`, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	offers := Offers{}
	for rows.Next() {
		var model string
		var adapter *string
		if err = rows.Scan(&model, &adapter); err != nil {
			return nil, err
		}
		if adapter != nil {
			offers[model] = append(offers[model], codemode.Adapter(*adapter))
		}
	}
	for _, adapters := range offers {
		slices.SortFunc(adapters, codeadapter.Compare)
	}
	return offers, rows.Err()
}

// ClientRequest selects the configuration ClientConfiguration generates. Empty
// fields take the route's default client, the first native model the route
// serves it, and that model for planning and background work.
type ClientRequest struct {
	GatewayURL, Client, Model, PlanModel, SmallModel string
}

// ClientConfiguration generates a client's configuration for a published
// route, never its mutable draft. GatewayURL is the operator-configured public
// gateway origin and prefix. A client's native models are the route's models
// an adapter supporting it offers, so one configuration can draw on several
// subscriptions: each request reaches an account serving its model.
func ClientConfiguration(route codemode.Route, offers Offers, in ClientRequest) (codemode.ClientConfiguration, error) {
	var out codemode.ClientConfiguration
	if !route.Enabled || route.RevisionID == "" || !access.RouteSlug.MatchString(route.Slug) {
		return out, access.Fail(409, "code_route_unpublished", "Publish an enabled code-mode route first.")
	}
	if err := codemode.ValidateModels(route.Models); err != nil {
		return out, access.Invalid("models", err.Error())
	}
	var supported []string
	for _, model := range route.Models {
		for _, adapter := range offers[model] {
			vendor, _ := codeadapter.Lookup(adapter)
			for _, client := range vendor.Clients {
				if !slices.Contains(supported, client) {
					supported = append(supported, client)
				}
			}
		}
	}
	if len(supported) == 0 {
		return out, access.Fail(409, "code_route_adapter_unsupported", "This route's accounts have no supported client.")
	}
	client := cmp.Or(in.Client, supported[0])
	if !slices.Contains(supported, client) {
		names := make([]string, len(supported))
		for i, c := range supported {
			names[i] = clientNames[c]
		}
		return out, access.Invalid("client", fmt.Sprintf("This route's accounts support %s.", conjoin(names)))
	}
	var native []string
	var adapters []codemode.Adapter
	for _, model := range route.Models {
		for _, adapter := range offers[model] {
			if vendor, _ := codeadapter.Lookup(adapter); vendor.Supports(client) {
				if !slices.Contains(native, model) {
					native = append(native, model)
				}
				if !slices.Contains(adapters, adapter) {
					adapters = append(adapters, adapter)
				}
			}
		}
	}
	slices.SortFunc(adapters, codeadapter.Compare)
	model := cmp.Or(in.Model, native[0])
	if !slices.Contains(native, model) {
		return out, access.Invalid("model", nativeModel)
	}
	plan, err := companionModel(client, "plan_model", "planning", in.PlanModel, model, native)
	if err != nil {
		return out, err
	}
	small, err := companionModel(client, "small_model", "background", in.SmallModel, model, native)
	if err != nil {
		return out, err
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
		RouteSlug: route.Slug, BaseURL: base.String(), NativeModels: native, Adapters: adapters,
		Client: client, SupportedClients: supported, Model: model, PlanModel: plan, SmallModel: small,
		QualificationGaps: qualificationGaps(adapters, client),
	}
	switch client {
	case codeadapter.ClientCodex:
		out.ClientVersion, out.Format, out.File = codexauth.ClientVersion, "toml", new("$CODEX_HOME/config.toml")
		out.Configuration = codexConfiguration(out.BaseURL, model)
	case codeadapter.ClientClaudeCode:
		names := make([]string, len(adapters))
		for i, adapter := range adapters {
			vendor, _ := codeadapter.Lookup(adapter)
			names[i] = vendor.Name
		}
		out.ClientVersion, out.Format = codeadapter.ClaudeCodeVersion, "shell"
		out.Configuration = claudeCodeConfiguration(route.Slug, conjoin(names), out.BaseURL, model, *plan, *small)
	case codeadapter.ClientOpenCode:
		out.ClientVersion, out.Format, out.File = codeadapter.OpenCodeVersion, "json", new("opencode.json")
		out.Configuration, err = openCodeConfiguration(adapters, offers, native, out.BaseURL, model, *plan, *small)
	}
	return out, err
}

const nativeModel = "Choose a native model the published route serves this client."

// companionModel returns a client's planning or background model: the main
// model unless another native one is selected. Codex takes neither.
func companionModel(client, field, role, selected, model string, native []string) (*string, error) {
	switch {
	case client == codeadapter.ClientCodex && selected != "":
		return nil, access.Invalid(field, "Codex takes no separate "+role+" model.")
	case client == codeadapter.ClientCodex:
		return nil, nil
	case selected == "":
		return &model, nil
	case !slices.Contains(native, selected):
		return nil, access.Invalid(field, nativeModel)
	}
	return &selected, nil
}

// CodexClientConfiguration is ClientConfiguration of the Codex client on a
// route whose Codex accounts serve all its models.
func CodexClientConfiguration(route codemode.Route, gatewayURL, model string) (codemode.ClientConfiguration, error) {
	return ClientConfiguration(route, codexOffers(route.Models), ClientRequest{GatewayURL: gatewayURL, Model: model})
}

// codexOffers offers every model by Codex.
func codexOffers(models []string) Offers {
	offers := Offers{}
	for _, model := range models {
		offers[model] = []codemode.Adapter{codemode.AdapterCodex}
	}
	return offers
}

// conjoin lists names in prose: "A", "A and B", "A, B and C".
func conjoin(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
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
// route admits, so background, subagent and auto-mode requests stay on it. A
// separate planning model selects opusplan, which plans with the Opus model
// and does everything else with the Sonnet model.
func claudeCodeConfiguration(slug, plans, baseURL, model, plan, small string) string {
	selected := model
	if plan != model {
		selected = "opusplan"
	}
	return fmt.Sprintf(`# Claude Code %s for OLP code-mode route %s (%s).
# Source this file in a POSIX shell, such as bash or zsh, before starting claude.
# Set OLP_API_KEY to your OLP inference key first. No Anthropic login or vendor
# key belongs on this machine. To select other models, regenerate this file.
unset ANTHROPIC_API_KEY ANTHROPIC_SMALL_FAST_MODEL CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_FOUNDRY
export ANTHROPIC_BASE_URL=%s
export ANTHROPIC_AUTH_TOKEN="${OLP_API_KEY:?Set OLP_API_KEY to your OLP inference key}"
export ANTHROPIC_MODEL=%s
export ANTHROPIC_DEFAULT_OPUS_MODEL=%s
export ANTHROPIC_DEFAULT_SONNET_MODEL=%s
export ANTHROPIC_DEFAULT_HAIKU_MODEL=%s
export CLAUDE_CODE_SUBAGENT_MODEL=%s
export CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
`, codeadapter.ClaudeCodeVersion, slug, plans, shellQuote(baseURL), shellQuote(selected), shellQuote(plan), shellQuote(model), shellQuote(small), shellQuote(model))
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// openCodeProviders are the OpenCode providers whose base URL a configuration
// overrides, keeping OpenCode's own choice of endpoint for each model.
var openCodeProviders = map[codemode.Adapter]string{codemode.AdapterOpenCodeGo: "opencode-go", codemode.AdapterZAICoding: "zai-coding-plan"}

// openCodeConfiguration lists each native model under the provider of the
// first adapter that offers it, and enables those providers. A planning model
// other than the main one becomes the plan agent's.
func openCodeConfiguration(adapters []codemode.Adapter, offers Offers, native []string, baseURL, model, plan, small string) (string, error) {
	type options struct {
		BaseURL string `json:"baseURL"`
		APIKey  string `json:"apiKey"`
	}
	type settings struct {
		Options   options  `json:"options"`
		Whitelist []string `json:"whitelist"`
	}
	type agent struct {
		Model string `json:"model"`
	}
	whitelists := map[codemode.Adapter][]string{}
	qualified := map[string]string{}
	for _, m := range native {
		for _, adapter := range offers[m] {
			if provider, ok := openCodeProviders[adapter]; ok {
				whitelists[adapter] = append(whitelists[adapter], m)
				qualified[m] = provider + "/" + m
				break
			}
		}
	}
	document := struct {
		Schema           string              `json:"$schema"`
		Model            string              `json:"model"`
		SmallModel       string              `json:"small_model"`
		Agent            map[string]agent    `json:"agent,omitempty"`
		EnabledProviders []string            `json:"enabled_providers"`
		Provider         map[string]settings `json:"provider"`
	}{Schema: "https://opencode.ai/config.json", Model: qualified[model], SmallModel: qualified[small], Provider: map[string]settings{}}
	if plan != model {
		document.Agent = map[string]agent{"plan": {Model: qualified[plan]}}
	}
	for _, adapter := range adapters {
		if models := whitelists[adapter]; len(models) != 0 {
			provider := openCodeProviders[adapter]
			document.EnabledProviders = append(document.EnabledProviders, provider)
			document.Provider[provider] = settings{Options: options{BaseURL: baseURL + "/v1", APIKey: "{env:OLP_API_KEY}"}, Whitelist: models}
		}
	}
	var b bytes.Buffer
	encoder := json.NewEncoder(&b)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	err := encoder.Encode(document)
	return b.String(), err
}

func qualificationGaps(adapters []codemode.Adapter, client string) []string {
	var gaps []string
	if slices.Contains(adapters, codemode.AdapterCodex) {
		gaps = append(gaps,
			"Enrollment validates authorization only; live subscription inference and client workflow qualification must be established separately.",
			"Hard token budgets require a separately proven operation/model bound; this authentication adapter supplies none.",
			"ChatGPT cloud tasks, standalone web search, account management and FedRAMP accounts are outside this local code-mode configuration.")
	}
	if slices.ContainsFunc(adapters, func(a codemode.Adapter) bool { return a != codemode.AdapterCodex }) {
		gaps = append(gaps,
			"Enrollment stores the pasted coding-plan key and identifies it by fingerprint; it qualifies no inference, entitlement or allowance.",
			"Hard token budgets require a separately proven operation/model bound; this adapter supplies none.")
	}
	for _, adapter := range adapters {
		switch adapter {
		case codemode.AdapterOpenCodeGo:
			gaps = append(gaps, "OpenCode Go's terms govern its use; OLP forwards this client's requests unmodified but cannot guarantee that OpenCode accepts pooled use.")
		case codemode.AdapterZAICoding:
			gaps = append(gaps, "Z.ai restricts the GLM Coding Plan to its officially supported coding tools; OLP forwards this client's requests unmodified but cannot guarantee that Z.ai accepts pooled use.")
		}
	}
	if len(adapters) > 1 {
		gaps = append(gaps,
			"Each request carries the conversation so far, so every subscription a conversation mixes receives the turns the others produced.",
			"No live vendor is qualified to accept another vendor's reasoning and tool calls in that history; mixed conversations are qualified against scripted vendors only.")
	}
	switch client {
	case codeadapter.ClientClaudeCode:
		gaps = append(gaps,
			"Token counting and the connectivity probe are not served; Claude Code falls back to local estimates.",
			"Server tools such as web search and web fetch, and claude.ai login features, are outside code mode.",
			"Each generation is limited to ten minutes; raising API_TIMEOUT_MS cannot extend it.")
		if slices.Contains(adapters, codemode.AdapterOpenCodeGo) {
			gaps = append(gaps, "OpenCode Go serves Anthropic Messages only for some models, such as MiniMax and Qwen; choose one of those for Claude Code.")
		}
	case codeadapter.ClientOpenCode:
		gaps = append(gaps,
			"OpenCode picks each model's endpoint from its own model catalogue; a route model the pinned release does not know is unavailable.",
			"Chat Completions usage settles only from the final usage chunk; a stream without it leaves consumption uncertain.")
		if slices.Contains(adapters, codemode.AdapterZAICoding) {
			gaps = append(gaps, "Z.ai's vision, web search and web reader MCP servers are separate endpoints this route does not serve.")
		}
	}
	return gaps
}
