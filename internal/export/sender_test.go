package export

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
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
