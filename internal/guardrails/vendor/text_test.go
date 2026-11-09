package vendor

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
)

func TestIBMOrchestratorThreshold(t *testing.T) {
	server := serve(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || r.Header.Get("Authorization") != "Bearer "+secret || r.Header.Get("detector-id") != "" {
			t.Error("incorrect orchestrator authentication")
		}
		if body["content"] != "private" || !reflect.DeepEqual(body["detectors"], map[string]any{"pii": map[string]any{"language": "en"}}) {
			t.Errorf("incorrect orchestrator request: %+v", body)
		}
		_, _ = io.WriteString(w, `{"detections":[{"detection_type":"pii","detection":"email","score":0.2}]}`)
	})
	decision, err := evaluate(t, server, "ibm_guardrails", configuration(server.URL, `"parameters":{"detector_id":"pii","orchestrator":true,"score_threshold":0.5,"detector_params":{"language":"en"}}`), Request{Role: User, Text: "private"})
	if err != nil || decision.Flagged() {
		t.Fatalf("threshold ignored: %+v, %v", decision, err)
	}
}
