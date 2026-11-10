package client

import (
	"cmp"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/code/vendor"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/routeidentity"
)

type Error struct {
	Status  int
	Code    string
	Field   string
	Message string
}

func (e *Error) Error() string { return e.Message }

func invalidField(field, message string) error {
	return &Error{Status: 422, Code: "validation_error", Field: field, Message: message}
}

func failConfig(status int, errCode, message string) error {
	return &Error{Status: status, Code: errCode, Message: message}
}

type Request struct {
	GatewayURL, Client, Platform, Model, PlanModel, SmallModel string
}

func Generate(route code.Route, offers Offers, in Request) (code.ClientConfiguration, error) {
	empty := code.ClientConfiguration{}
	if !route.Enabled || route.RevisionID == "" || !routeidentity.ValidSlug(route.Slug) {
		return empty, failConfig(409, "code_route_unpublished", "Publish an enabled code-mode route first.")
	}
	platform := cmp.Or(in.Platform, code.PlatformLinux)
	if !slices.Contains([]string{code.PlatformLinux, code.PlatformMacOS, code.PlatformWindows}, platform) {
		return empty, invalidField("platform", "Choose Linux, macOS or Windows.")
	}
	if err := code.ValidateModels(route.Models); err != nil {
		return empty, invalidField("models", err.Error())
	}
	if err := route.ValidateAliases(); err != nil {
		return empty, invalidField("aliases", err.Error())
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
		return empty, failConfig(409, "code_route_adapter_unsupported", "This route's accounts have no supported client.")
	}
	client := cmp.Or(in.Client, supported[0])
	if !slices.Contains(supported, client) {
		names := make([]string, len(supported))
		for i, c := range supported {
			names[i] = clientNames[c]
		}
		return empty, invalidField("client", fmt.Sprintf("This route's accounts support %s.", conjoin(names)))
	}
	var native []string
	var adapters []code.Adapter
	for _, model := range catalog {
		if !slices.Contains(model.Clients, client) {
			continue
		}
		native = append(native, model.ID)
		for _, adapter := range offers.adapters(route, model.ID) {
			if declaration, _ := vendor.Lookup(adapter); routeReaches(route, declaration, client, model.ID) && !slices.Contains(adapters, adapter) {
				adapters = append(adapters, adapter)
			}
		}
	}
	slices.SortFunc(adapters, vendor.Compare)
	model := cmp.Or(in.Model, native[0])
	if !slices.Contains(native, model) {
		return empty, invalidField("model", nativeModelMessage)
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
			return empty, invalidField(companion.field, "These models cannot share a conversation. Choose models from compatible subscriptions or only paid API targets.")
		}
	}
	base, err := url.Parse(in.GatewayURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || base.Opaque != "" {
		return empty, invalidField("gateway_url", "Use the public HTTPS gateway URL without credentials, query or fragment.")
	}
	local := base.Hostname() == "localhost"
	if ip := net.ParseIP(base.Hostname()); ip != nil {
		local = ip.IsLoopback()
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && local) {
		return empty, invalidField("gateway_url", "Use HTTPS, or HTTP on loopback for local development.")
	}
	base.Path = strings.TrimRight(base.Path, "/") + route.BasePath()
	base.RawPath = ""
	baseURL := base.String()
	version, format, configuration, renderErr := func() (string, string, string, error) {
		switch client {
		case code.ClientCodex:
			_, paid := offers.paidProtocol(route, model, client)
			return codexauth.ClientVersion, "toml", codexConfiguration(baseURL, model, !paid), nil
		case code.ClientClaudeCode:
			if platform == code.PlatformWindows {
				return vendor.ClaudeCodeVersion, "powershell", claudeCodePowerShellConfiguration(baseURL, model, *plan, *small), nil
			}
			return vendor.ClaudeCodeVersion, "shell", claudeCodeConfiguration(baseURL, model, *plan, *small), nil
		case code.ClientOpenCode:
			text, err := openCodeConfiguration(route, adapters, offers, through, native, baseURL, model, *plan, *small)
			return vendor.OpenCodeVersion, "json", text, err
		}
		return "", "", "", nil
	}()
	file, nativeInstructions := platformInstructions(client, platform)
	instructions := buildInstructions(nativeInstructions, route, client, adapters)
	out := code.ClientConfiguration{
		Models: catalog,
		PaidModels: slices.Collect(func(yield func(string) bool) {
			for _, m := range catalog {
				if m.Paid && !yield(m.ID) {
					return
				}
			}
		}),
		RouteSlug: route.Slug, BaseURL: baseURL, NativeModels: native, Adapters: slices.Clone(adapters),
		Client: client, Platform: platform, File: file, Instructions: instructions, SupportedClients: supported, Model: model, PlanModel: plan, SmallModel: small,
		ClientVersion: version, Format: format, Configuration: configuration,
		QualificationGaps: append(qualificationGaps(adapters, client), "Generated configuration is not evidence of native platform startup or live subscription qualification."),
	}
	if renderErr != nil {
		return out, renderErr
	}
	return out, addResearchConfiguration(&out, route.Tools)
}

func buildInstructions(base string, route code.Route, client string, adapters []code.Adapter) string {
	var b strings.Builder
	b.WriteString(base)
	if route.ChatToAntigravity && slices.Contains(adapters, code.AdapterAntigravity) {
		b.WriteString(" This published route enables the controlled Antigravity Gemini target for Chat text and function tools over HTTP/SSE. Signed thought state, reasoning controls, media and server tools are refused. Client identity and prompt compatibility remain disabled. Current-model tool acceptance and live subscription qualification remain pending.")
	}
	if client == code.ClientOpenCode && route.ChatToCodex && slices.Contains(adapters, code.AdapterCodex) {
		b.WriteString(" This published route explicitly translates Chat text and function tool workflows through Codex Responses. Low, medium and high reasoning effort is supported. Signed reasoning, media, server-owned state and unsupported required semantics are refused. Identity compatibility requires its independent opt-in. Current OpenCode client and live subscription acceptance remain unqualified.")
	} else if client == code.ClientClaudeCode && route.MessagesToCodex && slices.Contains(adapters, code.AdapterCodex) {
		b.WriteString(" This published route enables Messages text and tool use through Codex accounts. Explicit low, medium and high reasoning effort is supported. Signed thinking, numeric reasoning budgets, beta features and other unsupported required semantics are refused. Prompt text is preserved, and identity and prompt compatibility remain disabled.")
	}
	if route.ModelRewriting {
		b.WriteString(" Outside configured model pools, published aliases rewrite only model identifiers to their exact native targets. Existing conversations retain their pinned targets while authorized.")
	}
	if len(route.ModelPools) > 0 {
		b.WriteString(" Published model pools permit ordered alternate native models only after proven nonexecution and portable state. Exact selection remains the default outside these pools. Model and account availability, current authorization and budgets still apply. Existing conversations retain the recovered exact target while published and authorized.")
	}
	return b.String()
}

const nativeModelMessage = "Choose a published model or exact alias this route serves for the client."

func companionModel(client, field, role, selected, model string, native []string) (*string, error) {
	switch {
	case client == code.ClientCodex && selected != "":
		return nil, invalidField(field, "Codex takes no separate "+role+" model.")
	case client == code.ClientCodex:
		return nil, nil
	case selected == "":
		return &model, nil
	case !slices.Contains(native, selected):
		return nil, invalidField(field, nativeModelMessage)
	}
	return &selected, nil
}

func conjoin(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

var clientNames = map[string]string{
	code.ClientCodex:      "Codex",
	code.ClientClaudeCode: "Claude Code",
	code.ClientOpenCode:   "OpenCode",
}
