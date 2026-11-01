package surface

import "testing"

func TestMCPConsumerEndpointIsInferenceNotConsole(t *testing.T) {
	for _, path := range []string{"/mcp", "/mcp/"} {
		if got := Of(path); !got.Inference {
			t.Fatalf("%s classified outside inference: %+v", path, got)
		}
	}
	reserved := false
	for _, prefix := range Reserved() {
		if prefix.Path == "/mcp" {
			reserved = prefix.Surface.Inference
		}
	}
	if !reserved {
		t.Fatal("the console may answer /mcp")
	}
	if got := Of("/api/v1/mcp"); got.Inference {
		t.Fatalf("management MCP moved to inference: %+v", got)
	}
}
