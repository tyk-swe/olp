package media

import (
	"io"
	"net/http"
	"testing"
)

func TestTransportPreservesDecodedClientRejections(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		call *UpstreamCall
	}{
		{
			name: "BFL moderation",
			body: `{"id":"task-1","status":"Request Moderated","result":null}`,
			call: &UpstreamCall{Method: http.MethodPost, Path: "flux-2-pro", JSON: []byte(`{}`), Kind: ResponseImages, Ambiguous: true, Next: nextBFLStep},
		},
		{
			name: "Stability content filter",
			body: `{"image":"","finish_reason":"CONTENT_FILTERED"}`,
			call: &UpstreamCall{Method: http.MethodPost, Path: "images/generations", JSON: []byte(`{}`), Kind: ResponseImages, Ambiguous: true, DecodeImages: decodeStabilityImage},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, tc.body)
			}))
			result, failure := transport.Do(t.Context(), testTarget(server.URL+"/v1"), tc.call, nil)
			if result != nil || failure == nil || failure.Class != ClassUpstreamClient || failure.Status != http.StatusBadRequest {
				t.Fatalf("decoded rejection: result=%+v failure=%+v", result, failure)
			}
			if !failure.Dispatched || failure.Ambiguous || failure.Upstream == nil || failure.Upstream.Code != "content_filter" {
				t.Fatalf("definitive content-filter rejection lost: %+v", failure)
			}
		})
	}
}
