package routes

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestCodexClientConfigurationPreservesNativeModelsAndUsesOnlyOLPKey(t *testing.T) {
	route := codemode.Route{Slug: "team-code", Enabled: true, RevisionID: "published", Models: []string{"gpt-5.4", "gpt-5.3-codex"}}
	config, err := CodexClientConfiguration(route, "https://gateway.example/olp/", "gpt-5.3-codex")
	if err != nil || config.BaseURL != "https://gateway.example/olp/code/team-code" || config.ClientVersion != "0.160.0" || len(config.NativeModels) != 2 {
		t.Fatalf("configuration: %+v %v", config, err)
	}
	for _, want := range []string{`model = "gpt-5.3-codex"`, `env_key = "OLP_API_KEY"`, `requires_openai_auth = false`, `supports_websockets = true`, `name = "OpenAI"`, `request_max_retries = 0`, `stream_max_retries = 0`} {
		if !strings.Contains(config.Configuration, want) {
			t.Fatalf("missing client setting %s", want)
		}
	}
	for _, forbidden := range []string{"refresh_token", "chatgpt.com", "OPENAI_API_KEY", "Authorization", "http_headers"} {
		if strings.Contains(config.Configuration, forbidden) {
			t.Fatalf("configuration contains upstream credential material or first-party headers: %s", forbidden)
		}
	}
	config.NativeModels[0] = "changed"
	if route.Models[0] != "gpt-5.4" {
		t.Fatal("configuration mutated the published route")
	}
	if _, err := CodexClientConfiguration(route, "http://127.0.0.1:8080", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := CodexClientConfiguration(route, "https://gateway.example", "team-code"); err == nil {
		t.Fatal("route alias accepted as native model")
	}
	for _, invalid := range []string{"http://gateway.example", "https://key@gateway.example", "https://gateway.example?key=secret", "https://gateway.example#fragment", "/relative"} {
		if _, err := CodexClientConfiguration(route, invalid, ""); err == nil {
			t.Fatalf("accepted unsafe URL %s", invalid)
		}
	}
	route.RevisionID = ""
	if _, err := CodexClientConfiguration(route, "https://gateway.example", ""); err == nil {
		t.Fatal("unpublished draft got client configuration")
	}
}
