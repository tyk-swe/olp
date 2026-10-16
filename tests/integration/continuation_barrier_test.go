//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/resources"
)

func continuationBarrierFixture(t *testing.T, h *accessHarness, run func(http.ResponseWriter, *http.Request, []byte)) (string, string) {
	t.Helper()
	return continuationBarrierFixtureOwner(t, h, h.owner(), run)
}

func continuationBarrierFixtureOwner(t *testing.T, h *accessHarness, owner *browser, run func(http.ResponseWriter, *http.Request, []byte)) (string, string) {
	t.Helper()
	f := &strictProviderFixture{profile: "anthropic-messages"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if bytes.Contains(body, []byte("Weather and time in Paris?")) {
			run(w, r, body)
			return
		}
		var input map[string]json.RawMessage
		_ = json.Unmarshal(body, &input)
		kindGeneration(w, "anthropic", string(input["stream"]) == "true")
	}))
	t.Cleanup(f.Close)
	options := map[string]any{"bindings": map[string]any{vendorModel: map[string]any{"model": "fixture-model"}}, "operation_defaults": map[string]any{"generation": map[string]any{"dialect": "anthropic-messages", "values": map[string]any{"max_tokens": 2048, "thinking": map[string]any{"type": "enabled", "budget_tokens": 1024}}}}}
	slug, _ := publishStrictProvider(t, h, owner, f, options, nil, "strict")
	key := stateKey(t, h, owner, slug, true)
	h.refresh()
	return slug, key
}
func continuationHeaders() map[string]string {
	return map[string]string{"Content-Type": "application/json", "X-OLP-Continuation": continuationClientVersion, "X-OLP-Submission-ID": resources.SubmissionID(time.Now(), uuid.New())}
}

// Claim+dispatch are already durable. Holding the installation key lock now
// prevents the final encryption/ready transaction from committing.
