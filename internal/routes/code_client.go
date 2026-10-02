package routes

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
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
	err = s.Access.Pool.QueryRow(r.Context(), `SELECT r.project_id::text,v.document
		FROM olp.code_routes r LEFT JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id WHERE r.id=$1`, id).Scan(&project, &document)
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
	config, err := CodexClientConfiguration(route, r.URL.Query().Get("gateway_url"), r.URL.Query().Get("model"))
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(config, route.ETag), nil
}

// CodexClientConfiguration consumes the published route, never its mutable
// draft. gatewayURL is the operator-configured public gateway origin/prefix.
func CodexClientConfiguration(route codemode.Route, gatewayURL, model string) (codemode.ClientConfiguration, error) {
	var out codemode.ClientConfiguration
	if !route.Enabled || route.RevisionID == "" || !access.RouteSlug.MatchString(route.Slug) {
		return out, access.Fail(409, "code_route_unpublished", "Publish an enabled code-mode route first.")
	}
	if err := codemode.ValidateModels(route.Models); err != nil {
		return out, access.Invalid("models", err.Error())
	}
	if model == "" {
		model = route.Models[0]
	}
	if !slices.Contains(route.Models, model) {
		return out, access.Invalid("model", "Choose a native model admitted by the published route.")
	}
	base, err := url.Parse(gatewayURL)
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
		RouteSlug: route.Slug, BaseURL: base.String(), NativeModels: slices.Clone(route.Models), Client: "codex", ClientVersion: codexauth.ClientVersion,
		QualificationGaps: []string{
			"Enrollment validates authorization only; live subscription inference and client workflow qualification must be established separately.",
			"Hard token budgets require a separately proven operation/model bound; this authentication adapter supplies none.",
			"ChatGPT cloud tasks, standalone web search, account management and FedRAMP accounts are outside this local code-mode configuration.",
		},
	}
	out.Configuration = fmt.Sprintf(`# Codex %s. Save as $CODEX_HOME/config.toml (default: ~/.codex/config.toml).
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
`, codexauth.ClientVersion, strconv.Quote(model), strconv.Quote(out.BaseURL), strconv.Quote(model))
	return out, nil
}
