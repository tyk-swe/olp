package connectors

import "testing"

func TestRetainedAgentConnectorScope(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		want bool
	}{
		{Config{Kind: "gemini"}, false},
		{Config{Kind: "gemini", ProfileID: "gemini-interactions", ProfileRevision: ProfileRevision}, false},
		{Config{Kind: "gemini", ProfileID: "gemini-agents", ProfileRevision: ProfileRevision}, true},
		{Config{Kind: "gemini", ProfileID: "gemini-live", ProfileRevision: ProfileRevision}, false},
		{Config{Kind: "gemini", ProfileID: "gemini-interactions", ProfileRevision: "unknown"}, false},
		{Config{Kind: "openai_compatible"}, false},
		{Config{Kind: "vertex_ai"}, false},
	} {
		if got := tc.cfg.Supports("agent_resource", "gemini", "unary"); got != tc.want {
			t.Errorf("%+v: got %v", tc.cfg, got)
		}
	}
	if Supports("gemini", "custom", "agent_resource", "gemini", "unary") {
		t.Fatal("unreviewed vendor gained retained operations")
	}
	if (Config{Kind: "gemini"}).Supports("agent_resource", "gemini", "streaming") {
		t.Fatal("resource operation gained streaming")
	}
}

func TestRetainedAgentProfile(t *testing.T) {
	cfg := Config{Kind: "gemini", VendorID: "google", ProfileID: "gemini-agents", ProfileRevision: ProfileRevision, AuthMode: "api_key", Endpoint: "https://generativelanguage.googleapis.com/v1beta"}
	if err := cfg.ValidateProfile(); err != nil {
		t.Fatal(err)
	}
	endpoint, err := cfg.ResourceURL("gemini-2.5-flash", "agents", nil)
	if err != nil || endpoint != "https://generativelanguage.googleapis.com/v1beta/agents" {
		t.Fatalf("%s: %v", endpoint, err)
	}
	if cfg.Supports("generation", "gemini", "unary") {
		t.Fatal("managed-agent profile enabled generation")
	}
	cfg.OperationDefaults = map[string]DefaultSet{"arbitrary": {}}
	if err := cfg.ValidateProfile(); err == nil {
		t.Fatal("unbound defaults enabled")
	}
}

func TestVertexTuningResourceURL(t *testing.T) {
	cfg := Config{Kind: "vertex_ai", Endpoint: "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/publishers/google"}
	for path, want := range map[string]string{
		"tuningJobs":              "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/tuningJobs",
		"tuningJobs/job-1:cancel": "https://us-central1-aiplatform.googleapis.com/v1/projects/p/locations/us-central1/tuningJobs/job-1:cancel",
	} {
		got, err := cfg.ResourceURL("gemini-2.5-flash", path, nil)
		if err != nil || got != want {
			t.Fatalf("%s = %q, %v; want %q", path, got, err, want)
		}
	}
}
