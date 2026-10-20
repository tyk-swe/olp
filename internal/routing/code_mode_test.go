package routing

import (
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
)

func TestCodeCapabilitiesCompilePerRevision(t *testing.T) {
	connection := func(profile string) Configuration {
		return Configuration{Kind: "plugin", AuthMode: "grant", ProfileID: profile}
	}
	s := Snapshot{CodeConnections: map[string]Configuration{
		"revision:codex":    connection(codexauth.ProfileID),
		"revision:go":       connection(codeplans.OpenCodeGoProfile),
		"revision:zai":      connection(codeplans.ZAIProfile),
		"revision:bigmodel": connection(codeplans.BigModelProfile),
		"revision:unknown":  connection("unknown"),
		"other:another":     connection(codexauth.ProfileID),
	}}
	if err := s.compileCodeMode(); err != nil {
		t.Fatal(err)
	}
	route := codemode.Route{RevisionID: "revision"}
	for _, test := range []struct {
		path      string
		transport codeadapter.Transport
		protocol  codemode.Protocol
		model     string
		providers []string
	}{
		{"responses", codeadapter.HTTP, codemode.ProtocolResponses, "gpt-5.5", []string{"codex"}},
		{"responses", codeadapter.WebSocket, codemode.ProtocolResponses, "gpt-5.5", []string{"codex"}},
		{"responses/compact", codeadapter.WebSocket, codemode.ProtocolResponses, "gpt-5.5", nil},
		{"v1/responses", codeadapter.WebSocket, codemode.ProtocolResponses, "gpt-5.5", nil},
		{"v1/responses", codeadapter.HTTP, codemode.ProtocolResponses, "gpt-5.5", []string{"go"}},
		{"v1/responses", codeadapter.HTTP, codemode.ProtocolResponses, "glm-5.3", nil},
		{"v1/messages", codeadapter.HTTP, codemode.ProtocolMessages, "qwen-4", []string{"bigmodel", "go", "zai"}},
		{"v1/messages", codeadapter.HTTP, codemode.ProtocolMessages, "glm-5.3", []string{"bigmodel", "zai"}},
		{"v1/messages", codeadapter.HTTP, codemode.ProtocolChat, "qwen-4", nil},
		{"v1/chat/completions", codeadapter.HTTP, codemode.ProtocolChat, "glm-5.3", []string{"bigmodel", "go", "zai"}},
	} {
		if got := s.CodeModelProviders(route, test.path, test.protocol, test.model, test.transport); !slices.Equal(got, test.providers) {
			t.Fatalf("%s/%d/%s/%s: %v", test.path, test.transport, test.protocol, test.model, got)
		}
	}
	if got := s.CodeProviders(route, "v1/messages", codeadapter.HTTP); !slices.Equal(got, []string{"bigmodel", "go", "zai"}) {
		t.Fatalf("model filtering changed compiled providers: %v", got)
	}
}
