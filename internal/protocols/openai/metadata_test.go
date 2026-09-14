package openai

import (
	"fmt"
	"strings"
	"testing"
)

func TestStreamMetadataForwardsWithoutAccumulatingOutput(t *testing.T) {
	const frame = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"text\",\"refusal\":\"no\",\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"argument\"}}]}}]}\n\n"
	const terminal = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
	for _, count := range []int{1, 8192} {
		frames := 0
		c, err := StreamMetadata(FamilyChat, strings.NewReader(strings.Repeat(frame, count)+terminal), 1024, "r", true, func(frame []byte) error {
			frames++
			if frames <= count && (!strings.Contains(string(frame), `"content":"text"`) || !strings.Contains(string(frame), `"arguments":"argument"`)) {
				t.Error("output was not forwarded")
			}
			return nil
		})
		if err != nil || c == nil {
			t.Fatalf("stream: %v", err)
		}
		if frames != count+2 || c.OutputText != "" || c.Refusal != "" || len(c.ToolCalls) != 0 || c.Usage == nil || c.Usage.TotalTokens != 5 || c.FinishReason != "stop" {
			t.Fatalf("metadata summary retained content or lost accounting: %+v, frames=%d", c, frames)
		}
	}
}

func TestStreamChoiceTrackingIsBounded(t *testing.T) {
	var wire strings.Builder
	for i := 0; i <= 1024/16; i++ {
		fmt.Fprintf(&wire, "data: {\"choices\":[{\"index\":%d,\"delta\":{}}]}\n\n", i)
	}
	frames := 0
	_, err := StreamMetadata(FamilyChat, strings.NewReader(wire.String()), 1024, "r", true, func([]byte) error { frames++; return nil })
	if err == nil || frames != 1024/16 {
		t.Fatalf("choice tracking limit not enforced: frames=%d err=%v", frames, err)
	}
	for _, index := range []string{"-1", "0.5", `"0"`, "null"} {
		frames = 0
		_, err := StreamMetadata(FamilyChat, strings.NewReader("data: {\"choices\":[{\"index\":"+index+",\"delta\":{}}]}\n\n"), 1024, "r", true, func([]byte) error { frames++; return nil })
		if err == nil || frames != 0 {
			t.Errorf("invalid index %s committed", index)
		}
	}
}
