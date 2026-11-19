package playground

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
)

func TestPlaygroundStructuredOutputPreservesLargeInteger(t *testing.T) {
	t.Parallel()
	const exact = `{"count":9007199254740993}`
	in := request{ResponseFormat: &format{Type: "json_object"}}
	for _, test := range []struct{ text, structured string }{
		{exact, exact},
		{`{"count":0,"count":9007199254740993}`, exact},
		{exact + ` {"extra":true}`, ""},
	} {
		body, err := json.Marshal(response(kernel.MemberTrail{Route: "team-chat"}, &wire.Completion{OutputText: test.text}, in))
		if err != nil {
			t.Fatal(err)
		}
		var response struct {
			Text       string          `json:"output_text"`
			Structured json.RawMessage `json:"structured_output"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			t.Fatal(err)
		}
		if response.Text != test.text || string(response.Structured) != test.structured {
			t.Fatalf("unexpected Playground result for %q: %s", test.text, body)
		}
	}
}
