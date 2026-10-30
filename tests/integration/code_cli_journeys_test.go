//go:build integration && codecli

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
				client.UseConfiguration(t, configuration["configuration"].(string))
				flags := codecli.ControlledFlags(ws)
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

func TestCodeQualificationMessagesToolsOfficialSDK(t *testing.T) {
	f := newCodeFleet(t)
	path := "/api/v1/code/routes/" + f.route["id"].(string)
	draft := f.h.want(f.owner, "PUT", path, map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": []string{"gpt-5.4"}, "enabled": true, "messages_to_codex": true}, etagHeader(f.route), 200)
	f.h.want(f.owner, "POST", path+"/publish", nil, withMatch(draft, idem("messages-tools-sdk")), 200)
	gateway := f.in.replica("messages-sdk")
	frames := func(events ...string) [][]byte {
		return slices.Collect(func(yield func([]byte) bool) {
			for _, event := range events {
				if !yield([]byte("data: " + event + "\n\n")) {
					return
				}
			}
		})
	}
	f.peer.SetResponder(func(_ context.Context, header http.Header, body []byte) [][]byte {
		created := frames(`{"type":"response.created","response":{"id":"sdk-tools","status":"in_progress","output":[]}}`)
		if bytes.Contains(body, []byte(`"function_call_output"`)) {
			return slices.Concat(created, frames(`{"type":"response.completed","response":{"id":"sdk-tools","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"File read"}]}],"usage":{"input_tokens":13,"output_tokens":2,"total_tokens":15}}}`))
		}
		if strings.HasSuffix(header.Get("X-Claude-Code-Session-Id"), "complete") {
			return slices.Concat(created, frames(`{"type":"response.completed","response":{"id":"sdk-tools","status":"completed","output":[{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{\"path\":\"main.go\"}"}],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}`))
		}
		return slices.Concat(created, frames(
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{\"path\":"}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"\"main.go\"}"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{\"path\":\"main.go\"}"}}`,
			`{"type":"response.completed","response":{"id":"sdk-tools","status":"completed","output":[],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}}`,
		))
	})
	dir, err := filepath.Abs("../clients")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "go", "test", "-mod=readonly", "-tags=integration,codemode", "-timeout=60s", "./gosdk", "-run=^TestCodeMessagesToolExchange$", "-count=1")
	command.Dir = dir
	command.Env = append(os.Environ(), "OLP_CODE_MESSAGES_BASE_URL="+gateway.PublicOrigin+"/code/qualification", "OLP_CODE_MESSAGES_API_KEY="+f.key, "OLP_CODE_MESSAGES_MODEL=gpt-5.4")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("official Messages SDK %v\n%s", err, output)
	}
	requests := f.peer.Requests()
	if len(requests) != 8 {
		t.Fatalf("SDK tool exchanges dispatched %d requests, want eight", len(requests))
	}
	for _, request := range requests {
		if !bytes.Contains(request.Body, []byte(`"function_call_output"`)) {
			continue
		}
		if !bytes.Contains(request.Body, []byte(`"call_id":"call-read"`)) || !bytes.Contains(request.Body, []byte(`"arguments":"{\"path\":\"main.go\"}"`)) || !bytes.Contains(request.Body, []byte(`"output":"package main"`)) {
			t.Fatalf("SDK tool history changed %s", request.Body)
		}
	}
}
