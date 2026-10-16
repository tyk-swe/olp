package providerinvoke

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestPromptCachePreservesCallerControls(t *testing.T) {
	for _, tc := range []struct {
		family       wire.Format
		kind, source string
	}{
		{wire.Chat, "openai", `{"model":"route","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"caller"}`},
		{wire.Anthropic, "anthropic", `{"model":"route","max_tokens":10,"system":[{"type":"text","text":"private","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"user","content":"hi"}]}`},
	} {
		request, err := protocols.Parse(tc.family, []byte(tc.source), "")
		if err != nil {
			t.Fatal(err)
		}
		inv, err := Prepare(request, connectors.Config{Kind: tc.kind}, "model", nil)
		if err != nil {
			t.Fatal(err)
		}
		inv, err = ApplyPromptCache(inv, connectors.Config{Kind: tc.kind}, &cache.PromptPolicy{TTL: "5m"}, "derived")
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err = json.Unmarshal(inv.Prepared.Document().Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		if tc.family == wire.Chat {
			if string(doc["prompt_cache_key"]) != `"caller"` {
				t.Fatal("overwrote caller key")
			}
		} else if !strings.Contains(string(doc["system"]), `"ttl":"1h"`) {
			t.Fatal("overwrote caller TTL")
		}
	}
}
func TestPromptCacheInsertsBoundedAnthropicBreakpoints(t *testing.T) {
	request, err := protocols.Parse(wire.Anthropic, []byte(`{"model":"route","max_tokens":10,"system":"shared","tools":[{"name":"read","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`), "")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := Prepare(request, connectors.Config{Kind: "anthropic"}, "model", nil)
	if err != nil {
		t.Fatal(err)
	}
	inv, err = ApplyPromptCache(inv, connectors.Config{Kind: "anthropic"}, &cache.PromptPolicy{TTL: "1h"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	_ = json.Unmarshal(inv.Prepared.Document().Bytes(), &doc)
	if got := countCacheControls(doc); got != 3 {
		t.Fatalf("got %d breakpoints: %s", got, inv.Prepared.Document().Bytes())
	}
}
