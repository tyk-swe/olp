package research_test

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/code/research"
)

func TestRequestResponsePingAndEventSealing(t *testing.T) {
	rewritten, method, err := research.Request([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true}}}}`), "search")
	if err != nil || method != "initialize" {
		t.Fatalf("initialize request = %s, %q, %v", rewritten, method, err)
	}
	if _, _, err := research.Request([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{}}}`), "search"); err == nil {
		t.Fatal("disallowed tool call accepted")
	}
	filtered, err := research.Response([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"webSearchPrime"},{"name":"forbiddenTool"}]}}`), "tools/list", "search")
	if err != nil {
		t.Fatalf("Response = %v", err)
	}
	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(filtered, &parsed); err != nil || len(parsed.Result.Tools) != 1 || parsed.Result.Tools[0].Name != "webSearchPrime" {
		t.Fatalf("filtered tools = %+v (%v)", parsed, err)
	}
	pingReply, err := research.PingResponse([]byte(`{"jsonrpc":"2.0","id":"p1","method":"ping"}`))
	if err != nil || len(pingReply) == 0 {
		t.Fatalf("PingResponse = %s, %v", pingReply, err)
	}
	sealed, err := research.SealEventID("session-a", "evt-42", "Bearer secret")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := research.OpenEventID("session-a", sealed, "Bearer secret")
	if err != nil || opened != "evt-42" {
		t.Fatalf("OpenEventID = %q, %v", opened, err)
	}
	if _, err := research.OpenEventID("session-b", sealed, "Bearer secret"); err == nil {
		t.Fatal("cross-session event ID opened")
	}
}
