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
