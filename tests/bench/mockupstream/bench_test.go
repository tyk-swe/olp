//go:build bench

package mockupstream

import (
	"strings"
	"testing"
)

// BenchmarkHandler measures what a request costs the mock itself, with the
// network taken away: the 400 KB body is what a 100K-token prompt weighs, and
// at 3,000 requests per second the figure per operation is the core count the
// mock needs for that load.
func BenchmarkHandler(b *testing.B) {
	s := newMock(b, Config{Default: Behavior{OutputTokens: 16}})
	prompts := map[string]string{"short": "hello", "100K tokens": strings.Repeat("the ", 100_000)}
	for _, size := range []string{"short", "100K tokens"} {
		for _, w := range wires[:2] {
			for name, r := range map[string]request{"unary": w.unary, "stream": w.stream} {
				r.body = strings.Replace(r.body, `"hello"`, `"`+prompts[size]+`"`, 1)
				b.Run(w.name+"/"+name+"/"+size, func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(int64(len(r.body)))
					b.RunParallel(func(pb *testing.PB) {
						h := newHarness(b, s, r)
						for pb.Next() {
							h.serve()
						}
					})
				})
			}
		}
	}
}
