//go:build integration && codecli

package integration_test

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/tests/codecli"
	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

func TestCodeQualificationOfficialCLICompleteJourneysThroughFleet(t *testing.T) {
	for _, journey := range []string{"tools", "children", "compact"} {
		for _, ws := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/websocket=%t", journey, ws), func(t *testing.T) {
				f := newCodeFleet(t)
				f.peer.SetResponder(codexfixture.NewJourney(journey).Reply)
				client := codecli.New(t, f.gateways[0].PublicOrigin+"/code/qualification", f.key, ws)
				configuration := f.h.want(f.owner, "GET", "/api/v1/code/routes/"+f.route["id"].(string)+"/client-config?gateway_url="+url.QueryEscape(f.gateways[0].PublicOrigin), nil, nil, 200)
				if err := os.WriteFile(filepath.Join(client.Home, "config.toml"), []byte(configuration["configuration"].(string)), 0600); err != nil {
					t.Fatal(err)
				}
				flags := []string{"-c", `approval_policy="never"`, "-c", `sandbox_mode="read-only"`, "-c", `web_search="disabled"`, "-c", fmt.Sprintf("model_providers.olp.supports_websockets=%t", ws)}
				switch journey {
				case "children":
					flags = append(flags, "-c", "features.multi_agent_v2=true")
				case "compact":
					flags = append(flags, "-c", "model_auto_compact_token_limit=10000")
				}
				client.Exec(t, append(flags, "Complete the controlled fixture task.")...)
				if journey != "children" {
					client.Exec(t, append(flags, "resume", "--last", "Continue the controlled task.")...)
				}
				bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
				if len(bindings) == 0 {
					t.Fatal("official task has no durable pin")
				}
				for _, binding := range bindings {
					if binding.RootID != bindings[0].RootID || binding.AccountID != bindings[0].AccountID {
						t.Fatal("official task switched trees or accounts")
					}
				}
				requests := f.peer.Requests()
				if len(requests) < 3 {
					t.Fatal("official task missed continuation")
				}
				found := false
				for _, request := range requests {
					if request.WebSocket != ws || !bytes.Contains(request.Body, []byte(`"model":"gpt-5.4"`)) || request.Header.Get("Authorization") == "Bearer "+f.key {
						t.Fatal("native protocol or OLP authentication boundary changed")
					}
					switch journey {
					case "tools":
						found = found || bytes.Contains(request.Body, []byte("OLP_CONTROLLED_TOOL")) && bytes.Contains(request.Body, []byte("function_call_output"))
					case "children":
						found = found || request.Header.Get("X-Codex-Parent-Thread-Id") != ""
					case "compact":
						found = found || bytes.Contains(request.Body, []byte("compaction_trigger"))
					}
				}
				if !found || journey == "children" && len(bindings) < 2 {
					t.Fatal("official task did not exercise its journey")
				}
				if journey == "compact" {
					observed := false
					for _, attempt := range codePublicDecode[[]codemode.Attempt](t, f.list("attempts")) {
						observed = observed || attempt.Operation == "compact"
					}
					if !observed {
						t.Fatal("remote compaction was not classified in usage diagnostics")
					}
				}
			})
		}
	}
}
