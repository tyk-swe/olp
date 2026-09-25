package observability

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Bedrock, native-dialect and unprefixed Gemini Live requests are gateway
// inference, so the console's full management pool must not refuse them.
func TestGatewayInferenceSurfacesUseInferencePool(t *testing.T) {
	for path, want := range map[string]string{
		"/bedrock/model/route/converse":  "bedrock",
		"/native/anthropic/models/route": "native",
		"/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent": "gemini",
	} {
		if surface, inference := SurfaceFromPath(path); surface != want || !inference {
			t.Errorf("%s classified as %q (inference %t)", path, surface, inference)
		}
		inference, management := NewPool(1), NewPool(1)
		held := management.AcquirePermit()
		served := false
		admission := &PublicAdmission{
			Inference: inference, Management: management, InferenceEnabled: true,
			Reject: func(w http.ResponseWriter, _ *http.Request, _ string) { w.WriteHeader(http.StatusServiceUnavailable) },
		}
		admission.Wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true })).
			ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
		held.Release()
		if !served {
			t.Errorf("%s was refused while inference capacity was free", path)
		}
	}
}
