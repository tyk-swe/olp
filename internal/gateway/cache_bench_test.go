package gateway

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/secrets"
)

func benchmarkResponseCache(b *testing.B, client cache.Commander) {
	for _, mode := range []string{"bypass", "hit"} {
		b.Run(mode, func(b *testing.B) {
			ring, err := secrets.ParseRing([]byte(`{"active_version":1,"keys":[{"version":1,"key":"` + strings.Repeat("1", 64) + `"}]}`))
			if err != nil {
				b.Fatal(err)
			}
			store := &cache.Store{Client: client, Keys: ring, Auth: secrets.NewAuthKey(bytes.Repeat([]byte{2}, 32), "bench"), Installation: "bench"}
			transport := &benchTransport{status: 200, body: []byte(benchChat), header: http.Header{"Content-Type": {"application/json"}}}
			handler := benchGateway(b, transport, "gpt-bench", benchLimits{responseCache: store, cachePolicy: &cache.Policy{Mode: "exact"}})
			raw := []byte(`{"model":"` + routeSlug + `","messages":[{"role":"user","content":"Hello"}],"max_tokens":128}`)
			body := bytes.NewReader(raw)
			req := httptest.NewRequestWithContext(b.Context(), http.MethodPost, "/v1/chat/completions", body)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+fullKey)
			writer := &benchWriter{header: make(http.Header, 24)}
			serve := func() { body.Reset(raw); req.Body = io.NopCloser(body); writer.reset(); handler.ServeHTTP(writer, req) }
			deadline := time.Now().Add(5 * time.Second)
			for {
				serve()
				if writer.header.Get("X-OLP-Cache") == "hit" || time.Now().After(deadline) {
					break
				}
			}
			if writer.status != 200 || writer.header.Get("X-OLP-Cache") != "hit" {
				b.Fatalf("cache did not warm: status=%d result=%s", writer.status, writer.header.Get("X-OLP-Cache"))
			}
			if mode == "bypass" {
				req.Header.Set("X-OLP-Cache", "no-store")
			}
			samples := make([]int64, 8192)
			n := 0
			b.ReportAllocs()
			for b.Loop() {
				start := time.Now()
				serve()
				samples[n%len(samples)] = time.Since(start).Nanoseconds()
				n++
				if mode == "hit" && writer.header.Get("X-OLP-Cache") != "hit" {
					b.Fatalf("cache lookup failed during measurement: %s", writer.header.Get("X-OLP-Cache"))
				}
			}
			samples = samples[:min(n, len(samples))]
			slices.Sort(samples)
			b.ReportMetric(float64(samples[(len(samples)-1)*95/100]), "p95-ns/op")
		})
	}
}

func BenchmarkGatewayResponseCache(b *testing.B) {
	benchmarkResponseCache(b, &cacheMemory{values: map[string]string{}})
}
