package connectors

import (
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestCustomNativeHeadersPreserveConfiguredProtocolVersion(t *testing.T) {
	auth := NewAuth(&egress.Policy{})
	request, _ := http.NewRequest("POST", "https://api.anthropic.com/v1/messages", nil)
	config := Config{Kind: "anthropic", AuthMode: "headers", CredentialHeaders: []string{"X-Api-Key", "Anthropic-Version", "Anthropic-Beta"}}
	secret := []byte(`{"X-Api-Key":"custom-secret","Anthropic-Version":"custom-version","Anthropic-Beta":"custom-beta"}`)
	if _, err := auth.Apply(t.Context(), request, config, secret, nil); err != nil {
		t.Fatal(err)
	}
	if request.Header.Get("X-Api-Key") != "custom-secret" || request.Header.Get("Anthropic-Version") != "custom-version" || request.Header.Get("Anthropic-Beta") != "custom-beta" {
		t.Fatal("native defaults replaced explicitly configured headers")
	}
}
