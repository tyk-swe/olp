package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/tests/fidelity"
)

func TestNativeSourceAmbiguityRejectsBeforeProviderDispatch(t *testing.T) {
	for _, extension := range []string{`{"x":1,"x":2}`, `{"x":1,"\u0078":2}`, `{"x":"\ud800"}`, "{\"x\":\"\xff\"}"} {
		h := newHarness(t, Config{})
		body := []byte(`{"model":"team-chat","messages":[{"role":"user","content":"hi"}],"extension":` + extension + `}`)
		resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, body, nil)
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(raw), `"code":"invalid_json"`) {
			t.Fatalf("ambiguous input did not fail locally: %d %s", resp.StatusCode, raw)
		}
		if h.mock.count("a") != 0 || h.mock.count("b") != 0 {
			t.Fatal("ambiguous native data reached provider")
		}
	}
}

func TestNativeSourceConservationThroughPublishedGateway(t *testing.T) {
	h := newHarness(t, Config{})
	request := `{"model":"team-chat","messages":[{"role":"user","content":"é"}],"extension":{"integer":9007199254740993,"decimal":1.0000000000000001,"null":null,"empty":[],"false":false}}`
	want := strings.Replace(request, `"model":"team-chat"`, `"model":"model-a"`, 1)
	result := `{"id":"c","object":"chat.completion","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"extension":{"integer":9007199254740993,"opaque":"é"}}`
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		if err := fidelity.Compare([]byte(want), body); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, result)
	})
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(request), nil)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatal(resp.StatusCode, string(body))
	}
	if err := fidelity.Compare([]byte(strings.Replace(result, `"model":"model-a"`, `"model":"team-chat"`, 1)), body); err != nil {
		t.Fatal(err)
	}
}
