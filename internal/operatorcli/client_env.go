package operatorcli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/tyk-swe/olp/sdk/management"
)

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func (runner Runner) clientEnv(args []string) error {
	if len(args) == 0 {
		return errors.New("client-env requires a client name")
	}
	clientName := args[0]
	switch clientName {
	case "openai-agents":
		clientName = "agents"
	case "ai-sdk":
		clientName = "vercel"
	case "llamaindex-ts":
		clientName = "llamaindex"
	}
	opts, positional, err := parseOptions(args[1:])
	if err != nil {
		return err
	}
	if len(positional) > 0 {
		return errors.New("client-env takes named options")
	}
	if err = checkOptions(opts, "url", "key-file", "model", "output", "format", "surface"); err != nil {
		return err
	}
	u, err := url.Parse(opts.one("url"))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("--url must be an HTTP(S) origin")
	}
	if u.Scheme == "http" {
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return errors.New("--url requires HTTPS outside loopback")
		}
	}
	if opts.one("key-file") == "" {
		return errors.New("--key-file is required")
	}
	if _, err = management.ReadTokenFile(opts.one("key-file")); err != nil {
		return err
	}
	path, err := filepath.Abs(opts.one("key-file"))
	if err != nil {
		return err
	}
	key := "$(cat -- " + shellQuote(path) + ")"
	origin := strings.TrimSuffix(u.String(), "/")
	model := opts.one("model")
	if model == "" {
		model = "assistant"
	}
	format, surface := opts.one("format"), opts.one("surface")
	framework := clientName == "agents" || clientName == "vercel" || clientName == "langchain" || clientName == "llamaindex"
	if format == "" {
		format = "shell"
		if framework {
			format = "javascript"
		}
	}
	if surface == "" {
		surface = "openai"
		if clientName == "anthropic" || clientName == "gemini" {
			surface = clientName
		}
	}
	if surface != "openai" && surface != "anthropic" && surface != "gemini" {
		return errors.New("--surface must be openai, anthropic or gemini")
	}
	if format != "shell" {
		if !framework && clientName != "openai" && clientName != "anthropic" && clientName != "gemini" {
			return errors.New("code formats require a qualified SDK or framework")
		}
		if !framework && surface != clientName {
			return errors.New("--surface must match the selected SDK")
		}
		code, codeErr := clientCode(clientName, format, surface, origin, path, model)
		if codeErr != nil {
			return codeErr
		}
		return runner.output(opts.one("output"), []byte(code))
	}
	if framework || opts.one("surface") != "" {
		return errors.New("--surface requires a code format")
	}
	var out strings.Builder
	switch clientName {
	case "openai":
		fmt.Fprintf(&out, "export OPENAI_BASE_URL=%s\nexport OPENAI_API_KEY=%s\n", shellQuote(origin+"/v1"), key)
	case "anthropic", "claude-code":
		fmt.Fprintf(&out, "export ANTHROPIC_BASE_URL=%s\nexport ANTHROPIC_API_KEY=%s\nexport ANTHROPIC_MODEL=%s\n", shellQuote(origin+"/anthropic"), key, shellQuote(model))
		if clientName == "claude-code" {
			fmt.Fprintf(&out, "export ANTHROPIC_DEFAULT_HAIKU_MODEL=%s\nexport ANTHROPIC_SMALL_FAST_MODEL=%s\nexport CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1\n", shellQuote(model), shellQuote(model))
		}
	case "gemini", "gemini-cli":
		fmt.Fprintf(&out, "export GOOGLE_GEMINI_BASE_URL=%s\nexport GEMINI_API_KEY=%s\nexport GEMINI_MODEL=%s\n", shellQuote(origin+"/gemini"), key, shellQuote(model))
	case "codex":
		fmt.Fprintf(&out, "export OLP_API_KEY=%s\n", key)
		// TOML basic strings use JSON escaping for these scalar values.
		quote := func(value string) string { encoded, _ := json.Marshal(value); return string(encoded) }
		settings := []string{"model=" + quote(model), "model_provider=\"olp\"", "web_search=\"disabled\"", "features.multi_agent=false", "check_for_update_on_startup=false", "model_providers.olp.name=\"OLP\"", "model_providers.olp.base_url=" + quote(origin+"/v1"), "model_providers.olp.env_key=\"OLP_API_KEY\"", "model_providers.olp.wire_api=\"responses\""}
		out.WriteString("olp_codex() { command codex")
		for _, setting := range settings {
			fmt.Fprintf(&out, " -c %s", shellQuote(setting))
		}
		out.WriteString(" \"$@\"; }\n")
	default:
		return errors.New("unsupported client; use openai, anthropic, gemini, claude-code, gemini-cli, codex, openai-agents, vercel, langchain or llamaindex")
	}
	return runner.output(opts.one("output"), []byte(out.String()))
}
