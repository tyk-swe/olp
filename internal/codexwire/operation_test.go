package codexwire

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestIdentityAndOperationObservations(t *testing.T) {
	for _, test := range []struct {
		name, body            string
		headers               http.Header
		websocket             bool
		want, parent, refusal string
	}{
		{name: "legacy", body: `{ "model":"gpt-5.4", "input":[{"type":"function_call_output","call_id":"call_1","output":"private"}], "store":false }`, headers: http.Header{"Session_id": {"legacy-root"}}, want: "legacy-root"},
		{name: "current", body: `{"type":"response.create","model":"gpt-5.4","previous_response_id":"resp_1","client_metadata":{"thread_id":"child","x-codex-parent-thread-id":"root"}}`, headers: http.Header{"Thread-Id": {"child"}, "Session-Id": {"process-id"}}, websocket: true, want: "child", parent: "root"},
		{name: "prewarm", body: `{"type":"response.create","model":"gpt-5.4","generate":false}`, headers: http.Header{"Session_id": {"root"}}, websocket: true, want: "root"},
		{name: "duplicate", body: `{"model":"gpt-5.4","model":"other"}`, headers: http.Header{"Session_id": {"root"}}, refusal: "code_body_invalid"},
		{name: "ambiguous", body: `{"model":"gpt-5.4","client_metadata":{"thread_id":"other"}}`, headers: http.Header{"Thread-Id": {"root"}}, refusal: "code_identity_ambiguous"},
		{name: "repeated", body: `{"model":"gpt-5.4"}`, headers: http.Header{"Thread-Id": {"root", "root"}}, refusal: "code_identity_ambiguous"},
		{name: "subagent", body: `{"model":"gpt-5.4"}`, headers: http.Header{"Session_id": {"child"}, "X-Openai-Subagent": {"review"}}, refusal: "code_parent_unresolved"},
		{name: "background", body: `{"model":"gpt-5.4","background":true}`, headers: http.Header{"Session_id": {"root"}}, refusal: "code_operation_unsupported"},
		{name: "unclassified websocket", body: `{"type":"anything","model":"gpt-5.4"}`, headers: http.Header{"Session_id": {"root"}}, websocket: true, refusal: "code_operation_unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := []byte(test.body)
			request, err := Classify(original, test.headers, "responses", test.websocket)
			if string(original) != test.body {
				t.Fatal("observer rewrote payload")
			}
			if test.refusal != "" {
				var refusal *codemode.Refusal
				if !errors.As(err, &refusal) || refusal.Code != test.refusal {
					t.Fatalf("refusal=%v want %s", err, test.refusal)
				}
				return
			}
			if err != nil || request.Operation.Identity.Conversation != test.want || request.Operation.Identity.Parent != test.parent || request.Operation.Model != "gpt-5.4" {
				t.Fatalf("observation=%+v error=%v", request, err)
			}
			if test.name == "prewarm" && (!request.Prewarm || request.Operation.Name != "prewarm") {
				t.Fatal("prewarm not classified")
			}
		})
	}
}

func TestErrorAndAllowanceEventsAreMetadataOnly(t *testing.T) {
	observation := Observe([]byte(`{"type":"error","status":429,"error":{"message":"private"},"headers":{"x-codex-primary-used-percent":"100","authorization":"private"}}`), false)
	if !observation.Terminal || observation.Status != 429 || observation.Usage.Total != nil || observation.Allowance == nil || *observation.Allowance.RemainingPercent != 0 {
		t.Fatalf("bad error observation: %+v", observation)
	}
	observation = Observe([]byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":25,"reset_at":1738888888}}}`), false)
	if observation.Terminal || observation.Allowance == nil || *observation.Allowance.RemainingPercent != 75 || observation.Allowance.ResetsAt.Unix() != 1738888888 {
		t.Fatalf("bad allowance observation: %+v", observation)
	}
	for _, body := range []string{`{"model":"native-model","conversation":"remote-conversation"}`, `{"model":"native-model","input":[{"type":"item_reference","id":"remote-item"}]}`} {
		if _, err := Classify([]byte(body), http.Header{"Thread-Id": {"local-thread"}}, "responses", false); err == nil {
			t.Fatal("unresolved server context was admitted")
		}
	}
}

func TestEncodedObservationIsBoundedAndLeavesWireBytes(t *testing.T) {
	var wire bytes.Buffer
	encoder := gzip.NewWriter(&wire)
	_, _ = encoder.Write([]byte(strings.Repeat("a", 4096)))
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	original := bytes.Clone(wire.Bytes())
	if _, err := Decode(wire.Bytes(), "gzip", 1024); err == nil {
		t.Fatal("decompression bound ignored")
	}
	decoded, err := Decode(wire.Bytes(), "gzip", 8192)
	if err != nil || len(decoded) != 4096 || !bytes.Equal(wire.Bytes(), original) {
		t.Fatal("encoded observer altered wire body", err)
	}
}

func TestFragmentedSSEObservesOnlyConsistentTerminalUsage(t *testing.T) {
	event := "event: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15,\"input_tokens_details\":{\"cached_tokens\":7},\"output_tokens_details\":{\"reasoning_tokens\":3}}}}\r\n\r\n"
	var observations []Observation
	stream := NewStream(2048, func(o Observation) {
		if o.Terminal {
			observations = append(observations, o)
		}
	})
	for _, part := range []byte(": keepalive\n\ndata: " + strings.Repeat("z", 4096) + "\n\n" + event) {
		stream.Write([]byte{part})
	}
	if len(observations) != 1 || observations[0].Usage.Total == nil || *observations[0].Usage.Total != 15 || *observations[0].Usage.Cached != 7 || *observations[0].Usage.Reasoning != 3 {
		t.Fatalf("observations=%+v", observations)
	}
	for _, body := range []string{`{"type":"response.completed","response":{"usage":{"total_tokens":1,"input_tokens":8,"output_tokens":2}}}`, `{"type":"response.failed","response":{}}`} {
		o := Observe([]byte(body), false)
		if !o.Terminal || o.Usage.Total != nil {
			t.Fatalf("missing/contradictory usage became known: %+v", o)
		}
	}
}

func TestForwardHeadersPreserveRepeatedEndToEndValues(t *testing.T) {
	header := http.Header{"Authorization": {"Bearer olp-key"}, "Chatgpt-Account-Id": {"untrusted"}, "Connection": {"upgrade, X-Hop"}, "X-Hop": {"remove"}, "Upgrade": {"websocket"}, "X-Olp-Control": {"remove"}, "X-Native": {"one", "two"}, "Openai-Beta": {"responses=v1", "other=v2"}, "User-Agent": {"codex/0.114.0"}}
	want := http.Header{"X-Native": {"one", "two"}, "Openai-Beta": {"responses=v1", "other=v2"}, "User-Agent": {"codex/0.114.0"}}
	if got := ForwardHeaders(header, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("headers=%v", got)
	}
	if header.Get("Authorization") != "Bearer olp-key" {
		t.Fatal("source headers mutated")
	}
}

func TestSSECarriageReturnDelimitersAndFragmentedCRLF(t *testing.T) {
	for _, delimiter := range []string{"\r", "\n", "\r\n"} {
		t.Run(fmt.Sprintf("%q", delimiter), func(t *testing.T) {
			var observations []Observation
			stream := NewStream(1024, func(o Observation) { observations = append(observations, o) })
			for _, b := range []byte(`data: {"type":"response.completed","response":{"usage":{"total_tokens":0}}}` + delimiter + delimiter) {
				stream.Write([]byte{b})
			}
			if len(observations) != 1 || observations[0].Usage.Total == nil || *observations[0].Usage.Total != 0 {
				t.Fatalf("SSE line ending lost: %+v", observations)
			}
		})
	}
}

func TestAllowanceRequiresProviderReportedValidNumbers(t *testing.T) {
	now := time.Now()
	h := http.Header{"X-Codex-Primary-Used-Percent": {"25"}, "X-Codex-Primary-Reset-At": {"1900000000"}}
	a := Allowance(h, now)
	if a == nil || *a.RemainingPercent != 75 || a.ResetsAt.Unix() != 1900000000 || a.ObservedAt != now {
		t.Fatalf("allowance=%+v", a)
	}
	for _, value := range []string{"NaN", "Inf", "-1", ""} {
		h.Set("X-Codex-Primary-Used-Percent", value)
		if Allowance(h, now) != nil {
			t.Fatal("invalid allowance trusted")
		}
	}
}
