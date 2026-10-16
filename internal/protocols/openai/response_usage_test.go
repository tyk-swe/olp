package openai

import (
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"

	"github.com/tyk-swe/olp/internal/oif"
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

const partialResponseUsageEvent = `{"type":"response.in_progress","response":{"id":"resp_1","object":"response","status":"in_progress","output":[],"usage":{"input_tokens":8,"output_tokens":3,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}`

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

// ResponseCreatedEvent and other lifecycle envelopes contain the
// SDK's generic Response, including initial or replayed snapshots.
