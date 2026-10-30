// Package codeclient selects models and renders client configuration from a
// frozen route and its account offers. Publication and authorization stay in routes.
package codeclient

import (
	"cmp"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codexauth"
)

// Request selects the configuration Generate produces. Empty
// fields take the route's default client, the first native model the route
// serves it, and that model for planning and background work.
type Request struct {
	GatewayURL, Client, Platform, Model, PlanModel, SmallModel string
}

// Generate generates a client's configuration for a published
// route, never its mutable draft. GatewayURL is the operator-configured public
// gateway origin and prefix. A client's native models are the route's models
// an adapter offers on a protocol the client speaks, so one configuration can
// draw on several subscriptions: each request reaches an account serving its
// model. Its planning and background models are those one conversation reaches
// with the main model, which uses one account of each adapter.
func Generate(route codemode.Route, offers Offers, in Request) (codemode.ClientConfiguration, error) {
	empty := codemode.ClientConfiguration{}
	if !route.Enabled || route.RevisionID == "" || !access.RouteSlug.MatchString(route.Slug) {
		return empty, access.Fail(409, "code_route_unpublished", "Publish an enabled code-mode route first.")
	}
	platform := cmp.Or(in.Platform, "linux")
	if !slices.Contains([]string{"linux", "macos", "windows"}, platform) {
		return empty, access.Invalid("platform", "Choose Linux, macOS or Windows.")
	}
	if err := codemode.ValidateModels(route.Models); err != nil {
		return empty, access.Invalid("models", err.Error())
	}
	catalog := Catalog(route, offers)
	var supported []string
	for _, model := range catalog {
		for _, client := range model.Clients {
			if !slices.Contains(supported, client) {
				supported = append(supported, client)
			}
		}
	}
	if len(supported) == 0 {
		return empty, access.Fail(409, "code_route_adapter_unsupported", "This route's accounts have no supported client.")
	}
	client := cmp.Or(in.Client, supported[0])
	if !slices.Contains(supported, client) {
		names := make([]string, len(supported))
		for i, c := range supported {
			names[i] = clientNames[c]
		}
		return empty, access.Invalid("client", fmt.Sprintf("This route's accounts support %s.", conjoin(names)))
	}
	var native []string
	var adapters []codemode.Adapter
	for _, model := range catalog {
		if !slices.Contains(model.Clients, client) {
			continue
		}
		native = append(native, model.ID)
		for _, adapter := range offers.adapters(model.ID) {
			if declaration, _ := codeadapter.Lookup(adapter); routeReaches(route, declaration, client, model.ID) {
				if !slices.Contains(adapters, adapter) {
					adapters = append(adapters, adapter)
				}
			}
		}
	}
	slices.SortFunc(adapters, codeadapter.Compare)
	model := cmp.Or(in.Model, native[0])
	if !slices.Contains(native, model) {
		return empty, access.Invalid("model", nativeModel)
	}
	plan, err := companionModel(client, "plan_model", "planning", in.PlanModel, model, native)
	if err != nil {
		return empty, err
	}
	small, err := companionModel(client, "small_model", "background", in.SmallModel, model, native)
	if err != nil {
		return empty, err
	}
	selected := []string{model}
	through, _ := offers.through(route, client, selected)
	for _, companion := range []struct {
		field string
		model *string
	}{{"plan_model", plan}, {"small_model", small}} {
		if companion.model == nil || slices.Contains(selected, *companion.model) {
			continue
		}
		selected = append(selected, *companion.model)
		var ok bool
		if through, ok = offers.through(route, client, selected); !ok {
			return empty, access.Invalid(companion.field, "A conversation uses one account of each subscription, so it cannot reach this model alongside the others selected.")
		}
	}
	base, err := url.Parse(in.GatewayURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" {
		return empty, access.Invalid("gateway_url", "Use the public HTTPS gateway URL without credentials, query or fragment.")
	}
	local := base.Hostname() == "localhost"
	if ip := net.ParseIP(base.Hostname()); ip != nil {
		local = ip.IsLoopback()
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && local) {
		return empty, access.Invalid("gateway_url", "Use HTTPS, or HTTP on loopback for local development.")
	}
	base.Path = strings.TrimRight(base.Path, "/") + route.BasePath()
	base.RawPath = ""
	baseURL := base.String()
	version, format, configuration, renderErr := func() (string, string, string, error) {
		switch client {
		case codeadapter.ClientCodex:
			return codexauth.ClientVersion, "toml", codexConfiguration(baseURL, model), nil
		case codeadapter.ClientClaudeCode:
			if platform == "windows" {
				return codeadapter.ClaudeCodeVersion, "powershell", claudeCodePowerShellConfiguration(baseURL, model, *plan, *small), nil
			}
			return codeadapter.ClaudeCodeVersion, "shell", claudeCodeConfiguration(baseURL, model, *plan, *small), nil
		case codeadapter.ClientOpenCode:
			text, err := openCodeConfiguration(adapters, offers, through, native, baseURL, model, *plan, *small)
			return codeadapter.OpenCodeVersion, "json", text, err
		}
		return "", "", "", nil
	}()
	file, nativeInstructions := platformInstructions(client, platform)
	instructions := func() string {
		if client == codeadapter.ClientClaudeCode && route.MessagesToCodex && slices.Contains(adapters, codemode.AdapterCodex) {
			return nativeInstructions + " This published route enables Messages text and tool use through Codex accounts. Explicit low, medium and high reasoning effort is supported. Signed thinking, numeric reasoning budgets, beta features and other unsupported required semantics are refused. Model identifiers and prompt text are preserved, and identity and prompt compatibility remain disabled"
		}
		return nativeInstructions
	}()
	out := codemode.ClientConfiguration{
		Models:    catalog,
		RouteSlug: route.Slug, BaseURL: baseURL, NativeModels: native, Adapters: adapters,
		Client: client, Platform: platform, File: file, Instructions: instructions, SupportedClients: supported, Model: model, PlanModel: plan, SmallModel: small,
		ClientVersion: version, Format: format, Configuration: configuration,
		QualificationGaps: append(qualificationGaps(adapters, client), "Generated configuration is not evidence of native platform startup or live subscription qualification."),
	}
	if renderErr != nil {
		return out, renderErr
	}
	researchErr := addResearchConfiguration(&out, route.Tools)
	return out, researchErr
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

// conjoin lists names in prose: "A", "A and B", "A, B and C".
func conjoin(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

var clientNames = map[string]string{codeadapter.ClientCodex: "Codex", codeadapter.ClientClaudeCode: "Claude Code", codeadapter.ClientOpenCode: "OpenCode"}
