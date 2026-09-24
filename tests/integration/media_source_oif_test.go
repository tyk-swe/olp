//go:build integration

package integration_test

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNativeMediaSourceRejectsNestedAmbiguityBeforeDispatch(t *testing.T) {
	// Vertex's public certification accepts a scripted local image provider.
	// No personal ADC file or live model endpoint is used by this fixture.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(home, ".config/gcloud/application_default_credentials.json")); err == nil {
		t.Fatal("run local image qualification without personal gcloud credentials")
	}
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Metadata-Flavor", "Google")
		if r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/token" {
			writeJSON(w, map[string]any{"access_token": "adc-fixture-token", "token_type": "Bearer", "expires_in": 3600})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(metadata.Close)
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	pixel := base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4e, 0x47})
	var predictions atomic.Int64
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":countTokens") {
			writeJSON(w, map[string]any{"totalTokens": 5})
			return
		}
		if strings.HasSuffix(r.URL.Path, ":predict") {
			predictions.Add(1)
			writeJSON(w, map[string]any{"predictions": []any{map[string]string{"bytesBase64Encoded": pixel, "mimeType": "image/png"}}})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(provider.Close)
	h := newAccessHarness(t)
	slug, secret := provisionRoute(t, h,
		map[string]any{"kind": "vertex_ai", "auth_mode": "adc", "endpoint": provider.URL + "/v1/projects/fixture-project/locations/us-central1/publishers/google", "cloud_project": "fixture-project", "cloud_region": "us-central1"},
		nil, "imagen-3.0-generate-002",
		[]any{map[string]any{"operation": "image_generation", "surface": "openai", "mode": "unary"}},
		[]string{"image_generation"})
	before := predictions.Load()
	valid := []byte(`{"model":"` + slug + `","prompt":"a picture","n":1,"size":"1536x1024","response_format":"b64_json"}`)
	status, reply, _ := h.gatewayRaw(http.MethodPost, "/v1/images/generations", secret, bytes.NewReader(valid), map[string]string{"Content-Type": "application/json"})
	if status != http.StatusOK || !bytes.Contains(reply, []byte(pixel)) || predictions.Load() != before+1 {
		t.Fatalf("valid native image source was not served: status=%d dispatches=%d", status, predictions.Load()-before)
	}
	before = predictions.Load()
	for _, suffix := range []string{
		`"native_extension":{"path":1,"\u0070ath":2}`,
		`"native_extension":{"outer":{"x":1,"x":2}}`,
		`"native_extension":"\ud800"`,
	} {
		body := []byte(`{"model":"` + slug + `","prompt":"a picture",` + suffix + `}`)
		status, rejected, _ := h.gatewayRaw(http.MethodPost, "/v1/images/generations", secret, bytes.NewReader(body), map[string]string{"Content-Type": "application/json"})
		if status != http.StatusBadRequest || !bytes.Contains(rejected, []byte(`"code":"invalid_request"`)) {
			t.Fatalf("ambiguous native source escaped source validation: %d %s", status, rejected)
		}
	}
	if predictions.Load() != before {
		t.Fatal("ambiguous media source reached the provider")
	}
	if bytes.Contains(reply, []byte("native_extension")) {
		t.Fatal("native private extension leaked in client response")
	}
}
