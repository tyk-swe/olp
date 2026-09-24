//go:build integration

package integration_test

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestGeminiInteractionRefusesEscapedNativeResourceIDs(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", provider)
	path := "/gemini/v1beta/interactions"
	request := []byte(fmt.Sprintf(`{"model":%q,"input":"identity guard","store":true}`, slug))
	provider.escapedCreateID.Store(true)
	response, raw := geminiPublic(t, h, http.MethodPost, path, key, request)
	if response.StatusCode != http.StatusBadGateway || !bytes.Contains(raw, []byte("provider_protocol_error")) || bytes.Contains(raw, []byte("v1_fixture_")) {
		t.Fatalf("escaped native ID crossed create boundary: %d %s", response.StatusCode, raw)
	}
	provider.escapedCreateID.Store(false)
	response, raw = geminiPublic(t, h, http.MethodPost, path, key, request)
	local, ok := jsonStringField(raw, "id")
	if response.StatusCode != http.StatusOK || !ok || !strings.HasPrefix(local, "interaction_") {
		t.Fatalf("normal Interaction could not be retained: %d %s", response.StatusCode, raw)
	}
	provider.escapedReadID.Store(true)
	response, raw = geminiPublic(t, h, http.MethodGet, path+"/"+local, key, nil)
	if response.StatusCode != http.StatusBadGateway || !bytes.Contains(raw, []byte("provider_protocol_error")) || bytes.Contains(raw, []byte("v1_fixture_")) {
		t.Fatalf("escaped native ID crossed unary retrieval: %d %s", response.StatusCode, raw)
	}
	provider.escapedReadID.Store(false)
	provider.escapedStreamID.Store(true)
	response, raw = geminiPublic(t, h, http.MethodGet, path+"/"+local+"?stream=true", key, nil)
	if response.StatusCode != http.StatusBadGateway || !bytes.Contains(raw, []byte("interaction_incomplete")) || bytes.Contains(raw, []byte("v1_fixture_")) {
		t.Fatalf("escaped native ID crossed streamed retrieval: %d %s", response.StatusCode, raw)
	}
	provider.escapedStreamID.Store(false)
	response, raw = geminiPublic(t, h, http.MethodGet, path+"/"+local, key, nil)
	if response.StatusCode != http.StatusOK || !bytes.Contains(raw, []byte(`"id":"`+local+`"`)) {
		t.Fatalf("failed provider projection corrupted the retained identity: %d %s", response.StatusCode, raw)
	}
}
