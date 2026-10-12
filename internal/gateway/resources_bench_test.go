package gateway

import (
	"fmt"
	"strings"
	"testing"
)

func BenchmarkNativeResourceIdentity(b *testing.B) {
	const maxBytes = 1 << 20
	for _, kind := range []string{"file", "batch"} {
		id := kind + "-up-1"
		for _, payloadBytes := range []int{0, maxBytes} {
			body := `{"id":"` + id + `","object":"` + kind + `","status":"completed","pad":""}`
			if payloadBytes > 0 {
				body = strings.Replace(body, `"pad":""`, `"pad":"`+strings.Repeat("x", payloadBytes-len(body))+`"`, 1)
			}
			raw := []byte(body)
			for _, native := range []bool{false, true} {
				b.Run(fmt.Sprintf("%s/bytes=%d/native=%t", kind, len(raw), native), func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(raw)))
					for b.Loop() {
						var got string
						var valid bool
						if native {
							got, valid = nativeResourceID(raw, maxBytes)
						} else {
							got, valid = upstreamString(raw, "id")
						}
						if !valid || got != id {
							b.Fatalf("native identity extraction: id=%q valid=%t", got, valid)
						}
					}
				})
			}
		}
	}
}
