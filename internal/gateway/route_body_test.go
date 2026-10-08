package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/runtime"
)

func routeBodyLimit(t *testing.T, rt *fakeRuntime, slug string, limit int64) {
	t.Helper()
	route := rt.release.Snapshot.Routes[slug]
	route.MaxBodyBytes = &limit
	rt.release.Snapshot.Routes[slug] = route
	if err := rt.release.Snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
}
func TestRouteBodyLimitsCoverEncodedDecodedAndNativeIdentification(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, compressed := range []bool{false, true} {
			for _, delta := range []int64{-1, 0, 1} {
				t.Run(strings.Join([]string{map[bool]string{true: "native", false: "plain"}[native], map[bool]string{true: "gzip", false: "identity"}[compressed], string(rune('b' + delta))}, "/"), func(t *testing.T) {
					var h *harness
					if native {
						h = endUserHarness(t, "native")
					} else {
						h = newHarness(t, Config{})
					}
					h.mock.set("a", completion(modelA, "ok"))
					body := []byte(`{"model":"` + routeSlug + `","user":"body-user","messages":[{"role":"user","content":"` + strings.Repeat("x", 1024) + `"}]}`)
					wire := body
					headers := map[string]string{}
					if compressed {
						var out bytes.Buffer
						z := gzip.NewWriter(&out)
						_, _ = z.Write(body)
						_ = z.Close()
						wire = out.Bytes()
						headers["Content-Encoding"] = "gzip"
					}
					routeBodyLimit(t, h.rt, routeSlug, int64(len(body))+delta)
					response := h.do(context.Background(), "POST", "/v1/chat/completions", fullKey, wire, headers)
					defer response.Body.Close()
					want := http.StatusOK
					if delta < 0 {
						want = http.StatusRequestEntityTooLarge
					}
					if response.StatusCode != want {
						data, _ := io.ReadAll(response.Body)
						t.Fatalf("status=%d want=%d: %s", response.StatusCode, want, data)
					}
					if delta < 0 && h.mock.count("a")+h.mock.count("b") != 0 {
						t.Fatal("oversized body dispatched")
					}
				})
			}
		}
	}
}
func TestRouteBodyLimitDoesNotRaiseInstallationCap(t *testing.T) {
	h := newHarness(t, Config{MaxInFlight: 8, MaxBodyBytes: 64, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096})
	routeBodyLimit(t, h.rt, routeSlug, 1024)
	response, _ := h.chat(fullKey, nil, `,"extra":"`+strings.Repeat("x", 80)+`"`)
	defer response.Body.Close()
	if response.StatusCode != 413 {
		t.Fatalf("installation cap bypassed: %d", response.StatusCode)
	}
}
func TestRouteBodyLimitValidation(t *testing.T) {
	for _, size := range []int64{-1, 0, 1, 1 << 30, (1 << 30) + 1} {
		b := runtime.Behavior{MaxBodyBytes: &size}
		if (b.Validate("route", nil) == nil) != (size > 0 && size <= 1<<30) {
			t.Fatalf("size %d validation", size)
		}
		if b.IsZero() {
			t.Fatal("body limit treated as unconfigured")
		}
	}
}

func TestFallbackCannotDispatchThroughASmallerBodyLimit(t *testing.T) {
	h := newHarness(t, Config{})
	h.republish(func(s *runtime.Snapshot, _, b runtime.Provider) {
		splitRoutes(s, b, runtime.Behavior{Fallbacks: []runtime.Fallback{{Route: backupSlug, On: []string{runtime.FallbackExhausted}}}})
		route := s.Routes[backupSlug]
		limit := int64(1)
		route.MaxBodyBytes = &limit
		s.Routes[backupSlug] = route
	})
	h.mock.set("a", status(http.StatusServiceUnavailable, `{"error":{"message":"down","type":"server_error"}}`))
	response, _ := h.chat(fullKey, nil)
	defer response.Body.Close()
	if response.StatusCode == 200 || h.mock.count("b") != 0 {
		t.Fatal("fallback bypassed ingress limit")
	}
}

func TestCodeBodyLimitsRefuseBeforeDurableAdmission(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(map[bool]string{false: "identity", true: "gzip"}[compressed], func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			for slug, route := range h.rt.release.Snapshot.CodeRoutes {
				size := int64(128)
				route.MaxBodyBytes = &size
				h.rt.release.Snapshot.CodeRoutes[slug] = route
			}
			body := []byte(`{"model":"native-model","input":"` + strings.Repeat("x", 1024) + `","stream":true}`)
			headers := http.Header{"Content-Type": []string{"application/json"}}
			if compressed {
				var out bytes.Buffer
				z := gzip.NewWriter(&out)
				_, _ = z.Write(body)
				_ = z.Close()
				body = out.Bytes()
				headers.Set("Content-Encoding", "gzip")
			}
			response := codeDo(t, server, body, headers)
			defer response.Body.Close()
			if response.StatusCode != 413 {
				data, _ := io.ReadAll(response.Body)
				t.Fatalf("code body: %d %s", response.StatusCode, data)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.inputs) != 0 || len(ledger.marks) != 0 || h.mock.count("a") != 0 {
				t.Fatal("oversized code request admitted")
			}
		})
	}
}
