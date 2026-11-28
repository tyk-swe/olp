package openai

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"

	"github.com/tyk-swe/olp/internal/domain/oif"
)

func TestObserveResponseUsageRejectsMalformedAccounting(t *testing.T) {
	for _, usage := range []string{
		`{}`, `[]`, `{"output_tokens":0}`, `{"input_tokens":0}`,
		`{"input_tokens":null,"output_tokens":0}`,
		`{"input_tokens":-1,"output_tokens":0}`,
		`{"input_tokens":1.0,"output_tokens":0}`,
		`{"input_tokens":9223372036854775808,"output_tokens":0}`,
		`{"input_tokens":0,"output_tokens":-1}`,
		`{"input_tokens":0,"output_tokens":null}`,
		`{"input_tokens":0,"output_tokens":9223372036854775808}`,
		`{"input_tokens":9223372036854775807,"output_tokens":1}`,
		`{"input_tokens":0,"output_tokens":0,"total_tokens":-1}`,
		`{"input_tokens":0,"output_tokens":0,"total_tokens":"0"}`,
		`{"input_tokens":0,"output_tokens":0,"total_tokens":9223372036854775808}`,
		`{"input_tokens":2,"output_tokens":3,"input_tokens_details":[]}`,
		`{"input_tokens":2,"output_tokens":3,"input_tokens_details":{"cached_tokens":-1}}`,
		`{"input_tokens":2,"output_tokens":3,"input_tokens_details":{"cached_tokens":"1"}}`,
		`{"input_tokens":2,"output_tokens":3,"input_tokens_details":{"cached_tokens":9223372036854775808}}`,
		`{"input_tokens":2,"output_tokens":3,"input_tokens_details":{"cached_tokens":3}}`,
		`{"input_tokens":2,"output_tokens":3,"output_tokens_details":true}`,
		`{"input_tokens":2,"output_tokens":3,"output_tokens_details":{"reasoning_tokens":-1}}`,
		`{"input_tokens":2,"output_tokens":3,"output_tokens_details":{"reasoning_tokens":4}}`,
		`{"input_tokens":2,"input_tokens":3,"output_tokens":0}`,
	} {
		t.Run(usage, func(t *testing.T) {
			body := []byte(`{"status":"completed","output":[],"usage":` + usage + `}`)
			if _, err := ObserveResponseUsage(body); err == nil {
				t.Fatal("invalid usage became known accounting evidence")
			}
			if doc, err := oif.ParseJSON(body, oif.Limits{}); err == nil {
				if _, err := ObserveResponseDocumentUsage(doc); err == nil {
					t.Fatal("immutable document admitted invalid accounting evidence")
				}
			}
			if _, err := DecodeResponse(body, "route"); err == nil {
				t.Fatal("unary decoder accepted different accounting semantics")
			}
		})
	}
	for _, body := range []string{`null`, `[]`, `{"status":0}`, `{"status":false}`, `{"status":[]}`, `{"status":""}`} {
		if _, err := ObserveResponseUsage([]byte(body)); err == nil {
			t.Errorf("invalid observation accepted: %s", body)
		}
		doc, err := oif.ParseJSON([]byte(body), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ObserveResponseDocumentUsage(doc); err == nil {
			t.Errorf("invalid document observation accepted: %s", body)
		}
	}
	if _, err := ObserveResponseDocumentUsage(oif.Document{}); err == nil {
		t.Fatal("missing document became accounting evidence")
	}
}

func TestResponseEventUsageOwnsLifecycleCompleteness(t *testing.T) {
	for _, tc := range []struct {
		kind, status, wantStatus string
		complete                 bool
	}{
		{kind: "response.created", status: "queued", wantStatus: "queued"},
		{kind: "response.created", status: "in_progress", wantStatus: "in_progress"},
		{kind: "response.created", wantStatus: "in_progress"},
		{kind: "response.queued", status: "queued", wantStatus: "queued"},
		{kind: "response.queued", wantStatus: "queued"},
		{kind: "response.in_progress", status: "in_progress", wantStatus: "in_progress"},
		{kind: "response.in_progress", wantStatus: "in_progress"},
		{kind: "response.completed", status: "completed", wantStatus: "completed", complete: true},
		{kind: "response.completed", wantStatus: "completed", complete: true},
		{kind: "response.incomplete", status: "incomplete", wantStatus: "incomplete", complete: true},
		{kind: "response.incomplete", wantStatus: "incomplete", complete: true},
		{kind: "response.failed", status: "failed", wantStatus: "failed", complete: true},
		{kind: "response.failed", wantStatus: "failed", complete: true},
	} {
		t.Run(tc.kind+"/"+tc.status, func(t *testing.T) {
			status := ""
			if tc.status != "" {
				status = `,"status":"` + tc.status + `"`
			}
			body := `{"type":"` + tc.kind + `","response":{"output":[],"usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}` + status + `}}`
			event, err := LiftEvent(wire.Responses, body, tc.kind, 0, 4096)
			if err != nil {
				t.Fatal(err)
			}
			observed, known, err := ObserveResponseEventUsage(event)
			if err != nil || !known || observed.Status != tc.wantStatus || observed.Complete != tc.complete || observed.Usage == nil || observed.Usage.InputTokens != 0 || observed.Usage.OutputTokens != 0 || observed.Usage.CachedInputTokens == nil || *observed.Usage.CachedInputTokens != 0 || event.Source().Raw() != body {
				t.Fatalf("lifecycle proof changed native meaning: observed=%+v known=%t err=%v", observed, known, err)
			}
		})
	}
	for _, body := range []string{
		`{"type":"response.created","response":{"status":"unknown","usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"response.queued","response":{"status":1,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"response.in_progress","response":{"status":false,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"response.in_progress","response":{"status":"cancelling","usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"response.completed","response":{"status":"in_progress","usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"response.completed","response":{"status":null,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		`{"type":"response.completed"}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{}}}`,
	} {
		event, err := LiftEvent(wire.Responses, body, "", 0, 4096)
		if err != nil {
			t.Fatal(err)
		}
		observed, known, err := ObserveResponseEventUsage(event)
		if err == nil || !known || observed.Usage != nil || observed.Complete {
			t.Errorf("native contradiction became accounting proof: body=%s observed=%+v known=%t err=%v", body, observed, known, err)
		}
	}
	event, err := LiftEvent(wire.Responses, `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":0,"output_tokens":0}}}`, "response.in_progress", 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if _, known, err := ObserveResponseEventUsage(event); !known || err == nil {
		t.Fatal("event-name contradiction became accounting proof")
	}
	for _, kind := range []string{"response.created", "response.queued", "response.in_progress"} {
		for _, response := range []string{"", `,"response":null`} {
			event, err := LiftEvent(wire.Responses, `{"type":"`+kind+`"`+response+`}`, kind, 0, 4096)
			if err != nil {
				t.Fatal(err)
			}
			observed, known, err := ObserveResponseEventUsage(event)
			if err != nil || !known || observed.Usage != nil || observed.Complete {
				t.Errorf("sparse nonterminal changed: kind=%s observed=%+v known=%t err=%v", kind, observed, known, err)
			}
		}
	}
}

func TestResponseUsageCompletenessAgreesAcrossResultsAndEvents(t *testing.T) {
	for _, status := range []string{"completed", "incomplete", "failed"} {
		t.Run(status, func(t *testing.T) {
			for _, usage := range []string{"", `,"usage":{"input_tokens":2,"output_tokens":1}`} {
				response := `{"status":"` + status + `"` + usage + `}`
				result, err := ObserveResponseUsage([]byte(response))
				if err != nil {
					t.Fatal(err)
				}
				kind := "response." + status
				event, err := LiftEvent(wire.Responses, `{"type":"`+kind+`","response":`+response+`}`, kind, 0, 4096)
				if err != nil {
					t.Fatal(err)
				}
				streamed, known, err := ObserveResponseEventUsage(event)
				if err != nil || !known || streamed.Complete != result.Complete {
					t.Fatalf("result=%+v event=%+v known=%t err=%v", result, streamed, known, err)
				}
			}
		})
	}
}

func TestIncompleteToolResponsesPreserveFinishReasonAndUsage(t *testing.T) {
	for reason, finish := range map[string]string{"max_output_tokens": "length", "content_filter": "content_filter"} {
		t.Run(reason, func(t *testing.T) {
			body := []byte(`{"id":"limited-tool","status":"incomplete","incomplete_details":{"reason":"` + reason + `"},"output":[{"type":"function_call","call_id":"call-read","name":"read_file","arguments":"{}"}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
			completion, err := DecodeResponse(body, "route")
			if err != nil {
				t.Fatal(err)
			}
			if completion.FinishReason != finish || len(completion.ToolCalls) != 1 || completion.ToolCalls[0].ID != "call-read" || completion.Usage == nil || completion.Usage.TotalTokens != 5 || !completion.UsageComplete {
				t.Fatalf("incomplete tool reply %+v", completion)
			}
		})
	}
}

func TestChatUsageAccounting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, usage, want string }{
		{
			"details",
			`{"prompt_tokens":100,"completion_tokens":40,"total_tokens":140,"prompt_tokens_details":{"cached_tokens":60,"cache_write_tokens":20,"audio_tokens":5},"completion_tokens_details":{"reasoning_tokens":12,"audio_tokens":3}}`,
			`{"input_tokens":100,"output_tokens":40,"total_tokens":140,"cached_input_tokens":60,"cache_write_input_tokens":20,"audio_input_tokens":5,"audio_output_tokens":3,"reasoning_tokens":12}`,
		},
		{"null total is computed", `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":null}`, `{"input_tokens":11,"output_tokens":7,"total_tokens":18}`},
		{"unusable total is computed", `{"prompt_tokens":11,"completion_tokens":7,"total_tokens":-1}`, `{"input_tokens":11,"output_tokens":7,"total_tokens":18}`},
		{"unusable optional counts are ignored", `{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":{"cached_tokens":-1,"audio_tokens":"5"},"completion_tokens_details":{"reasoning_tokens":1.5}}`, `{"input_tokens":11,"output_tokens":7,"total_tokens":18}`},
		{"null usage", `null`, `null`},
		{"output not reported", `{"prompt_tokens":11,"total_tokens":11}`, ""},
		{"negative input", `{"prompt_tokens":-1,"completion_tokens":7}`, ""},
		{"computed total overflow", `{"prompt_tokens":9223372036854775807,"completion_tokens":1}`, ""},
		{"details are not an object", `{"prompt_tokens":11,"completion_tokens":7,"prompt_tokens_details":5}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":` + tc.usage + `}`
			c, err := DecodeChat([]byte(body), "route", wire.Chat)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("accepted %s", tc.usage)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got, err := json.Marshal(c.Usage); err != nil || string(got) != tc.want {
				t.Fatalf("usage = %s, want %s: %v", got, tc.want, err)
			}
		})
	}
}

func TestResponsesUsageAccounting(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, usage, want string }{
		{
			"details",
			`{"input_tokens":100,"output_tokens":40,"total_tokens":140,"input_tokens_details":{"cached_tokens":60},"output_tokens_details":{"reasoning_tokens":12}}`,
			`{"input_tokens":100,"output_tokens":40,"total_tokens":140,"cached_input_tokens":60,"reasoning_tokens":12}`,
		},
		{"provider total is kept", `{"input_tokens":11,"output_tokens":7,"total_tokens":99}`, `{"input_tokens":11,"output_tokens":7,"total_tokens":99}`},
		{"total not reported", `{"input_tokens":11,"output_tokens":7}`, `{"input_tokens":11,"output_tokens":7,"total_tokens":18}`},
		{"null total and details", `{"input_tokens":11,"output_tokens":7,"total_tokens":null,"input_tokens_details":null,"output_tokens_details":null}`, `{"input_tokens":11,"output_tokens":7,"total_tokens":18}`},
		{"null usage", `null`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed, err := ObserveResponseUsage([]byte(`{"status":"completed","output":[],"usage":` + tc.usage + `}`))
			if err != nil {
				t.Fatal(err)
			}
			if got, err := json.Marshal(observed.Usage); err != nil || string(got) != tc.want {
				t.Fatalf("usage = %s, want %s: %v", got, tc.want, err)
			}
		})
	}
}
