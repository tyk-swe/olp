package providerinvoke

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

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
