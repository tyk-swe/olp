package openai

import (
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestResponsesStreamIdentityPrecedesUsageAndNativeObservation(t *testing.T) {
	for _, known := range []bool{false, true} {
		for name, terminalID := range map[string]string{
			"changed": `"id":"another-response",`,
			"missing": "",
			"null":    `"id":null,`,
		} {
			t.Run(fmt.Sprintf("%s/known=%t", name, known), func(t *testing.T) {
				usage := "null"
				if known {
					usage = `{"input_tokens":10,"output_tokens":4,"total_tokens":14,"input_tokens_details":{"cached_tokens":2}}`
				}
				created := `{"type":"response.created","response":{"id":"original-response","status":"in_progress","output":[],"usage":` + usage + `}}`
				terminal := `{"type":"response.completed","response":{` + terminalID + `"status":"completed","output":[],"usage":{"input_tokens":90,"output_tokens":80,"total_tokens":170}}}`
				encodedWire := "event: response.created\ndata: " + created + "\n\nevent: response.completed\ndata: " + terminal + "\n\n"
				var emitted, observed int
				completion, err := StreamMetadataEvents(wire.Responses, strings.NewReader(encodedWire), 4096, "route", true,
					func([]byte) error { emitted++; return nil }, func(oif.Event) error { observed++; return nil })
				if err == nil || emitted != 1 || observed != 1 {
					t.Fatalf("unbound terminal reached delivery or native observation: emitted=%d observed=%d err=%v", emitted, observed, err)
				}
				if !known {
					if completion != nil && completion.Usage != nil {
						t.Fatalf("unbound terminal manufactured observed usage: %+v", completion)
					}
					return
				}
				if completion == nil || completion.Usage == nil || completion.UsageComplete || completion.UpstreamID != "original-response" ||
					completion.Usage.InputTokens != 10 || completion.Usage.OutputTokens != 4 || completion.Usage.CachedInputTokens == nil || *completion.Usage.CachedInputTokens != 2 {
					t.Fatalf("unbound terminal replaced original partial usage: %+v", completion)
				}
			})
		}
	}
}

func TestResponsesLocalProjectionRetainsNativeTerminalOutcome(t *testing.T) {
	for _, status := range []string{"completed", "incomplete", "failed"} {
		t.Run(status, func(t *testing.T) {
			nativeError := ""
			if status == "failed" {
				nativeError = `,"error":{"type":"server_error","message":"native generation failed"}`
			}
			body := `{"type":"response.` + status + `","response":{"id":"original-response","model":"native","status":"` + status + `","output":[],"usage":{"input_tokens":17,"output_tokens":9,"total_tokens":26}}` + nativeError + `}`
			encodedWire := "event: response." + status + "\ndata: " + body + "\n\n"
			var emitted int
			completion, err := StreamMetadataEvents(wire.Responses, strings.NewReader(encodedWire), 4096, strings.Repeat("r", 8192), true,
				func([]byte) error { emitted++; return nil }, nil)
			terminal, failed := completion.NativeResponseOutcome()
			if err == nil || !IsResponseProjectionError(err) || !terminal || (failed != nil) != (status == "failed") || emitted != 0 {
				t.Fatalf("local refusal erased or changed native outcome: terminal=%t failure=%v emitted=%d err=%v", terminal, failed, emitted, err)
			}
			if completion.Usage == nil || !completion.UsageComplete || completion.Usage.InputTokens != 17 || completion.Usage.OutputTokens != 9 {
				t.Fatalf("local refusal erased native terminal consumption: %+v", completion)
			}
		})
	}
}
