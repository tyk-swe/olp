package operatorcli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/tyk-swe/olp/management"
)

func (runner Runner) codeSetup(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		_, err := fmt.Fprintln(runner.Out, "usage: olp code-setup preview --route ID --gateway-url URL [--model MODEL]\n       olp code-setup apply --route ID --gateway-url URL [--model MODEL] [--revision ID]\n       olp code-setup status\n       olp code-setup detach")
		return err
	}
	action := args[0]
	opts, positional, err := parseOptions(args[1:])
	if err != nil {
		return err
	}
	if len(positional) != 0 {
		return errors.New("code-setup takes named options")
	}
	if err := checkCodeSetupOptions(action, opts); err != nil {
		return err
	}
	path, err := runner.codexConfigPath()
	if err != nil {
		return err
	}
	file, err := readCodexFile(path)
	if err != nil {
		return err
	}
	doc, err := parseCodexTOML(file.data)
	if err != nil {
		return err
	}
	if action == "status" || action == "detach" {
		return runner.localCodeSetup(file, doc, action)
	}
	pub, fragment, err := runner.codexFragment(ctx, opts)
	if err != nil {
		return err
	}
	next, err := doc.planSetup(fragment, pub, file.info != nil)
	if err != nil {
		return err
	}
	if bytes.Equal(file.data, next) {
		return runner.showCodeSetup("Already configured", path, pub)
	}
	if action == "preview" {
		return runner.showCodeSetup("Would configure", path, pub)
	}
	if err := file.replace(next); err != nil {
		return err
	}
	return runner.showCodeSetup("Configured", path, pub)
}

func checkCodeSetupOptions(action string, opts options) error {
	switch action {
	case "status", "detach":
		return checkOptions(opts)
	case "preview", "apply":
		if opts.one("route") == "" || opts.one("gateway-url") == "" {
			return errors.New("--route and --gateway-url are required")
		}
		if action == "apply" {
			return checkOptions(opts, "route", "gateway-url", "model", "revision")
		}
		return checkOptions(opts, "route", "gateway-url", "model")
	default:
		return errors.New("unknown code-setup command; use olp code-setup help")
	}
}

func (runner Runner) codexConfigPath() (string, error) {
	if home := runner.Getenv("CODEX_HOME"); home != "" {
		return filepath.Abs(filepath.Join(home, "config.toml"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Abs(filepath.Join(home, ".codex", "config.toml"))
}

func (runner Runner) localCodeSetup(file codexFile, doc codexDocument, action string) error {
	state, err := doc.setupState()
	if err != nil {
		return err
	}
	if state == nil {
		_, err := fmt.Fprintf(runner.Out, "No managed Code Mode setup in %q\n", file.path)
		return err
	}
	if action == "status" {
		if err := state.checkDrift(doc); err != nil {
			return err
		}
		return runner.showCodeSetup("Configured", file.path, state.Publication)
	}
	if err := file.detach(doc, *state); err != nil {
		return err
	}
	_, err = fmt.Fprintf(runner.Out, "Detached Code Mode from %q\n", file.path)
	return err
}

func (runner Runner) showCodeSetup(action, path string, p codexPublication) error {
	_, err := fmt.Fprintf(runner.Out, "%s %q\nEndpoint: %q\nModel: %q\nRoute revision: %d (%q)\n", action, path, p.Endpoint, p.Model, p.Revision, p.RevisionID)
	return err
}

func (runner Runner) codexFragment(ctx context.Context, opts options) (codexPublication, codexDocument, error) {
	client, err := runner.client()
	if err != nil {
		return codexPublication{}, codexDocument{}, err
	}
	op, ok := management.Lookup("get_code_client_configuration")
	if !ok {
		return codexPublication{}, codexDocument{}, errors.New("Code Mode configuration operation is unavailable")
	}
	query := map[string]any{"gateway_url": opts.one("gateway-url"), "client": "codex", "model": opts.one("model")}
	switch runtime.GOOS {
	case "darwin":
		query["platform"] = "macos"
	case "windows", "linux":
		query["platform"] = runtime.GOOS
	}
	response, err := client.Call(ctx, op, management.Arguments{Path: map[string]string{"id": opts.one("route")}, Query: query})
	if err != nil {
		return codexPublication{}, codexDocument{}, err
	}
	var result struct {
		Client        string `json:"client"`
		Format        string `json:"format"`
		Configuration string `json:"configuration"`
		BaseURL       string `json:"base_url"`
		Model         string `json:"model"`
		RevisionID    string `json:"revision_id"`
		Revision      int    `json:"revision"`
	}
	if err := json.Unmarshal(response.Body, &result); err != nil {
		return codexPublication{}, codexDocument{}, errors.New("invalid published Codex configuration response")
	}
	pub := codexPublication{Endpoint: result.BaseURL, Model: result.Model, RevisionID: result.RevisionID, Revision: result.Revision}
	if result.Client != "codex" || result.Format != "toml" || !pub.valid() {
		return codexPublication{}, codexDocument{}, errors.New("expected Codex TOML and a published route revision from the management API")
	}
	if expected := opts.one("revision"); expected != "" && expected != pub.RevisionID {
		return codexPublication{}, codexDocument{}, errors.New("the published route revision changed; run preview again")
	}
	doc, err := parseCodexTOML([]byte(result.Configuration))
	if err != nil || !doc.matchesPublication(pub) {
		return codexPublication{}, codexDocument{}, errors.New("unsupported Codex settings in the published route configuration")
	}
	return pub, doc, nil
}

func (doc codexDocument) matchesPublication(pub codexPublication) bool {
	provider := doc.provider
	return doc.journal.end == 0 && doc.model == pub.Model && doc.selected == "olp" &&
		doc.providerErr == nil && provider.endpoint == pub.Endpoint && provider.envKey == "OLP_API_KEY" &&
		provider.wireAPI == "responses" && provider.explicitlyNoOpenAIAuth &&
		provider.hasWebsockets && provider.modelHeader == pub.Model
}
