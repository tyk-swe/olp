package media

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"
)

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
