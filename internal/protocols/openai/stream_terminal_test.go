package openai

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/testutil"
)

func TestStreamStopsAtDeliveredTerminalEvent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		family   Family
		wire     string
		terminal string
	}{
		{"chat", FamilyChat, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n", "[DONE]"},
		{"responses completed", FamilyResponses, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n", "response.completed"},
		{"responses incomplete", FamilyResponses, "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"r\",\"object\":\"response\",\"status\":\"incomplete\",\"output\":[],\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n\n", "response.incomplete"},
	} {
		for _, writeFailure := range []bool{false, true} {
			t.Run(tc.name+"/write_failure="+map[bool]string{false: "false", true: "true"}[writeFailure], func(t *testing.T) {
				// A deadline on the read after the terminal frame must not affect success.
				reader := io.MultiReader(testutil.Fragmented([]byte(tc.wire), 1), failedStreamReader{context.DeadlineExceeded})
				terminalFrames := 0
				completion, err := Stream(tc.family, reader, 4096, "route", true, func(frame []byte) error {
					if strings.Contains(string(frame), tc.terminal) {
						terminalFrames++
						if writeFailure {
							return io.ErrClosedPipe
						}
					}
					return nil
				})
				if terminalFrames != 1 {
					t.Fatalf("terminal frames: %d", terminalFrames)
				}
				if completion == nil || completion.Usage == nil || completion.Usage.TotalTokens != 5 {
					t.Fatalf("terminal usage lost: %+v", completion)
				}
				if writeFailure {
					if err != io.ErrClosedPipe {
						t.Fatalf("terminal delivery error lost: %v", err)
					}
				} else if err != nil {
					t.Fatalf("terminal completion: %+v, %v", completion, err)
				}
			})
		}
	}
}
