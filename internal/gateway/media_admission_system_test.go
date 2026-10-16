//go:build integration

package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/media"
)

// mediaTestPolicy mirrors the loopback egress policy the harnesses use.
func mediaTestPolicy() *egress.Policy {
	return &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
}

// secondMediaGateway builds an independently constructed gateway sharing the
// harness runtime, upstream, and limiter — the shape two replicas take.
func secondMediaGateway(t *testing.T, h *harness, limiter *limits.Limiter) (*Server, *httptest.Server, *capture) {
	t.Helper()
	gw := New(h.rt, mediaTestPolicy(), Config{MaxInFlight: 8, MaxBodyBytes: 64 * 1024, MaxResponseBytes: 1 << 20, MaxEventBytes: 4096}, h.gateway.log)
	spool, err := media.NewSpool(t.TempDir(), media.MinCapacityBytes, h.gateway.log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	gw.Media = &MediaDeps{
		Jobs: &media.Service{Log: h.gateway.log, Transport: &media.Transport{
			Client: gw.client, Auth: gw.auth, Egress: gw.egress,
			Spool: spool, MaxResponseBytes: 1 << 20,
		}},
		Admission: media.NewAdmissionState(media.MinCapacityBytes),
	}
	gw.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, h.gateway.log)
	sink := &capture{}
	gw.Sink = sink
	mux := http.NewServeMux()
	gw.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return gw, server, sink
}

func postMedia(t *testing.T, url string, body []byte, headers map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fullKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var decoded map[string]any
	data, _ := io.ReadAll(resp.Body)
	if len(data) > 0 {
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("invalid JSON response %q: %v", data, err)
		}
	}
	return resp, decoded
}

// The second gateway has no local memory of the rejection: only the
// shared cooldown can keep provider a's slot out of this dispatch.

// durable reservation can no longer persist

// A preference that would exclude the pinned provider during planning is
// not a reason to reroute — or refuse — the recorded job.

// Header validation still applies to retained operations.
