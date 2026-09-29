package connectors

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// recordingCarrier answers every carried request with a 418 response, recording
// what it was asked to carry.
type recordingCarrier struct {
	digest   string
	provider abi.Provider
	url      string
	secrets  []string
}

func (c *recordingCarrier) Carry(_ context.Context, digest string, provider abi.Provider, req *http.Request, secrets []string) (*http.Response, error) {
	c.digest, c.provider, c.url, c.secrets = digest, provider, req.URL.String(), secrets
	return &http.Response{StatusCode: http.StatusTeapot, Header: http.Header{}, Body: http.NoBody}, nil
}

// A profile whose unconfined plugin carries its traffic serves only
// transformed routes, and its connector sends every finished request through
// the plugin, for the provider it serves.
func TestACarriedProfileSendsItsRequestsThroughItsPlugin(t *testing.T) {
	manifest := optionsManifest()
	manifest.Profiles[0].CarriesTraffic = true
	plugin, err := NewUnconfinedPluginProfile(pluginDigest, manifest, "acme-chat")
	if err != nil {
		t.Fatal(err)
	}
	if profile := plugin.Profile(); profile.Strict || profile.Transport != "plugin" {
		t.Fatalf("a carried profile is strict %v over %s", profile.Strict, profile.Transport)
	}
	options := map[string]string{"account": "acme", "region": "eu"}
	cfg := Config{Plugin: plugin, Kind: KindPlugin, AuthMode: AuthStaticCredential, ProfileID: "acme-chat", ProfileRevision: pluginDigest, PluginOptions: options}
	if !cfg.CarriedByPlugin() || pluginConfig(t, pluginManifest()).CarriedByPlugin() {
		t.Fatal("a connector's plugin carries its traffic only where its profile declares it")
	}
	carrier := &recordingCarrier{}
	resp, err := cfg.CarrierClient(carrier, []string{"secret"}).Get("https://api.acme.example/accounts/acme/v2/models")
	if err != nil || resp.StatusCode != http.StatusTeapot {
		t.Fatalf("carried %v: %v", resp, err)
	}
	want := recordingCarrier{digest: pluginDigest, provider: abi.Provider{Profile: "acme-chat", Options: options}, url: "https://api.acme.example/accounts/acme/v2/models", secrets: []string{"secret"}}
	if !reflect.DeepEqual(*carrier, want) {
		t.Fatalf("the plugin carried %+v, want %+v", *carrier, want)
	}
	// A process that runs no plugins sends nothing.
	_, err = cfg.CarrierClient(nil, nil).Post("https://api.acme.example/accounts/acme/v2/chat/completions", "application/json", strings.NewReader("{}"))
	if !errors.Is(err, ErrNotSent) {
		t.Fatalf("a request no plugin carries failed with %v", err)
	}
}
