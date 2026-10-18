package routing

import (
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func TestCompletionEffectiveOutputLimitIgnoresChatAlias(t *testing.T) {
	for _, fields := range []string{`,"max_tokens":1000,"max_completion_tokens":1`, `,"max_completion_tokens":1`} {
		request, err := openai.Parse(wire.Completions, []byte(`{"model":"route","prompt":"hello"`+fields+`}`))
		if err != nil {
			t.Fatal(err)
		}
		bound := EffectiveOutputLimit(request)
		if request.Field("max_tokens") == nil {
			if bound != nil {
				t.Fatalf("chat-only alias supplied a completion bound: %d", *bound)
			}
		} else if bound == nil || *bound != 1000 {
			t.Fatalf("completion bound: %v", bound)
		}
	}
}
