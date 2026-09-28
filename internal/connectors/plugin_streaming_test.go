package connectors

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// streamingManifest declares a Responses profile whose upstream serves only
// streaming requests.
func streamingManifest() abi.Manifest {
	manifest := pluginManifest()
	manifest.Profiles[0].Dialect = "openai-responses"
	manifest.Profiles[0].Hosting.ForceStreaming = true
	return manifest
}

func TestForcedStreamingNeedsADialectOLPAggregates(t *testing.T) {
	c := pluginConfig(t, streamingManifest())
	if p, _ := c.Profile(); p.Strict || !c.ForcesStreaming() {
		t.Fatalf("a profile forcing streaming serves strict routes: %+v", p)
	}
	published, encoded := publish(t, c)
	if !published.ForcesStreaming() || !reflect.DeepEqual(published.Plugin.hosting, c.Plugin.hosting) {
		t.Fatalf("forced streaming did not survive publication: %s", encoded)
	}
	if pluginConfig(t, pluginManifest()).ForcesStreaming() || (Config{}).ForcesStreaming() {
		t.Fatal("a connector without forced streaming forces it")
	}

	for _, dialect := range []string{"openai-chat", "anthropic-messages", "gemini-generate-content"} {
		p := streamingManifest().Profiles[0]
		p.Dialect = dialect
		refusal, ok := errors.AsType[*ProfileError](ValidatePluginProfile(p))
		if !ok || refusal.Field != "hosting.force_streaming" || !strings.HasSuffix(refusal.Message, ": openai-responses.") {
			t.Errorf("forced streaming in %s: %v", dialect, refusal)
		}
	}
}

func TestForcedStreamingMakesEveryRequestAStreamingOne(t *testing.T) {
	c := pluginConfig(t, streamingManifest())
	for body, want := range map[string]string{
		`{"model":"m","input":"hi"}`:                `{"model":"m","input":"hi","stream":true}`,
		`{"model":"m","input":"hi","stream":false}`: `{"model":"m","input":"hi","stream":true}`,
	} {
		streamed, err := c.StreamRequest(prepared(t, body))
		if err != nil {
			t.Fatal(err)
		}
		if got := streamed.Document().Raw(); got != want {
			t.Fatalf("streamed %s as %s, want %s", body, got, want)
		}
		changed := []string{}
		for _, entry := range streamed.Provenance() {
			if entry.Origin == forcedStreaming {
				changed = append(changed, entry.Pointer)
			}
		}
		if !reflect.DeepEqual(changed, []string{"", "/stream"}) {
			t.Fatalf("recorded forced streaming of %q", changed)
		}
	}

	streaming := prepared(t, `{"model":"m","input":"hi","stream":true}`)
	if same, err := c.StreamRequest(streaming); err != nil || !reflect.DeepEqual(same, streaming) {
		t.Fatalf("a streaming request changed: %v", err)
	}
	unary := prepared(t, `{"model":"m","input":"hi"}`)
	if same, err := pluginConfig(t, pluginManifest()).StreamRequest(unary); err != nil || !reflect.DeepEqual(same, unary) {
		t.Fatalf("a profile that does not force streaming changed a request: %v", err)
	}
}
