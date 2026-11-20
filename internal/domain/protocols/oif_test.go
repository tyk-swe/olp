package protocols_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
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

func TestMessagesStreamAppliesSuccessiveDeltas(t *testing.T) {
	frame := func(kind, fields string) string {
		return "event: " + kind + "\ndata: " + `{"type":"` + kind + `"` + fields + "}\n\n"
	}
	start := frame("message_start", `,"message":{"type":"message","role":"assistant","model":"route","content":[],"usage":{"input_tokens":3,"output_tokens":1}}`)
	block := frame("content_block_start", `,"index":0,"content_block":{"type":"text","text":""}`)
	text := frame("content_block_delta", `,"index":0,"delta":{"type":"text_delta","text":"hello"}`)
	closed := start + block + text + frame("content_block_stop", `,"index":0`)
	delta := frame("message_delta", `,"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}`)
	partial := frame("message_delta", `,"delta":{"stop_reason":null},"usage":{"output_tokens":4}`)
	final := frame("message_delta", `,"delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":6}`)
	reasonOnly := frame("message_delta", `,"delta":{"stop_reason":"end_turn"}`)
	usageOnly := frame("message_delta", `,"delta":{},"usage":{"output_tokens":6}`)
	stop := frame("message_stop", "")

	for _, tc := range []struct {
		name, forwarded, rejected string
		finishReason              string
		outputTokens              int64
		truncated                 bool
	}{
		{name: "latest counts and reason", forwarded: closed + delta + final + stop, finishReason: "length", outputTokens: 6},
		{name: "reason deferred", forwarded: closed + partial + final + stop, finishReason: "length", outputTokens: 6},
		{name: "usage omitted later", forwarded: closed + partial + reasonOnly + stop, finishReason: "stop", outputTokens: 4},
		{name: "reason omitted later", forwarded: closed + delta + usageOnly + stop, finishReason: "stop", outputTokens: 6},
		{name: "initial output alone is not final usage", forwarded: closed + reasonOnly + stop, finishReason: "stop"},
		{name: "reason never supplied", forwarded: closed + partial, rejected: stop, outputTokens: 4},
		{name: "malformed reason", forwarded: closed, rejected: frame("message_delta", `,"delta":{"stop_reason":42}`) + stop, outputTokens: 1},
		{name: "open content block", forwarded: start + block + text, rejected: delta + stop, outputTokens: 1},
		{name: "content after message delta", forwarded: closed + partial, rejected: frame("content_block_start", `,"index":1,"content_block":{"type":"text","text":""}`), outputTokens: 4},
		{name: "missing message stop", forwarded: closed + delta + final, finishReason: "length", outputTokens: 6, truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			result, err := protocols.Stream(wire.Anthropic, wire.Anthropic, strings.NewReader(tc.forwarded+tc.rejected), 8192, "route", func(frame []byte) error {
				_, err := output.Write(frame)
				return err
			})
			if tc.rejected != "" || tc.truncated {
				var protocolErr *wire.ProtocolError
				if !errors.As(err, &protocolErr) || protocolErr.Truncated != tc.truncated {
					t.Fatalf("expected protocol error (truncated=%t), got %v", tc.truncated, err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := fidelity.CompareEvents([]byte(tc.forwarded), output.Bytes()); err != nil {
				t.Fatal(err)
			}
			if result.FinishReason != tc.finishReason || result.UsageComplete != (err == nil && tc.outputTokens > 0) {
				t.Fatalf("finish reason = %q, complete usage = %t", result.FinishReason, result.UsageComplete)
			}
			if tc.outputTokens == 0 {
				if result.Usage != nil {
					t.Fatalf("initial output count became final usage: %+v", result.Usage)
				}
			} else if result.Usage == nil || result.Usage.InputTokens != 3 ||
				result.Usage.OutputTokens != tc.outputTokens || result.Usage.TotalTokens != 3+tc.outputTokens {
				t.Fatalf("usage = %+v, want 3 input and %d output tokens", result.Usage, tc.outputTokens)
			}
		})
	}
}
