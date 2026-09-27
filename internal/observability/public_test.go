package observability

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPublicAdmissionChargesEveryInferenceSurfaceToTheInferencePool(t *testing.T) {
	inference, management := NewPool(1), NewPool(1)
	admission := &PublicAdmission{Inference: inference, Management: management, InferenceEnabled: true,
		Reject: func(w http.ResponseWriter, r *http.Request, surface string) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}}
	var charged *Pool
	handler := admission.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case inference.Admitted() == 1:
			charged = inference
		case management.Admitted() == 1:
			charged = management
		}
	}))
	for path, want := range map[string]*Pool{
		"/v1/chat/completions":      inference,
		"/native/gemini/models/x":   inference,
		"/bedrock/model/x/converse": inference,
		"/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent": inference,
		"/anthropic/v1/messages":  inference,
		"/gemini/v1beta/models/x": inference,
		"/api/v1/providers":       management,
		"/":                       management,
	} {
		charged = nil
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, path, nil))
		if charged != want {
			t.Errorf("%s was charged to the wrong admission pool", path)
		}
	}

	admission.InferenceEnabled = false
	charged = nil
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/bedrock/model/x/converse", nil))
	if charged != management {
		t.Fatal("a process without inference must charge every request to the management pool")
	}
}
