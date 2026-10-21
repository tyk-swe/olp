//go:build integration

package gosdk

import (
	"errors"
	"net/http"
	"testing"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
)

func TestOpenAIResponsesWithoutStore(t *testing.T) {
	h := connect(t)
	input := responses.ResponseNewParamsInputUnion{OfString: openai.String("Say hello.")}
	// A transformed route serves the call as the SDK sends it, with no store.
	response, err := h.openAI().Responses.New(timeout(t), responses.ResponseNewParams{Model: h.models["openai"], Input: input})
	if err != nil {
		t.Fatal(err)
	}
	if response.OutputText() != h.defaultReply || response.Model != h.models["openai"] {
		t.Fatalf("reply %q from %q", response.OutputText(), response.Model)
	}
	r := h.onlyRequest(t)
	if r.Path != "/openai/v1/responses" || r.Model != h.upstreamModels["openai"] {
		t.Fatalf("upstream saw %+v", r)
	}

	// A strict route preserves the call, and the API reads an omitted store as
	// true, which retains provider state. Only a key that allows it may.
	_, err = h.openAI().Responses.New(timeout(t), responses.ResponseNewParams{Model: h.models["openai_strict"], Input: input})
	e, ok := errors.AsType[*openai.Error](err)
	if !ok || e.StatusCode != http.StatusBadRequest || e.Code != "policy_conflict" || e.Param != "store" {
		t.Fatalf("a strict route without store: %T %v", err, err)
	}
	if requests := h.recorded(t, nil); len(requests) != 1 {
		t.Fatalf("the refused call reached the upstream: %+v", requests)
	}
}
