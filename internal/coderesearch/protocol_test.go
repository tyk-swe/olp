package coderesearch

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResearchRequestBoundary(t *testing.T) {
	for _, test := range []struct {
		service, body string
		allowed       bool
	}{
		{"search", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"webSearchPrime","arguments":{"search_query":"Go docs"}}}`, true},
		{"reader", `{"jsonrpc":"2.0","id":"r","method":"tools/call","params":{"name":"webReader"}}`, true},
		{"zread", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file"}}`, true},
		{"search", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file"}}`, false},
		{"search", `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, false},
		{"search", `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`, true},
		{"search", `{"jsonrpc":"2.0","id":1,"method":"notifications/cancelled"}`, false},
		{"search", `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"webSearchPrime"}}`, false},
		{"search", `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, false},
		{"search", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"webSearchPrime","name":"read_file"}}`, false},
		{"search", `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`, false},
		{"search", `{"jsonrpc":"2.0","id":1,"method":"ping","result":{"roots":[]}}`, false},
	} {
		got, _, err := Request([]byte(test.body), test.service)
		if (err == nil) != test.allowed || test.allowed && string(got) != test.body {
			t.Errorf("%s %s: %s %v", test.service, test.body, got, err)
		}
	}
	raw, method, err := Request([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"roots":{"listChanged":true},"sampling":{}},"clientInfo":{"name":"test","version":"1"}}}`), "search")
	if err != nil || method != "initialize" || strings.Contains(string(raw), "sampling") || strings.Contains(string(raw), "roots") || !strings.Contains(string(raw), `"clientInfo"`) {
		t.Fatalf("initialization: %s %v", raw, err)
	}
}

func TestResearchResumeIDsBindSessionAndCredential(t *testing.T) {
	const event = "native:42/é"
	token, err := SealEventID("session-one", event, "vendor-bearer-one")
	if err != nil {
		t.Fatal(err)
	}
	if decoded, err := OpenEventID("session-one", token, "vendor-bearer-one"); err != nil || decoded != event {
		t.Fatalf("native resume ID: %q %v", decoded, err)
	}
	if replica, _ := SealEventID("session-one", event, "vendor-bearer-one"); replica != token {
		t.Fatal("resume ID is not stable across replicas")
	}
	for _, test := range []struct{ session, credential, token string }{
		{"session-two", "vendor-bearer-one", token},
		{"session-one", "vendor-bearer-two", token},
		{"session-one", "vendor-bearer-one", token + "tampered"},
		{"session-one", "vendor-bearer-one", "native:42"},
	} {
		if _, err := OpenEventID(test.session, test.token, test.credential); err == nil {
			t.Fatal("resume ID crossed its session or credential boundary")
		}
	}
	if _, err := SealEventID("session", strings.Repeat("x", 1025), "bearer"); err == nil {
		t.Fatal("oversized resume ID accepted")
	}
}

func TestResearchNativeDiscoveryAndResults(t *testing.T) {
	tool := `{"name":"webSearchPrime","description":"Search docs","inputSchema":{"type":"object","properties":{"query":{"type":"string"}}},"outputSchema":{"type":"object"},"annotations":{"readOnlyHint":true},"_meta":{"native":true}}`
	raw := `{"jsonrpc":"2.0","id":1,"result":{"tools":[` + tool + `,{"name":"vision"}],"nextCursor":"page2","_meta":{"page":1}}}`
	for _, method := range []string{"tools/list", ""} {
		got, err := Response([]byte(raw), method, "search")
		var decoded struct {
			Result struct {
				Tools []json.RawMessage `json:"tools"`
				Next  string            `json:"nextCursor"`
			} `json:"result"`
		}
		if err != nil || json.Unmarshal(got, &decoded) != nil || len(decoded.Result.Tools) != 1 || string(decoded.Result.Tools[0]) != tool || decoded.Result.Next != "page2" {
			t.Fatalf("discovery: %s %v", got, err)
		}
	}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"source https://example.com"}],"structuredContent":{"answer":"result","sources":["https://example.com"]},"isError":true}}`,
		`{"jsonrpc":"2.0","id":2,"error":{"code":-32000,"message":"Vendor error","data":{"retry":false}}}`,
	} {
		if got, err := Response([]byte(raw), "tools/call", "search"); err != nil || string(got) != raw {
			t.Fatalf("native result changed: %s %v", got, err)
		}
	}
	got, err := Response([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","serverInfo":{"name":"native","version":"1"},"capabilities":{"tools":{"listChanged":true},"resources":{},"prompts":{}}}}`), "initialize", "search")
	if err != nil || strings.Contains(string(got), "resources") || strings.Contains(string(got), "prompts") || !strings.Contains(string(got), `"listChanged":true`) {
		t.Fatalf("capabilities: %s %v", got, err)
	}
	got, err = Response([]byte(`{"jsonrpc":"2.0","id":9,"method":"sampling/createMessage","params":{}}`), "", "search")
	if err != nil || got != nil {
		t.Fatalf("unexpected client capability: %s %v", got, err)
	}
}
