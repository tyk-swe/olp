package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/runtime"
)

const jsonSchemaField = `,"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object"}}}`

func declareParams(h *harness, model string, params []string) {
	h.t.Helper()
	snap := h.rt.release.Snapshot
	for id, p := range snap.Providers {
		if len(p.Capabilities) == 0 || p.Capabilities[0].Model != model {
			continue
		}
		metadata, err := json.Marshal(runtime.ModelMetadata{SupportedParameters: &params})
		if err != nil {
			h.t.Fatal(err)
		}
		if p.Models == nil {
			p.Models = map[string]json.RawMessage{}
		}
		p.Models[model] = metadata
		snap.Providers[id] = p
	}
}

func TestStructuredOutputRequiresDeclaredSupport(t *testing.T) {
	h := newHarness(t, Config{})
	declareParams(h, modelB, []string{"response_format"})
	resp, body := h.chat(fullKey, nil, jsonSchemaField)
	if resp.StatusCode != http.StatusOK || body["model"] != routeSlug {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	if h.mock.count("a") != 0 || h.mock.count("b") != 1 {
		t.Fatalf("unqualified target must not be attempted: a=%d b=%d", h.mock.count("a"), h.mock.count("b"))
	}
}

func TestStructuredOutputUsesQualifiedTarget(t *testing.T) {
	h := newHarness(t, Config{})
	declareParams(h, modelA, []string{"response_format"})
	resp, body := h.chat(fullKey, nil, jsonSchemaField)
	if resp.StatusCode != http.StatusOK || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
		t.Fatalf("status %d body %v calls a=%d b=%d", resp.StatusCode, body, h.mock.count("a"), h.mock.count("b"))
	}
}

func TestStructuredOutputWithoutQualifiedTargetFails(t *testing.T) {
	h := newHarness(t, Config{})
	resp, body := h.chat(fullKey, nil, jsonSchemaField)
	if resp.StatusCode != http.StatusBadRequest || errorCode(t, body) != "invalid_request" {
		t.Fatalf("status %d body %v", resp.StatusCode, body)
	}
	if h.mock.count("a") != 0 || h.mock.count("b") != 0 {
		t.Fatalf("no target may serve: a=%d b=%d", h.mock.count("a"), h.mock.count("b"))
	}
}

func TestTextResponseFormatDoesNotRequireQualification(t *testing.T) {
	h := newHarness(t, Config{})
	for _, field := range []string{"", `,"response_format":{"type":"text"}`} {
		resp, _ := h.chat(fullKey, nil, field)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("field %q: status %d", field, resp.StatusCode)
		}
	}
	if h.mock.count("a") != 2 || h.mock.count("b") != 0 {
		t.Fatalf("plain requests keep their targets: a=%d b=%d", h.mock.count("a"), h.mock.count("b"))
	}
}
