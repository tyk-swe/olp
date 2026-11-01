package export

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tyk-swe/olp/internal/egress"
)

func testPolicy() *egress.Policy {
	return &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
}

func TestHTTPSDeliveryRejectsWeakCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("request reached receiver") }))
	defer server.Close()
	sender := NewSender(testPolicy())
	record := Record{ID: "id", Stream: "requests", Data: json.RawMessage(`{}`)}
	for name, credential := range map[string]string{
		"short secret":     `{"signing_secret":"short"}`,
		"newline secret":   `{"signing_secret":"0123456789abcdef\nx"}`,
		"reserved header":  `{"headers":{"Transfer-Encoding":"chunked"}}`,
		"signature header": `{"headers":{"X-OLP-Signature":"forged"}}`,
		"invalid header":   "{\n",
	} {
		err := sender.Send(t.Context(), Destination{Type: "https", URL: server.URL, Format: "json"}, []byte(credential), record)
		if err == nil || DeliveryErrorCode(err) != "credential" {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestHTTPSDeliversSignedWorkflowEvidence(t *testing.T) {
	for _, tc := range []struct {
		stream string
		body   string
	}{
		{"runs", `{"stream":"runs","data":{"evidence_origin":"gateway_observed"}}`},
		{"trace_spans", `{"stream":"trace_spans","data":{"evidence_origin":"client_asserted"}}`},
	} {
		t.Run(tc.stream, func(t *testing.T) {
			type receivedRequest struct {
				method string
				header http.Header
				body   []byte
				err    error
			}
			received := make(chan receivedRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				received <- receivedRequest{method: r.Method, header: r.Header.Clone(), body: body, err: err}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			secret := "workflow-signing-secret-0123456789"
			credential, err := json.Marshal(map[string]string{"signing_secret": secret})
			if err != nil {
				t.Fatal(err)
			}
			record := Record{ID: "evidence-event", Stream: tc.stream, Data: json.RawMessage(tc.body)}
			destination := Destination{Type: "https", URL: server.URL, Format: "json"}
			if err := NewSender(testPolicy()).Send(t.Context(), destination, credential, record); err != nil {
				t.Fatalf("deliver evidence: %v", err)
			}

			request := <-received
			if request.err != nil {
				t.Fatal(request.err)
			}
			if request.method != http.MethodPost || string(request.body) != tc.body {
				t.Fatalf("unexpected request method or body: %s %s", request.method, request.body)
			}
			if request.header.Get("Content-Type") != "application/json" || request.header.Get("X-OLP-Event-ID") != record.ID {
				t.Fatalf("unexpected evidence headers: %v", request.header)
			}
			signature := hmac.New(sha256.New, []byte(secret))
			_, _ = signature.Write(request.body)
			if request.header.Get("X-OLP-Signature") != "sha256="+hex.EncodeToString(signature.Sum(nil)) {
				t.Fatal("signature does not cover the delivered evidence")
			}
			for _, values := range request.header {
				for _, value := range values {
					if strings.Contains(value, secret) {
						t.Fatal("signing secret appeared in request headers")
					}
				}
			}
		})
	}
}

func TestHTTPSRejectsUnsafeWorkflowEvidenceDestination(t *testing.T) {
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sender := NewSender(&egress.Policy{PlainHTTPHosts: []string{"127.0.0.1"}})
	for _, tc := range []struct {
		stream string
		origin string
	}{
		{"runs", "gateway_observed"},
		{"trace_spans", "client_asserted"},
	} {
		t.Run(tc.stream, func(t *testing.T) {
			record := Record{ID: "evidence-event", Stream: tc.stream, Data: json.RawMessage(`{"data":{"evidence_origin":"` + tc.origin + `"}}`)}
			err := sender.Send(t.Context(), Destination{Type: "https", URL: server.URL, Format: "json"}, []byte(`{"signing_secret":"workflow-signing-secret-0123456789"}`), record)
			if err == nil || DeliveryErrorCode(err) != "endpoint" {
				t.Fatalf("private destination was not rejected: %v", err)
			}
			select {
			case <-received:
				t.Fatal("evidence reached a disallowed destination")
			default:
			}
		})
	}
}

func TestOTLPDeliversWorkflowEvidence(t *testing.T) {
	for _, tc := range []struct {
		stream string
		body   string
	}{
		{"runs", `{"stream":"runs","data":{"evidence_origin":"gateway_observed"}}`},
		{"trace_spans", `{"stream":"trace_spans","data":{"evidence_origin":"client_asserted"}}`},
	} {
		t.Run(tc.stream, func(t *testing.T) {
			type receivedRequest struct {
				header http.Header
				body   []byte
				err    error
			}
			received := make(chan receivedRequest, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				received <- receivedRequest{header: r.Header.Clone(), body: body, err: err}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			sender := NewSender(testPolicy())
			sender.Installation = "evidence-installation"
			record := Record{ID: "evidence-event", Stream: tc.stream, At: time.Unix(1700000000, 0), Data: json.RawMessage(tc.body)}
			destination := Destination{Type: "otlp_logs", URL: server.URL, Format: "otlp"}
			credential := []byte(`{"headers":{"X-Collector-Key":"collector-secret"}}`)
			if err := sender.Send(t.Context(), destination, credential, record); err != nil {
				t.Fatalf("deliver evidence: %v", err)
			}

			request := <-received
			if request.err != nil {
				t.Fatal(request.err)
			}
			if request.header.Get("Content-Type") != "application/x-protobuf" || request.header.Get("X-Collector-Key") != "collector-secret" {
				t.Fatalf("unexpected collector headers: %v", request.header)
			}
			var delivered collogspb.ExportLogsServiceRequest
			if err := proto.Unmarshal(request.body, &delivered); err != nil {
				t.Fatalf("decode evidence: %v", err)
			}
			resources := delivered.GetResourceLogs()
			if len(resources) != 1 || len(resources[0].GetScopeLogs()) != 1 || len(resources[0].GetScopeLogs()[0].GetLogRecords()) != 1 {
				t.Fatalf("unexpected delivered records: %v", &delivered)
			}
			hasAttribute := func(attributes []*commonpb.KeyValue, key, value string) bool {
				return slices.ContainsFunc(attributes, func(attribute *commonpb.KeyValue) bool {
					return attribute.GetKey() == key && attribute.GetValue().GetStringValue() == value
				})
			}
			if !hasAttribute(resources[0].GetResource().GetAttributes(), "olp.installation_id", sender.Installation) {
				t.Fatal("installation identity missing from evidence")
			}
			entry := resources[0].GetScopeLogs()[0].GetLogRecords()[0]
			if entry.GetBody().GetStringValue() != tc.body || entry.GetTimeUnixNano() != uint64(record.At.UnixNano()) ||
				!hasAttribute(entry.GetAttributes(), "olp.event_id", record.ID) || !hasAttribute(entry.GetAttributes(), "olp.stream", tc.stream) {
				t.Fatalf("evidence identity, provenance or timestamp changed: %v", entry)
			}
		})
	}
}

func TestS3ObjectPUTUsesStableKeyAndJSONL(t *testing.T) {
	var gotPath, gotType string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotType = r.URL.Path, r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer server.Close()
	sender := NewSender(testPolicy())
	credential, _ := json.Marshal(map[string]any{"region": "us-east-1", "auth_mode": "static", "secret": map[string]string{"access_key_id": "AKIAIOSFODNN7EXAMPLE", "secret_access_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}})
	record := Record{ID: "rec1", Stream: "usage_rollups", At: time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), Data: json.RawMessage(`{"stream":"usage_rollups","data":{"v":1}}`)}
	if err := sender.Send(t.Context(), Destination{Type: "s3", URL: server.URL + "/bucket/prefix", Format: "jsonl"}, credential, record); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/bucket/prefix/usage_rollups/2026-03-04/rec1.jsonl" {
		t.Fatalf("key %q", gotPath)
	}
	if gotType != "application/x-ndjson" || !strings.HasSuffix(string(gotBody), "\n") {
		t.Fatalf("jsonl body content-type=%q body=%q", gotType, gotBody)
	}
}

func TestOTLPCollectorOutcomes(t *testing.T) {
	response := func(rejected int64, message string) []byte {
		data, err := proto.Marshal(&collogspb.ExportLogsServiceResponse{PartialSuccess: &collogspb.ExportLogsPartialSuccess{RejectedLogRecords: rejected, ErrorMessage: message}})
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, tc := range []struct {
		name string
		body []byte
		code string
	}{
		{"empty-success", nil, ""},
		{"warning", response(0, "collector warning"), ""},
		{"rejected", response(1, "private-collector-detail"), "otlp_rejected"},
		{"rejected-without-message", response(1, ""), "otlp_rejected"},
		{"truncated", []byte{0x0a}, "otlp_response"},
		{"malformed", []byte{0xff}, "otlp_response"},
		{"oversized", response(0, strings.Repeat("x", maxDeliveryResponseBytes+1)), "otlp_response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-protobuf")
				if tc.name == "truncated" {
					w.Header().Set("Content-Length", "100")
				}
				w.Write(tc.body)
			}))
			defer server.Close()
			sender := NewSender(testPolicy())
			err := sender.Send(t.Context(), Destination{Type: "otlp_logs", URL: server.URL, Format: "otlp"}, []byte(`{}`), Record{ID: "event", Stream: "requests", At: time.Now(), Data: json.RawMessage(`{}`)})
			if tc.code == "" {
				if err != nil {
					t.Fatalf("accepted data reported as failure: %v", err)
				}
				return
			}
			if err == nil || DeliveryErrorCode(err) != tc.code {
				t.Fatalf("outcome=%v want=%s", err, tc.code)
			}
			if strings.Contains(err.Error(), "private-collector-detail") {
				t.Fatal("collector response leaked")
			}
			if DeliveryRejected(err) != (tc.code == "otlp_rejected") {
				t.Fatalf("incorrect retry disposition: %v", err)
			}

		})
	}
}
