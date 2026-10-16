package gateway

import (
	"bytes"
	"testing"
)

func TestStrictResponseStreamProjectionRetainsSSEAndOpaqueNumbers(t *testing.T) {
	frame := []byte("id: cursor-7\nevent: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_up\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"native\",\"native\":{\"large\":9007199254740993,\"zero\":-0}}}\n\n")
	projection := responseProjection{upstreamID: "resp_up", localID: "strict_response_local", route: "published-route"}
	out, observed, err := projectStoredResponseFrame(frame, projection, true, nil)
	if err != nil || !bytes.HasPrefix(out, []byte("id: cursor-7\nevent: response.created\ndata: ")) ||
		!bytes.Contains(out, []byte(`"id":"strict_response_local"`)) ||
		!bytes.Contains(out, []byte(`"model":"published-route"`)) ||
		!bytes.Contains(out, []byte(`"large":9007199254740993,"zero":-0`)) ||
		observed == nil || observed.Status != "in_progress" || observed.Complete || bytes.Contains(out, []byte(`"id":"resp_up"`)) {
		t.Fatalf("native stream projection: err=%v output=%s observed=%+v", err, out, observed)
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
