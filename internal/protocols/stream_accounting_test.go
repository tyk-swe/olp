package protocols

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/oif"
)

func TestNativeTerminalUsageSurvivesProjectionAndDeliveryFailure(t *testing.T) {
	anthropic := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"a\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"native\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	var bedrock bytes.Buffer
	for _, frame := range []struct{ kind, body string }{
		{"messageStart", `{"role":"assistant"}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
		{"metadata", `{"usage":{"inputTokens":2,"outputTokens":1,"totalTokens":3}}`},
	} {
		if err := eventstream.NewEncoder().Encode(&bedrock, eventstream.Message{Headers: eventstream.Headers{{Name: ":message-type", Value: eventstream.StringValue("event")}, {Name: ":event-type", Value: eventstream.StringValue(frame.kind)}}, Payload: []byte(frame.body)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		family wire.Format
		wire   string
		marker string
	}{
		{wire.Chat, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n", `"usage"`},
		{wire.Responses, "data: {\"type\":\"response.completed\",\"response\":{\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n", "response.completed"},
		{wire.Anthropic, anthropic, "message_stop"},
		{wire.Gemini, "data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[]}}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1,\"totalTokenCount\":3}}\n\n", "usageMetadata"},
		{wire.CohereChat, cohereStream(`{"type":"message-start","id":"c"}`, `{"type":"message-end","delta":{"finish_reason":"COMPLETE","usage":{"billed_units":{"input_tokens":2,"output_tokens":1}}}}`), "message-end"},
		{wire.Bedrock, bedrock.String(), "inputTokens"},
	} {
		for _, refusal := range []string{"projection", "delivery"} {
			t.Run(string(tc.family)+"/"+refusal, func(t *testing.T) {
				blocked := errors.New("projection refused")
				writes := 0
				emit := func(frame []byte) error {
					if strings.Contains(string(frame), tc.marker) {
						writes++
						if refusal == "delivery" {
							return io.ErrClosedPipe
						}
					}
					return nil
				}
				observe := func(event oif.Event) error {
					if refusal == "projection" && strings.Contains(event.Source().Raw(), tc.marker) {
						return blocked
					}
					return nil
				}
				c, err := StreamWithEvents(tc.family, tc.family, strings.NewReader(tc.wire), 65536, "route", true, emit, observe, 1)
				want := blocked
				if refusal == "delivery" {
					want = io.ErrClosedPipe
				}
				if !errors.Is(err, want) || c == nil || c.Usage == nil || !c.UsageComplete || c.Usage.TotalTokens != 3 {
					t.Fatalf("authoritative native accounting lost after %s: completion=%+v error=%v", refusal, c, err)
				}
				if refusal == "projection" && writes != 0 {
					t.Fatal("refused native event crossed the delivery barrier")
				}
			})
		}
	}
}

func TestIntermediateNativeUsageRemainsIncomplete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		family wire.Format
		wire   string
		expect int64
	}{
		{"anthropic-start", wire.Anthropic, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"a\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"native\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n", 1},
		{"anthropic-delta", wire.Anthropic, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"a\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n", 1},
		{"gemini-unfinished-candidate", wire.Gemini, "data: {\"candidates\":[{\"index\":0,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1}}\n\n", 1},
		{"gemini-other-candidate", wire.Gemini, "data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[]}},{\"index\":1,\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"partial\"}]}}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1}}\n\n", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := StreamWithEvents(tc.family, tc.family, strings.NewReader(tc.wire), 65536, "route", true, func([]byte) error { return nil }, nil, tc.expect)
			if err == nil || c == nil || c.Usage == nil || c.UsageComplete {
				t.Fatalf("intermediate native counters treated as final: completion=%+v error=%v", c, err)
			}
		})
	}
}

func TestGeminiTerminalAccountingRequiresDispatchedCandidateProofOrEOF(t *testing.T) {
	encodedWire := "data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[]}}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1}}\n\n"
	for _, tc := range []struct {
		name     string
		expected []int64
		fail     bool
		complete bool
	}{
		{"unscoped clean EOF", nil, false, true},
		{"unscoped failed delivery", nil, true, false},
		{"one dispatched candidate", []int64{1}, true, true},
		{"another dispatched candidate", []int64{2}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := StreamWithEvents(wire.Gemini, wire.Gemini, strings.NewReader(encodedWire), 65536, "route", true, func([]byte) error {
				if tc.fail {
					return io.ErrClosedPipe
				}
				return nil
			}, nil, tc.expected...)
			if tc.fail && !errors.Is(err, io.ErrClosedPipe) || !tc.fail && err != nil || c == nil || c.Usage == nil || c.UsageComplete != tc.complete {
				t.Fatalf("candidate scope proof lost: completion=%+v error=%v", c, err)
			}
		})
	}
}

func TestTranslatedTerminalUsageSurvivesDeliveryFailure(t *testing.T) {
	encodedWire := "data: {\"candidates\":[{\"index\":0,\"finishReason\":\"STOP\",\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"done\"}]}}],\"usageMetadata\":{\"promptTokenCount\":2,\"candidatesTokenCount\":1}}\n\n"
	c, err := StreamWithEvents(wire.Gemini, wire.Chat, strings.NewReader(encodedWire), 65536, "route", true, func(frame []byte) error {
		if strings.Contains(string(frame), `"usage"`) {
			return io.ErrClosedPipe
		}
		return nil
	}, nil, 1)
	if !errors.Is(err, io.ErrClosedPipe) || c == nil || c.Usage == nil || !c.UsageComplete || c.Usage.TotalTokens != 3 {
		t.Fatalf("translation lost native terminal accounting: completion=%+v error=%v", c, err)
	}
}
