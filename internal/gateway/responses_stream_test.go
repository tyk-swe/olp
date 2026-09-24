package gateway

import (
	"bytes"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestStrictResponseProjectionPreservesNativeSourceAndParentIdentity(t *testing.T) {
	source := []byte(`{"id":"resp_up","object":"response","status":"in_progress","model":"native-model","previous_response_id":"resp_parent","opaque":{"big":9007199254740993,"zero":-0,"order":[3,1]}}`)
	doc, err := oif.ParseJSON(source, oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	projection := responseProjection{upstreamID: "resp_up", localID: "strict_response_local", route: "published-route", previousUpstream: "resp_parent", previousLocal: "strict_response_parent"}
	mapped, err := projection.project(doc, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"id":"strict_response_local","object":"response","status":"in_progress","model":"published-route","previous_response_id":"strict_response_parent","opaque":{"big":9007199254740993,"zero":-0,"order":[3,1]}}`)
	if !bytes.Equal(mapped.Bytes(), want) || !bytes.Equal(doc.Bytes(), source) {
		t.Fatalf("native response source changed beyond identity overlays:\n got %s\nwant %s", mapped.Bytes(), want)
	}
	for _, changed := range [][]byte{
		bytes.Replace(source, []byte(`"id":"resp_up"`), []byte(`"id":"other"`), 1),
		bytes.Replace(source, []byte(`"previous_response_id":"resp_parent"`), []byte(`"previous_response_id":"other"`), 1),
		bytes.Replace(source, []byte(`"status":"in_progress"`), []byte(`"status":"queued|completed"`), 1),
	} {
		invalid, err := oif.ParseJSON(changed, oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := projection.project(invalid, ""); err == nil {
			t.Fatalf("unbound response admitted: %s", changed)
		}
	}
}

func TestStrictResponseStreamProjectionRetainsSSEAndOpaqueNumbers(t *testing.T) {
	frame := []byte("id: cursor-7\nevent: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_up\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"native\",\"native\":{\"large\":9007199254740993,\"zero\":-0}}}\n\n")
	projection := responseProjection{upstreamID: "resp_up", localID: "strict_response_local", route: "published-route"}
	out, original, err := projectStoredResponseFrame(frame, projection, true)
	if err != nil || !bytes.HasPrefix(out, []byte("id: cursor-7\nevent: response.created\ndata: ")) ||
		!bytes.Contains(out, []byte(`"id":"strict_response_local"`)) ||
		!bytes.Contains(out, []byte(`"model":"published-route"`)) ||
		!bytes.Contains(out, []byte(`"large":9007199254740993,"zero":-0`)) ||
		!bytes.Contains(original, []byte(`"id":"resp_up"`)) || bytes.Contains(out, []byte(`"id":"resp_up"`)) {
		t.Fatalf("native stream projection: err=%v output=%s original=%s", err, out, original)
	}
}

func TestResponseRetrievalQueryForwardsOnlyRegisteredControls(t *testing.T) {
	query, stream, err := responseRetrievalQuery("stream=true&starting_after=7&include%5B%5D=reasoning.encrypted_content&include_obfuscation=false")
	if err != nil || !stream || query.Get("starting_after") != "7" || query.Get("include[]") != "reasoning.encrypted_content" || query.Get("include_obfuscation") != "false" {
		t.Fatalf("SDK retrieval controls changed: query=%v stream=%t err=%v", query, stream, err)
	}
	for _, raw := range []string{
		"starting_after=7", "stream=true&starting_after=-1", "stream=true&starting_after=1&starting_after=2",
		"stream=true&include%5B%5D=unqualified.field", "stream=true&include_obfuscation=maybe", "stream=true&endpoint=https%3A%2F%2Fevil.example",
	} {
		if _, _, err := responseRetrievalQuery(raw); err == nil {
			t.Fatalf("unqualified retrieval query admitted: %s", raw)
		}
	}
}

func TestFailedResponseFrameRedactsDecodedCredentialsWithoutChangingOtherSource(t *testing.T) {
	frame := []byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_up\",\"status\":\"failed\",\"error\":{\"message\":\"provider\\u002dsecret\"},\"native\":{\"big\":9007199254740993,\"zero\":-0}}}\n\n")
	safe, err := redactFailedResponseFrame(frame, []string{"provider-secret"})
	if err != nil || !bytes.Contains(safe, []byte(`"message":"[REDACTED]"`)) ||
		!bytes.Contains(safe, []byte(`"big":9007199254740993,"zero":-0`)) ||
		bytes.Contains(safe, []byte(`provider\u002dsecret`)) {
		t.Fatalf("escaped credential survived redaction or native bytes changed: %v %s", err, safe)
	}
	maliciousKey := bytes.Replace(frame, []byte(`"native"`), []byte(`"provider\u002dsecret"`), 1)
	if _, err := redactFailedResponseFrame(maliciousKey, []string{"provider-secret"}); err == nil {
		t.Fatal("escaped credential in a native member name was forwarded")
	}
}
