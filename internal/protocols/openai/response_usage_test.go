package openai

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestObserveResponseUsagePreservesPresenceAndNativeBytes(t *testing.T) {
	zero, cached, reasoning := int64(0), int64(1), int64(2)
	for _, tc := range []struct {
		name, body, status string
		usage              *Usage
		complete           bool
	}{
		{name: "missing usage", body: `{"status":"in_progress"}`, status: "in_progress"},
		{name: "terminal unknown", body: `{"status":"completed","usage":null}`, status: "completed"},
		{name: "missing status", body: `{"usage":{"input_tokens":2,"output_tokens":3}}`, usage: &Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}},
		{name: "null status", body: `{"status":null,"usage":{"input_tokens":2,"output_tokens":3}}`, usage: &Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}},
		{name: "null status unknown usage", body: `{"status":null}`},
		{name: "partial", body: `{"status":"in_progress", "usage": {"input_tokens":2,"output_tokens":3,"input_tokens_details":{"cached_tokens":1},"output_tokens_details":{"reasoning_tokens":2}},"vendor_extension":[1, 2]}`, status: "in_progress", usage: &Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5, CachedInputTokens: &cached, ReasoningTokens: &reasoning}},
		{name: "null total", body: `{"status":"queued","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":null}}`, status: "queued", usage: &Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}},
		{name: "provided total", body: `{"status":"completed","usage":{"input_tokens":2,"output_tokens":3,"total_tokens":0}}`, status: "completed", usage: &Usage{InputTokens: 2, OutputTokens: 3}, complete: true},
		{name: "explicit zero", body: `{"status":"completed","usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`, status: "completed", usage: &Usage{CachedInputTokens: &zero, ReasoningTokens: &zero}, complete: true},
		{name: "null cache unknown", body: `{"status":"completed","usage":{"input_tokens":2,"output_tokens":3,"input_tokens_details":{"cached_tokens":null}}}`, status: "completed", usage: &Usage{InputTokens: 2, OutputTokens: 3, TotalTokens: 5}, complete: true},
		{name: "maximum", body: `{"status":"completed","usage":{"input_tokens":9223372036854775807,"output_tokens":0}}`, status: "completed", usage: &Usage{InputTokens: 9223372036854775807, TotalTokens: 9223372036854775807}, complete: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			original := bytes.Clone(body)
			observed, err := ObserveResponseUsage(body)
			if err != nil || observed.Status != tc.status || observed.Complete != tc.complete || !reflect.DeepEqual(observed.Usage, tc.usage) {
				t.Fatalf("observation %+v, err=%v; want status=%q complete=%t usage=%+v", observed, err, tc.status, tc.complete, tc.usage)
			}
			doc, err := oif.ParseJSON(body, oif.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			fromDocument, err := ObserveResponseDocumentUsage(doc)
			if err != nil || !reflect.DeepEqual(fromDocument, observed) || !bytes.Equal(doc.Bytes(), original) {
				t.Fatalf("immutable document observation differs: %+v err=%v", fromDocument, err)
			}
			if !bytes.Equal(body, original) {
				t.Fatal("observation rewrote native response bytes")
			}
		})
	}
	for _, status := range []string{"completed", "incomplete", "failed", "cancelled"} {
		observed, err := ObserveResponseUsage([]byte(`{"status":"` + status + `","usage":{"input_tokens":0,"output_tokens":0}}`))
		if err != nil || !observed.Complete || observed.Usage == nil {
			t.Fatalf("terminal %s lost explicit zero evidence: %+v, %v", status, observed, err)
		}
	}
}

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

func TestResponsesUnaryUsageCompletenessFollowsAcceptedOperation(t *testing.T) {
	for _, status := range []string{"queued", "in_progress", "completed", "incomplete", ""} {
		t.Run(status, func(t *testing.T) {
			statedStatus := ""
			if status != "" {
				statedStatus = `,"status":"` + status + `"`
			}
			body := []byte(`{"output":[],"model":"native","usage":{"input_tokens":2,"output_tokens":3}` + statedStatus + `}`)
			completion, err := DecodeBackgroundResponse(body, "route")
			pending := status == "queued" || status == "in_progress"
			if err != nil || completion == nil || completion.Usage == nil || completion.UsageComplete == pending || !bytes.Equal(completion.Native.Source().Bytes(), body) {
				t.Fatalf("operation status=%q changed native bytes or completeness: %+v, %v", status, completion, err)
			}
		})
	}
}

const partialResponseUsageEvent = `{"type":"response.in_progress","response":{"id":"resp_1","object":"response","status":"in_progress","output":[],"usage":{"input_tokens":8,"output_tokens":3,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1}}}}`

func TestResponsesStreamRetainsPartialUsageBeforeFailure(t *testing.T) {
	for _, tc := range []struct {
		name, suffix string
		readErr      error
		emitErr      error
		observeErr   error
	}{
		{name: "missing terminal"},
		{name: "malformed JSON", suffix: "data: invalid\n\n"},
		{name: "invalid terminal usage", suffix: responsesEvents(`{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{}}}`)},
		{name: "mismatched terminal", suffix: responsesEvents(`{"type":"response.completed","response":{"id":"resp_1","status":"incomplete","output":[]}}`)},
		{name: "provider error", suffix: responsesEvents(`{"type":"error","error":{"message":"failed"}}`)},
		{name: "transport error", readErr: io.ErrUnexpectedEOF},
		{name: "cancellation", readErr: context.Canceled},
		{name: "delivery failure", emitErr: io.ErrClosedPipe},
		{name: "observer failure", observeErr: io.ErrClosedPipe},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reader io.Reader = strings.NewReader(responsesEvents(partialResponseUsageEvent) + tc.suffix)
			if tc.readErr != nil {
				reader = io.MultiReader(reader, failedStreamReader{tc.readErr})
			}
			completion, err := StreamMetadataEvents(FamilyResponses, reader, 4096, "route", true, func([]byte) error { return tc.emitErr }, func(_ oif.Event) error { return tc.observeErr })
			if err == nil || completion == nil || completion.UsageComplete || completion.Usage == nil || completion.Usage.InputTokens != 8 || completion.Usage.OutputTokens != 3 || completion.Usage.TotalTokens != 11 || completion.Usage.CachedInputTokens == nil || *completion.Usage.CachedInputTokens != 2 || completion.Usage.ReasoningTokens == nil || *completion.Usage.ReasoningTokens != 1 {
				t.Fatalf("partial evidence lost or marked complete: %+v, %v", completion, err)
			}
			for _, expected := range []error{tc.readErr, tc.emitErr, tc.observeErr} {
				if expected != nil && !errors.Is(err, expected) {
					t.Fatalf("failure changed: got %v, want %v", err, expected)
				}
			}
		})
	}
}

func TestResponsesTerminalUsageOwnsCompletenessAndCorrections(t *testing.T) {
	zero, one, two := int64(0), int64(1), int64(2)
	for _, tc := range []struct {
		name, usage string
		want        *Usage
		complete    bool
	}{
		{name: "missing", want: &Usage{InputTokens: 8, OutputTokens: 3, TotalTokens: 11, CachedInputTokens: &two, ReasoningTokens: &one}},
		{name: "null", usage: `,"usage":null`, want: &Usage{InputTokens: 8, OutputTokens: 3, TotalTokens: 11, CachedInputTokens: &two, ReasoningTokens: &one}},
		{name: "lower", usage: `,"usage":{"input_tokens":1,"output_tokens":1,"input_tokens_details":{"cached_tokens":0}}`, want: &Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2, CachedInputTokens: &zero}, complete: true},
		{name: "zero", usage: `,"usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}`, want: &Usage{CachedInputTokens: &zero}, complete: true},
		{name: "cache correction", usage: `,"usage":{"input_tokens":8,"output_tokens":3,"input_tokens_details":{"cached_tokens":1}}`, want: &Usage{InputTokens: 8, OutputTokens: 3, TotalTokens: 11, CachedInputTokens: &one}, complete: true},
	} {
		for _, terminal := range []string{"completed", "incomplete", "failed"} {
			t.Run(tc.name+"/"+terminal, func(t *testing.T) {
				wire := responsesEvents(partialResponseUsageEvent, `{"type":"response.`+terminal+`","response":{"id":"resp_1","status":"`+terminal+`","output":[]`+tc.usage+`}}`)
				completion, err := StreamMetadata(FamilyResponses, strings.NewReader(wire), 4096, "route", true, func([]byte) error { return nil })
				if (err != nil) != (terminal == "failed") || completion == nil || completion.UsageComplete != tc.complete || !reflect.DeepEqual(completion.Usage, tc.want) {
					t.Fatalf("terminal evidence %+v, err=%v; want complete=%t usage=%+v", completion, err, tc.complete, tc.want)
				}
			})
		}
	}
}

func TestResponsesUsageObservationPreservesStreamBytesAndTerminalEvidenceBeforeDelivery(t *testing.T) {
	partial := `{"type":"response.in_progress", "response":{"status":"in_progress","usage":{"input_tokens":8, "output_tokens":3,"vendor_count":4}}, "vendor_extension":[1, 2]}`
	terminal := `{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}}}`
	wire := responsesEvents(partial, terminal)
	for _, failure := range []string{"none", "delivery", "observer"} {
		t.Run(failure, func(t *testing.T) {
			var frames strings.Builder
			completion, err := StreamMetadataEvents(FamilyResponses, strings.NewReader(wire), 4096, "route", true, func(frame []byte) error {
				frames.Write(frame)
				if failure == "delivery" && strings.Contains(string(frame), "response.completed") {
					return io.ErrClosedPipe
				}
				return nil
			}, func(event oif.Event) error {
				if failure == "observer" && strings.Contains(event.Source().Raw(), "response.completed") {
					return io.ErrClosedPipe
				}
				return nil
			})
			if completion == nil || !completion.UsageComplete || completion.Usage == nil || completion.Usage.InputTokens != 0 || completion.Usage.OutputTokens != 0 || completion.Usage.CachedInputTokens == nil || *completion.Usage.CachedInputTokens != 0 {
				t.Fatalf("terminal correction lost on %s: %+v", failure, completion)
			}
			if failure == "none" && (err != nil || frames.String() != wire) {
				t.Fatalf("native usage bytes changed: err=%v\n%s", err, frames.String())
			}
			if failure != "none" && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("terminal delivery failure changed: %v", err)
			}
		})
	}
}

func TestResponsesTerminalUsageCannotBeDowngradedByLaterFrames(t *testing.T) {
	terminal := `{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}}}`
	for _, suffix := range []string{responsesEvents(partialResponseUsageEvent), "data: invalid\n\n"} {
		frames := 0
		completion, err := StreamMetadata(FamilyResponses, strings.NewReader(responsesEvents(terminal)+suffix), 4096, "route", true, func([]byte) error { frames++; return nil })
		if err != nil || frames != 1 || completion == nil || !completion.UsageComplete || completion.Usage == nil || completion.Usage.InputTokens != 0 || completion.Usage.OutputTokens != 0 || completion.Usage.CachedInputTokens == nil || *completion.Usage.CachedInputTokens != 0 {
			t.Fatalf("later frame changed terminal evidence: %+v, frames=%d, err=%v", completion, frames, err)
		}
	}
}

func TestResponsesGrammarValidationPrecedesUsageObserver(t *testing.T) {
	for _, terminal := range []string{
		`{"type":"response.completed","response":{"status":"incomplete","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"input_tokens_details":{"cached_tokens":2}}}}`,
		`{"type":"response.completed"}`,
		`{"type":"response.failed","response":{"status":null,"output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"response.failed","response":{"status":1,"output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"response.in_progress","response":{"status":1,"output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"type":"response.created","response":{"status":"unknown","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`,
	} {
		t.Run(terminal, func(t *testing.T) {
			observed, emitted := 0, 0
			completion, err := StreamMetadataEvents(FamilyResponses, strings.NewReader(responsesEvents(partialResponseUsageEvent, terminal)), 4096, "route", true, func([]byte) error {
				emitted++
				return nil
			}, func(oif.Event) error {
				observed++
				return nil
			})
			if err == nil || observed != 1 || emitted != 1 || completion == nil || completion.UsageComplete || completion.Usage == nil || completion.Usage.InputTokens != 8 || completion.Usage.OutputTokens != 3 {
				t.Fatalf("invalid terminal reached effects or erased partial evidence: observed=%d emitted=%d completion=%+v err=%v", observed, emitted, completion, err)
			}
		})
	}
	terminal := `{"type":"response.failed","response":{"id":"resp_1","status":"failed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`
	observed := 0
	completion, err := StreamMetadataEvents(FamilyResponses, strings.NewReader(responsesEvents(partialResponseUsageEvent, terminal)), 4096, "route", true, func([]byte) error { return nil }, func(oif.Event) error { observed++; return nil })
	var failure *UpstreamError
	if !errors.As(err, &failure) || observed != 2 || completion == nil || !completion.UsageComplete || completion.Usage == nil || completion.Usage.InputTokens != 1 || completion.Usage.OutputTokens != 1 {
		t.Fatalf("valid failed terminal lost observed accounting: observed=%d completion=%+v err=%v", observed, completion, err)
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
			event, err := LiftEvent(FamilyResponses, body, tc.kind, 0, 4096)
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
		event, err := LiftEvent(FamilyResponses, body, "", 0, 4096)
		if err != nil {
			t.Fatal(err)
		}
		observed, known, err := ObserveResponseEventUsage(event)
		if err == nil || !known || observed.Usage != nil || observed.Complete {
			t.Errorf("native contradiction became accounting proof: body=%s observed=%+v known=%t err=%v", body, observed, known, err)
		}
	}
	event, err := LiftEvent(FamilyResponses, `{"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":0,"output_tokens":0}}}`, "response.in_progress", 0, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if _, known, err := ObserveResponseEventUsage(event); !known || err == nil {
		t.Fatal("event-name contradiction became accounting proof")
	}
	for _, kind := range []string{"response.created", "response.queued", "response.in_progress"} {
		for _, response := range []string{"", `,"response":null`} {
			event, err := LiftEvent(FamilyResponses, `{"type":"`+kind+`"`+response+`}`, kind, 0, 4096)
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

func TestNonterminalResponseSnapshotsPreserveWireAndAccountingPhase(t *testing.T) {
	terminal := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0,"input_tokens_details":{"cached_tokens":0}}}}`
	for _, kind := range []string{"response.created", "response.queued", "response.in_progress"} {
		for _, status := range []string{"queued", "in_progress", "completed", "incomplete", "failed", "cancelled", "null"} {
			t.Run(kind+"/"+status, func(t *testing.T) {
				// ResponseCreatedEvent and other lifecycle envelopes contain the
				// SDK's generic Response, including initial or replayed snapshots.
				statusJSON := `"` + status + `"`
				if status == "null" {
					statusJSON = "null"
				}
				partial := `{"type":"` + kind + `", "response":{"id":"resp_1", "status":` + statusJSON + `, "output":[], "usage":{"input_tokens":8,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}, "native":[9007199254740993,-0]}`
				wantStatus := strings.TrimPrefix(kind, "response.")
				if kind == "response.created" {
					wantStatus = "in_progress"
					if status == "queued" {
						wantStatus = "queued"
					}
				}
				for _, finish := range []bool{false, true} {
					wire := responsesEvents(partial)
					if finish {
						wire += responsesEvents(terminal)
					}
					wantObserved := 1
					if finish {
						wantObserved++
					}
					observed := 0
					var delivered strings.Builder
					completion, err := StreamMetadataEvents(FamilyResponses, strings.NewReader(wire), 4096, "route", true, func(frame []byte) error { delivered.Write(frame); return nil }, func(event oif.Event) error {
						proof, lifecycle, err := ObserveResponseEventUsage(event)
						if err != nil || !lifecycle || proof.Usage == nil {
							t.Fatalf("valid SDK snapshot refused: proof=%+v lifecycle=%t err=%v", proof, lifecycle, err)
						}
						if observed == 0 && (proof.Status != wantStatus || proof.Complete || event.Source().Raw() != partial) {
							t.Fatalf("nonterminal snapshot acquired completion authority or changed bytes: %+v", proof)
						}
						if observed == 1 && (proof.Status != "completed" || !proof.Complete || event.Source().Raw() != terminal) {
							t.Fatalf("terminal envelope lost authority: %+v", proof)
						}
						observed++
						return nil
					})
					if delivered.String() != wire || completion == nil || completion.Usage == nil || completion.UsageComplete != finish || observed != wantObserved {
						t.Fatalf("snapshot bytes/evidence changed: completion=%+v observed=%d err=%v", completion, observed, err)
					}
					if finish {
						if err != nil || completion.Usage.InputTokens != 0 || completion.Usage.OutputTokens != 0 || completion.Usage.CachedInputTokens == nil || *completion.Usage.CachedInputTokens != 0 {
							t.Fatalf("authoritative terminal correction lost: %+v err=%v", completion, err)
						}
					} else if err == nil || completion.Usage.InputTokens != 8 || completion.Usage.OutputTokens != 3 || completion.Usage.CachedInputTokens == nil || *completion.Usage.CachedInputTokens != 2 {
						t.Fatalf("partial SDK snapshot lost or finalized: %+v err=%v", completion, err)
					}
				}
			})
		}
	}
}

func TestOpaqueResponseExtensionsNeverBecomeAccounting(t *testing.T) {
	for _, kind := range []string{"vendor.event", "error", "response.output_text.delta", "response.cancelled"} {
		for _, response := range []string{`"opaque"`, `{"status":"completed","model":"native","usage":{}}`, `{"status":"completed","model":"native","usage":{"input_tokens":99,"output_tokens":99}}`} {
			t.Run(kind+"/"+response, func(t *testing.T) {
				body := `{"type":"` + kind + `","response":` + response + `}`
				event, err := LiftEvent(FamilyResponses, body, kind, 0, 4096)
				if err != nil {
					t.Fatal(err)
				}
				observed, known, err := ObserveResponseEventUsage(event)
				if err != nil || known || observed.Usage != nil || observed.Complete || event.Source().Raw() != body {
					t.Fatalf("opaque extension became trusted accounting: observed=%+v known=%t err=%v", observed, known, err)
				}
				var frames strings.Builder
				completion, _ := StreamMetadata(FamilyResponses, strings.NewReader(responsesEvents(partialResponseUsageEvent, body)), 4096, "route", true, func(frame []byte) error { frames.Write(frame); return nil })
				if completion == nil || completion.UsageComplete || completion.Usage == nil || completion.Usage.InputTokens != 8 || completion.Usage.OutputTokens != 3 || !strings.Contains(frames.String(), "data: "+body+"\n\n") {
					t.Fatalf("opaque extension altered accounting or native bytes: completion=%+v frames=%s", completion, frames.String())
				}
			})
		}
	}
}

func TestResponsesCompleteUsageSurvivesProjectionFailure(t *testing.T) {
	const maxEvent = 4096
	base := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"m","output":[],"usage":{"input_tokens":0,"output_tokens":0}},"pad":""}`
	terminal := strings.Replace(base, `"pad":""`, `"pad":"`+strings.Repeat("x", maxEvent-40-len(base))+`"`, 1)
	for _, observerErr := range []error{nil, io.ErrClosedPipe} {
		frames, observed := 0, 0
		completion, err := StreamMetadataEvents(FamilyResponses, strings.NewReader(responsesEvents(terminal)), maxEvent, strings.Repeat("r", 64), true, func([]byte) error { frames++; return nil }, func(event oif.Event) error {
			observed++
			usage, known, err := ObserveResponseEventUsage(event)
			if err != nil || !known || !usage.Complete || usage.Status != "completed" || usage.Usage == nil || event.Source().Raw() != terminal {
				t.Fatalf("projection refusal lost native accounting proof: %+v known=%t err=%v", usage, known, err)
			}
			return observerErr
		})
		if err == nil || frames != 0 || observed != 1 || completion == nil || !completion.UsageComplete || completion.Usage == nil || completion.Usage.InputTokens != 0 || completion.Usage.OutputTokens != 0 {
			t.Fatalf("valid complete usage lost with projection failure: completion=%+v frames=%d observed=%d err=%v", completion, frames, observed, err)
		}
		if observerErr != nil && !errors.Is(err, observerErr) {
			t.Fatalf("native observation failure changed: %v", err)
		}
	}
}
