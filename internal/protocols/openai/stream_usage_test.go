package openai

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestFailedChatStreamRetainsObservedUsage(t *testing.T) {
	for _, includeUsage := range []bool{false, true} {
		for _, tc := range []struct {
			name, suffix string
			readErr      error
			writeErr     error
		}{
			{name: "missing done"},
			{name: "malformed JSON", suffix: "data: invalid\n\n"},
			{name: "malformed SSE", suffix: "data: \xff\n\n"},
			{name: "oversized event", suffix: "data: " + strings.Repeat("x", 4096) + "\n\n"},
			{name: "upstream error", suffix: "data: {\"error\":{\"message\":\"failed\"}}\n\n"},
			{name: "transport failure", readErr: io.ErrUnexpectedEOF},
			{name: "cancelled", readErr: context.Canceled},
			{name: "client write failure", writeErr: io.ErrClosedPipe},
		} {
			t.Run(fmt.Sprintf("%s/include_usage=%t", tc.name, includeUsage), func(t *testing.T) {
				const chunk = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":1},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n"
				var reader io.Reader = strings.NewReader(chunk + tc.suffix)
				if tc.readErr != nil {
					reader = io.MultiReader(reader, failedStreamReader{tc.readErr})
				}
				var frames strings.Builder
				completion, err := Stream(FamilyChat, reader, 4096, "route", includeUsage, func(frame []byte) error {
					frames.Write(frame)
					return tc.writeErr
				})
				if err == nil {
					t.Fatal("failed stream was accepted")
				}
				if tc.readErr != nil && !errors.Is(err, tc.readErr) || tc.writeErr != nil && !errors.Is(err, tc.writeErr) {
					t.Fatalf("I/O error changed: %v", err)
				}
				cached, reasoning := int64(1), int64(2)
				want := &Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5, CachedInputTokens: &cached, ReasoningTokens: &reasoning}
				if completion == nil || !reflect.DeepEqual(completion.Usage, want) {
					t.Fatalf("observed usage lost: %+v", completion)
				}
				if strings.Contains(frames.String(), "[DONE]") || strings.Contains(frames.String(), `"usage":`) != includeUsage {
					t.Fatalf("failure changed stream output rules: %s", frames.String())
				}
			})
		}
	}
}
