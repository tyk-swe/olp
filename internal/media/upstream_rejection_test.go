package media

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
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

func TestTransportSaturatesHugeRetryAfter(t *testing.T) {
	transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "18446744073709551616")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"slow down"}}`)
	}))
	call := &UpstreamCall{Method: "POST", Path: "images/generations", JSON: []byte(`{}`), Kind: ResponseImages, Ambiguous: true}
	_, failure := transport.Do(context.Background(), testTarget(server.URL+"/v1"), call, nil)
	if failure == nil || failure.Class != ClassRateLimit || failure.Status != http.StatusTooManyRequests {
		t.Fatalf("rate limit rejection: %+v", failure)
	}
	if !failure.Dispatched || failure.Ambiguous {
		t.Fatalf("rate-limit dispatch state: %+v", failure)
	}
	saturated := time.Duration(uint64((1<<63-1)/time.Second)) * time.Second
	if failure.RetryAfter != saturated {
		t.Fatalf("Retry-After: %v, want saturated %v", failure.RetryAfter, saturated)
	}
}

func TestMultipartEarlyRejectionPreservesProviderStatus(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			release := make(chan struct{})
			transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.NewResponseController(w).EnableFullDuplex()
				body := `{"error":{"message":"request rejected"}}`
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(status)
				io.WriteString(w, body)
				w.(http.Flusher).Flush()
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer close(release)
			artifact, err := transport.Spool.PutBytes(t.Context(), "image.png", "image/png", make([]byte, 16<<20), 16<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.Spool.Remove(artifact.Handle)
			request := &Request{Op: OpImageEdit, Route: "images", Prompt: "photo", Images: []Part{{Handle: artifact.Handle}}}
			call, e := Encode(request, "openai", "image-model")
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			_, failure := transport.Do(ctx, testTarget(server.URL+"/v1"), call, request)
			class := ClassCredential
			if status == 429 {
				class = ClassRateLimit
			}
			if failure == nil || failure.Status != status || failure.Class != class || failure.Ambiguous || ctx.Err() != nil {
				t.Fatalf("early rejection lost: %+v, context=%v", failure, ctx.Err())
			}
			if status == 429 && failure.RetryAfter != 2*time.Second {
				t.Fatalf("lost Retry-After: %+v", failure)
			}
		})
	}
}
