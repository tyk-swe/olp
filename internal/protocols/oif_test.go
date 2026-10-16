package protocols_test

import (
	"bytes"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/tests/fidelity"
	fixtures "github.com/tyk-swe/olp/tests/fixtures/fidelity"
)

func TestOrderedGenerationViewUsesIndependentNativeSource(t *testing.T) {
	data, err := fixtures.Files.ReadFile("v1/anthropic-tool-next-request.json")
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Replace(data, []byte(`"fixture-model"`), []byte(`"fidelity-route"`), 1)
	r, err := protocols.Parse(wire.Anthropic, raw, "")
	if err != nil {
		t.Fatal(err)
	}
	view, err := protocols.GenerationRequest(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) != 3 || len(view.Messages[1].Nodes) != 5 {
		t.Fatal("native boundaries lost")
	}
	want := []string{"thinking", "text", "tool_call", "tool_call", "text"}
	for i, kind := range want {
		if view.Messages[1].Nodes[i].Kind != kind {
			t.Fatalf("ordered block %d changed", i)
		}
	}
	for i, id := range []string{"call-weather", "call-clock"} {
		n := view.Messages[2].Nodes[i]
		if n.CallID != id || len(n.Dependencies) != 1 || n.Dependencies[0].ID != id {
			t.Fatal("parallel result dependency lost")
		}
	}
	prepared, err := protocols.PrepareIdentity(r, "fixture-model")
	if err != nil {
		t.Fatal(err)
	}
	if err := fidelity.Compare(data, prepared.Document().Bytes()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, r.OIF().Source().Bytes()) {
		t.Fatal("native source was modified")
	}
	// A consumer can rearrange a derived view, but never the authoritative source.
	view.Messages[1].Nodes[1], view.Messages[1].Nodes[4] = view.Messages[1].Nodes[4], view.Messages[1].Nodes[1]
	if err := fidelity.Compare(data, prepared.Document().Bytes()); err != nil {
		t.Fatal(err)
	}
}

func TestOIFResultRetainsNativeCandidatesAndOpaqueState(t *testing.T) {
	native := []byte(`{"id":"native-id","object":"chat.completion","created":1,"model":"upstream","choices":[{"index":1,"message":{"role":"assistant","content":"second","annotations":[{"native":9007199254740993}]},"finish_reason":"stop"},{"index":0,"message":{"role":"assistant","content":null,"refusal":"declined"},"finish_reason":"stop"}],"native_extension":{"opaque":"\u0061","precise":1e400}}`)
	c, err := protocols.Decode(wire.Chat, wire.Chat, native, "route", "")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.Native.Source().Bytes(), native) {
		t.Fatal("upstream result was rewritten before lifting")
	}
	v, err := protocols.GenerationResult(c.Native)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Candidates) != 2 || v.Candidates[0].Index != 1 || v.Candidates[1].Index != 0 || v.Candidates[1].Nodes[0].Kind != "refusal" {
		t.Fatal("candidate order or refusal lost")
	}
	if err := fidelity.Compare(bytes.Replace(native, []byte(`"model":"upstream"`), []byte(`"model":"route"`), 1), c.Body); err != nil {
		t.Fatal(err)
	}
}

func TestNativeStreamLiftsBeforeProjectionAndDoesNotCollectHistory(t *testing.T) {
	source, err := fixtures.Files.ReadFile("v1/anthropic-tool-workflow.sse")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	count := uint64(0)
	sawModel, sawSignature := false, false
	c, err := protocols.StreamWithEvents(wire.Anthropic, wire.Anthropic, bytes.NewReader(source), 8192, "route", true, func(frame []byte) error { out.Write(frame); return nil }, func(e oif.Event) error {
		if e.Sequence() != count {
			t.Fatal("event position changed")
		}
		count++
		if value, ok := e.Source().Lookup("/message/model"); ok {
			sawModel = value.Raw() == `"fixture-model"`
		}
		if _, ok := e.Source().Lookup("/delta/signature"); ok {
			sawSignature = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 19 || !sawModel || !sawSignature {
		t.Fatal("native event lifting missed source state", count)
	}
	if c.Native.Source().Valid() || c.OutputText != "" {
		t.Fatal("native stream accumulated a complete source history")
	}
	expected := bytes.ReplaceAll(source, []byte(`"model":"fixture-model"`), []byte(`"model":"route"`))
	if err := fidelity.CompareEvents(expected, out.Bytes()); err != nil {
		t.Fatal(err)
	}
}
