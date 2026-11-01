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
	if err := route.ValidateAliases(); err != nil {
		return empty, access.Invalid("aliases", err.Error())
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
		for _, adapter := range offers.adapters(route, model.ID) {
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
			return empty, access.Invalid(companion.field, "These models cannot share a conversation. Choose models from compatible subscriptions or only paid API targets.")
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
			_, paid := offers.paidProtocol(route, model, client)
			return codexauth.ClientVersion, "toml", codexConfiguration(baseURL, model, !paid), nil
		case codeadapter.ClientClaudeCode:
			if platform == "windows" {
				return codeadapter.ClaudeCodeVersion, "powershell", claudeCodePowerShellConfiguration(baseURL, model, *plan, *small), nil
			}
			return codeadapter.ClaudeCodeVersion, "shell", claudeCodeConfiguration(baseURL, model, *plan, *small), nil
		case codeadapter.ClientOpenCode:
			text, err := openCodeConfiguration(route, adapters, offers, through, native, baseURL, model, *plan, *small)
			return codeadapter.OpenCodeVersion, "json", text, err
		}
		return "", "", "", nil
	}()
	file, nativeInstructions := platformInstructions(client, platform)
	instructions := nativeInstructions + func() string {
		if route.ChatToAntigravity && slices.Contains(adapters, codemode.AdapterAntigravity) {
			return " This published route enables the controlled Antigravity Gemini target for Chat text and function tools over HTTP/SSE. Signed thought state, reasoning controls, media and server tools are refused. Client identity and prompt compatibility remain disabled. Current-model tool acceptance and live subscription qualification remain pending."
		}
		return ""
	}() + func() string {
		return ""
	}() + func() string {
		if client == codeadapter.ClientOpenCode && route.ChatToCodex && slices.Contains(adapters, codemode.AdapterCodex) {
			return " This published route explicitly translates Chat text and function tool workflows through Codex Responses. Low, medium and high reasoning effort is supported. Signed reasoning, media, server-owned state and unsupported required semantics are refused. Identity compatibility requires its independent opt-in. Current OpenCode client and live subscription acceptance remain unqualified."
		}
		if client == codeadapter.ClientClaudeCode && route.MessagesToCodex && slices.Contains(adapters, codemode.AdapterCodex) {
			return " This published route enables Messages text and tool use through Codex accounts. Explicit low, medium and high reasoning effort is supported. Signed thinking, numeric reasoning budgets, beta features and other unsupported required semantics are refused. Prompt text is preserved, and identity and prompt compatibility remain disabled."
		}
		return ""
	}() + func() string {
		if route.ModelRewriting {
			return " Outside configured model pools, published aliases rewrite only model identifiers to their exact native targets. Existing conversations retain their pinned targets while authorized."
		}
		return ""
	}() + func() string {
		if len(route.ModelPools) > 0 {
			return " Published model pools permit ordered alternate native models only after proven nonexecution and portable state. Exact selection remains the default outside these pools. Model and account availability, current authorization and budgets still apply. Existing conversations retain the recovered exact target while published and authorized."
		}
		return ""
	}()
	out := codemode.ClientConfiguration{
		Models: catalog,
		PaidModels: slices.Collect(func(yield func(string) bool) {
			for _, m := range catalog {
				if m.Paid && !yield(m.ID) {
					return
				}
			}
		}),
		RouteSlug: route.Slug, BaseURL: baseURL, NativeModels: native, Adapters: append([]codemode.Adapter{}, adapters...),
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

const nativeModel = "Choose a published model or exact alias this route serves for the client."

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
