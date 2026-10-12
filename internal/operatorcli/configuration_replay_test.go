package operatorcli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSavedConfigurationIntentIsPinnedWithoutReplanning(t *testing.T) {
	planned, applied := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/configuration/export":
			_, _ = w.Write([]byte(`{"digest":"destination","document":{}}`))
		case "/api/v1/configuration/plan", "/api/v1/configuration/apply":
			var request map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			if !strings.Contains(string(request["document"]), "9007199254740993") || !strings.Contains(string(request["external_credential_bindings"]), "9007199254740993") {
				t.Error("exact integer intent or reference pin was rounded")
			}
			if r.URL.Path == "/api/v1/configuration/plan" {
				planned++
			} else {
				applied++
				if r.Header.Get("Idempotency-Key") != "pinned-apply" || string(request["expected_digest"]) != `"destination"` {
					t.Error("original logical mutation key/precondition changed")
				}
			}
			_, _ = w.Write([]byte(`{"digest":"desired","actions":[],"conflicts":[],"blockers":[]}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	runner, _ := testRunner(t, server)
	dir := t.TempDir()
	document, references, plan := filepath.Join(dir, "document.json"), filepath.Join(dir, "references.json"), filepath.Join(dir, "plan.json")
	for path, content := range map[string]string{
		document:   `{"count":9007199254740993,"ratio":0.0000000000001}`,
		references: `{"provider":{"version":9007199254740993}}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := runner.Run(t.Context(), []string{"config", "plan", "--file", document, "--external-bindings-file", references, "--output", plan}); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(plan)
	if err != nil {
		t.Fatal(err)
	}
	var original SavedPlan
	if err := json.Unmarshal(encoded, &original); err != nil || original.Version != 2 || original.IntentDigest == "" {
		t.Fatalf("missing versioned intent consistency: version=%d err=%v", original.Version, err)
	}
	args := []string{"config", "apply", "--plan-file", plan, "--external-bindings-file", references, "--idempotency-key", "pinned-apply"}
	for _, test := range []struct {
		name   string
		mutate func(*SavedPlan)
	}{
		{"desired document", func(saved *SavedPlan) { saved.Document = json.RawMessage(`{"count":9007199254740994}`) }},
		{"desired plan", func(saved *SavedPlan) { saved.Plan.Digest = "another-intent" }},
		{"original precondition", func(saved *SavedPlan) { saved.DestinationDigest = "another-snapshot" }},
		{"reference pin", func(saved *SavedPlan) { saved.ExternalBindingsDigest = "another-reference" }},
		{"old format", func(saved *SavedPlan) { saved.Version = 1 }},
		{"missing consistency", func(saved *SavedPlan) { saved.IntentDigest = "" }},
		{"other destination", func(saved *SavedPlan) {
			saved.Destination = "https://another-installation.example"
			saved.IntentDigest, _ = savedIntentDigest(*saved)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			saved := original
			test.mutate(&saved)
			changed, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(plan, changed, 0600); err != nil {
				t.Fatal(err)
			}
			if err := runner.Run(t.Context(), args); err == nil || applied != 0 || planned != 1 {
				t.Fatalf("changed intent dispatched: applied=%d planned=%d err=%v", applied, planned, err)
			}
		})
	}
	// Whole-file and document indentation do not change the pinned intent.
	original.Document = json.RawMessage(`{ "count": 9007199254740993, "ratio": 0.0000000000001 }`)
	encoded, err = json.MarshalIndent(original, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(t.Context(), args); err != nil || applied != 1 || planned != 1 {
		t.Fatalf("original intent did not dispatch directly: applied=%d planned=%d err=%v", applied, planned, err)
	}
}

func TestSavedIntentPreservesNativeConfigurationMemberOrder(t *testing.T) {
	saved := SavedPlan{Version: 2, Document: json.RawMessage(`{"providers":[{"configuration":{"options":{"parameter_defaults":{"response_format":{"json_schema":{"schema":{"type":"object","properties":{"first":{"type":"string"},"second":{"type":"string"}}}}}}}}}]}`)}
	first, err := savedIntentDigest(saved)
	if err != nil {
		t.Fatal(err)
	}
	saved.Document = json.RawMessage(`{"providers":[{"configuration":{"options":{"parameter_defaults":{"response_format":{"json_schema":{"schema":{"type":"object","properties":{"second":{"type":"string"},"first":{"type":"string"}}}}}}}}}]}`)
	second, err := savedIntentDigest(saved)
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("reordered native schema properties retained the saved intent checksum")
	}
}
