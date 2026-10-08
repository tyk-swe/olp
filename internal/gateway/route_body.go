package gateway

import (
	"io"

	"github.com/tyk-swe/olp/internal/runtime"
)

// measuredBody counts ingress bytes only when a pinned release has route body
// limits. It retains no content. Decoded JSON and declared lengths are checked
// independently so gzip and chunked uploads cannot evade the bound.
type measuredBody struct {
	io.ReadCloser
	read, declared, decoded int64
}

func (b *measuredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	return n, err
}
func (b *measuredBody) size() int64 {
	if b == nil {
		return 0
	}
	return max(b.read, b.declared, b.decoded)
}
func (x *execution) checkBody(route *runtime.Route) *Error {
	if route == nil || x.request.body == nil {
		return nil
	}
	// Retained resources may return a historical serving route; ingress policy
	// comes from the request's pinned release.
	if x.request.release != nil && x.request.release.Snapshot != nil {
		if current, ok := x.request.release.Snapshot.Routes[route.Slug]; ok {
			route = &current
		}
	}
	if route.MaxBodyBytes != nil && x.request.body.size() > *route.MaxBodyBytes {
		return bodyTooLarge()
	}
	return nil
}
