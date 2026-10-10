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

func TestHTTPSDeliverySignsExactBytes(t *testing.T) {
	var gotBody []byte
	var signature, eventID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		signature = r.Header.Get("X-OLP-Signature")
		eventID = r.Header.Get("X-OLP-Event-ID")
		w.WriteHeader(204)
	}))
	defer server.Close()
	sender := NewSender(testPolicy())
	credential, _ := json.Marshal(map[string]any{"signing_secret": "0123456789abcdef0123456789abcdef"})
	record := Record{ID: "01900000-0000-7000-8000-000000000001", Stream: "requests", At: time.Unix(1700000000, 0), Data: json.RawMessage(`{"event_id":"e","data":{"x":1}}`)}
	if err := sender.Send(t.Context(), Destination{Type: "https", URL: server.URL, Format: "json"}, credential, record); err != nil {
		t.Fatal(err)
	}
	sum := hmac.New(sha256.New, []byte("0123456789abcdef0123456789abcdef"))
	sum.Write(record.Data)
	if signature != "sha256="+hex.EncodeToString(sum.Sum(nil)) {
		t.Fatalf("signature %q does not cover the exact record bytes", signature)
	}
	if eventID != record.ID {
		t.Fatalf("event id %q", eventID)
	}
	if string(gotBody) != string(record.Data) {
		t.Fatalf("body rewritten: %s", gotBody)
	}
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

func TestDeliveryRefusesRedirectAndFailure(t *testing.T) {
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("redirect followed") }))
	defer redirected.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirected.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	sender := NewSender(testPolicy())
	record := Record{ID: "id", Stream: "requests", Data: json.RawMessage(`{}`)}
	if err := sender.Send(t.Context(), Destination{Type: "https", URL: server.URL, Format: "json"}, nil, record); err == nil {
		t.Fatal("redirect was followed")
	}
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer failed.Close()
	if err := sender.Send(t.Context(), Destination{Type: "https", URL: failed.URL, Format: "json"}, nil, record); DeliveryErrorCode(err) != "status" {
		t.Fatalf("status failure: %v", err)
	}
}

func TestObjectKeyStableAcrossRetries(t *testing.T) {
	record := Record{ID: "01900000-0000-7000-8000-000000000001", Stream: "requests", At: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)}
	first, second := objectKey(record), objectKey(record)
	if first != second || first != "requests/2026-10-09/01900000-0000-7000-8000-000000000001.jsonl" {
		t.Fatalf("unstable key %q", first)
	}
	capture := objectKey(Record{ID: "01900000-0000-7000-8000-000000000001", Stream: "captures"})
	if capture != "captures/01900000000070008000000000000001.jsonl" {
		t.Fatalf("capture key %q", capture)
	}
}

func TestDeliveryValidatesDestinationEveryAttempt(t *testing.T) {
	sender := NewSender(&egress.Policy{})
	record := Record{ID: "id", Stream: "requests", Data: json.RawMessage(`{}`)}
	for _, destination := range []string{"https://user:pw@example.test/x", "https://example.test/x?key=1", "https://example.test/x#frag", "http://example.test/x"} {
		if err := sender.Send(t.Context(), Destination{Type: "https", URL: destination, Format: "json"}, nil, record); err == nil {
			t.Fatalf("destination %s dialed", destination)
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

func TestOTLPLogsCarriesEnvelopeAsBody(t *testing.T) {
	var gotContentType string
	var gotRequest []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		gotRequest, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer server.Close()
	sender := NewSender(testPolicy())
	sender.Installation = "inst-1"
	record := Record{ID: "rec-otlp", Stream: "requests", At: time.Unix(1700000000, 0), Data: json.RawMessage(`{"event_id":"rec-otlp","data":{"k":1}}`)}
	if err := sender.Send(t.Context(), Destination{Type: "otlp_logs", URL: server.URL + "/v1/logs", Format: "otlp"}, nil, record); err != nil {
		t.Fatal(err)
	}
	if gotContentType != "application/x-protobuf" {
		t.Fatalf("content type %q", gotContentType)
	}
	var exportRequest collogspb.ExportLogsServiceRequest
	if err := proto.Unmarshal(gotRequest, &exportRequest); err != nil {
		t.Fatal(err)
	}
	if len(exportRequest.ResourceLogs) != 1 || len(exportRequest.ResourceLogs[0].ScopeLogs[0].LogRecords) != 1 {
		t.Fatalf("unexpected OTLP shape: %d resource logs", len(exportRequest.ResourceLogs))
	}
	record0 := exportRequest.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	if body := record0.GetBody().GetStringValue(); body != string(record.Data) {
		t.Fatalf("log body %q, want the final envelope bytes", body)
	}
	resource := exportRequest.ResourceLogs[0].Resource.Attributes
	var installation string
	for _, attribute := range resource {
		if attribute.Key == "olp.installation_id" {
			installation = attribute.Value.GetStringValue()
		}
	}
	if installation != "inst-1" {
		t.Fatalf("installation resource attribute %q", installation)
	}
}
