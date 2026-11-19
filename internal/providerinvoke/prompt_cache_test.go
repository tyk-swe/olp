package providerinvoke

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
)

func preparedPromptCache(t testing.TB, format wire.Format, kind connectors.Kind, body string) (Invocation, connectors.Config) {
	t.Helper()
	doc, err := oif.ParseJSON([]byte(body), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	request, err := wire.NewRequest(format, "team-chat", false, doc)
	if err != nil {
		t.Fatal(err)
	}
	config := connectors.Config{Kind: kind}
	inv, err := Prepare(request, config, "model-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	return inv, config
}

func decodedBody(t *testing.T, inv Invocation) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(inv.Prepared.Document().Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func lastBlockControl(t *testing.T, value any) any {
	t.Helper()
	blocks, ok := value.([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("expected content blocks, got %#v", value)
	}
	return blocks[len(blocks)-1].(map[string]any)["cache_control"]
}

func TestApplyPromptCacheAnthropicBreakpoints(t *testing.T) {
	ephemeral := map[string]any{"type": "ephemeral", "ttl": "5m"}
	tools := `"tools":[{"name":"a","input_schema":{"type":"object"}},{"name":"b","input_schema":{"type":"object","properties":{"cache_control":{"type":"string"}}}}]`
	cached := func(ttl string) string { return `,"cache_control":{"type":"ephemeral","ttl":"` + ttl + `"}` }
	userTurn := func(extra string) string {
		return `{"role":"user","content":[{"type":"text","text":"first"` + extra + `}]}`
	}
	for _, tc := range []struct {
		name string
		body string
		want func(t *testing.T, body map[string]any)
	}{
		{
			name: "default positions mark tools, system and the last user turn",
			body: `{"max_tokens":16,"system":"be brief",` + tools + `,"messages":[` + userTurn("") + `,{"role":"assistant","content":"ok"},{"role":"user","content":"again"}]}`,
			want: func(t *testing.T, body map[string]any) {
				if got := lastBlockControl(t, body["tools"]); !reflect.DeepEqual(got, ephemeral) {
					t.Fatalf("tools breakpoint = %#v", got)
				}
				if got := lastBlockControl(t, body["system"]); !reflect.DeepEqual(got, ephemeral) {
					t.Fatalf("system breakpoint = %#v", got)
				}
				messages := body["messages"].([]any)
				if got := lastBlockControl(t, messages[2].(map[string]any)["content"]); !reflect.DeepEqual(got, ephemeral) {
					t.Fatalf("last user breakpoint = %#v", got)
				}
				if _, marked := messages[0].(map[string]any)["content"].([]any)[0].(map[string]any)["cache_control"]; marked {
					t.Fatal("an earlier user turn was marked")
				}
			},
		},
		{
			name: "caller breakpoints count toward the limit of four",
			body: `{"max_tokens":16,"system":[{"type":"text","text":"s"},{"type":"text","text":"t"}],` + tools + `,"messages":[` + userTurn(cached("5m")) + `,{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"a","input":{}` + cached("5m") + `}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[{"type":"text","text":"result"` + cached("5m") + `}]}]},{"role":"user","content":"again"}]}`,
			want: func(t *testing.T, body map[string]any) {
				if got := lastBlockControl(t, body["tools"]); !reflect.DeepEqual(got, ephemeral) {
					t.Fatalf("tools breakpoint = %#v", got)
				}
				if got := lastBlockControl(t, body["system"]); got != nil {
					t.Fatalf("system marked past the limit: %#v", got)
				}
				messages := body["messages"].([]any)
				if content, ok := messages[3].(map[string]any)["content"].(string); !ok || content != "again" {
					t.Fatalf("last user turn changed past the limit: %#v", messages[3])
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, config := preparedPromptCache(t, wire.Anthropic, "anthropic", tc.body)
			out, err := ApplyPromptCache(inv, config, &cache.PromptPolicy{}, "")
			if err != nil {
				t.Fatal(err)
			}
			tc.want(t, decodedBody(t, out))
		})
	}
}

func TestApplyPromptCacheLeavesCallerPolicyInCharge(t *testing.T) {
	for name, body := range map[string]string{
		"top-level automatic policy":  `{"max_tokens":16,"cache_control":{"type":"ephemeral"},"system":"s","messages":[{"role":"user","content":"hi"}]}`,
		"different nested ttl":        `{"max_tokens":16,"system":"s","messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]},{"role":"user","content":"again"}]}`,
		"different tool result ttl":   `{"max_tokens":16,"system":"s","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}]}`,
		"different search result ttl": `{"max_tokens":16,"system":"s","messages":[{"role":"user","content":[{"type":"search_result","source":"https://example.com","title":"Result","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}]}`,
		"different MCP result ttl":    `{"max_tokens":16,"system":"s","messages":[{"role":"user","content":[{"type":"mcp_tool_result","tool_use_id":"call-1","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}]}`,
		"non-object nested control":   `{"max_tokens":16,"system":"s","messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":null}]},{"role":"user","content":"again"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			inv, config := preparedPromptCache(t, wire.Anthropic, "anthropic", body)
			out, err := ApplyPromptCache(inv, config, &cache.PromptPolicy{}, "")
			if err != nil {
				t.Fatal(err)
			}
			if got, want := out.Prepared.Document().Raw(), inv.Prepared.Document().Raw(); got != want {
				t.Fatalf("caller-owned policy was overridden:\n got %s\nwant %s", got, want)
			}
		})
	}
	t.Run("missing nested ttl matches the default", func(t *testing.T) {
		inv, config := preparedPromptCache(t, wire.Anthropic, "anthropic", `{"max_tokens":16,"system":"s","messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
		out, err := ApplyPromptCache(inv, config, &cache.PromptPolicy{}, "")
		if err != nil {
			t.Fatal(err)
		}
		if got := lastBlockControl(t, decodedBody(t, out)["system"]); got == nil {
			t.Fatal("a default-ttl caller breakpoint blocked insertion")
		}
	})
}

func TestApplyPromptCacheOpenAIKey(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"absent key is set":       {`{"model":"m","messages":[{"role":"user","content":"hi"}]}`, "olp-key"},
		"caller key is preserved": {`{"model":"m","prompt_cache_key":"mine","messages":[{"role":"user","content":"hi"}]}`, "mine"},
	} {
		t.Run(name, func(t *testing.T) {
			inv, config := preparedPromptCache(t, wire.Chat, "openai", tc.body)
			out, err := ApplyPromptCache(inv, config, &cache.PromptPolicy{}, "olp-key")
			if err != nil {
				t.Fatal(err)
			}
			if got := decodedBody(t, out)["prompt_cache_key"]; got != tc.want {
				t.Fatalf("prompt_cache_key = %#v, want %q", got, tc.want)
			}
		})
	}
}
