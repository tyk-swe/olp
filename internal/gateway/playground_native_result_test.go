package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func TestPlaygroundPreservesNativeOperationResultBytes(t *testing.T) {
	started := time.Date(2026, time.September, 22, 0, 0, 0, 0, time.UTC)
	native := `{"data":[{"index":0,"embedding":[-0,0.1000000000000000000001]}],"metadata":{"count":9007199254740993,"__proto__":{"inert":true}}}`
	p := Playground{Gateway: &Server{now: func() time.Time { return started.Add(time.Millisecond) }}}
	x := &execution{route: &runtime.Route{Slug: "vector-route"}, request: request{startedAt: started}}
	out := &outcome{completion: &openai.Completion{Body: []byte(native)}}
	result := p.response(x, out, playgroundRequest{Operation: "embeddings"})
	if result["response_raw"] != native {
		t.Fatal("native result bytes changed in the console response")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"count":9007199254740993`) || !strings.Contains(string(encoded), `"embedding":[-0,0.1000000000000000000001]`) {
		t.Fatalf("parsed result was rounded or collapsed: %s", encoded)
	}
}
