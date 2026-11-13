package main

import (
	"errors"
	"flag"
	"github.com/tyk-swe/olp/internal/admission"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func TestControlNeverAnswersConsumerProtocolPathsWithTheConsole(t *testing.T) {
	public := http.NewServeMux()
	public.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("console")) })
	reservePublicPaths(public, Control)
	for _, request := range []struct{ method, path string }{
		{http.MethodPost, "/mcp?toolset_id=ts"},
		{http.MethodGet, "/mcp/sse?toolset_id=ts"},
		{http.MethodPost, "/mcp/messages?session_id=s"},
		{http.MethodPost, "/a2a/agent"},
		{http.MethodGet, "/a2a/agent/.well-known/agent-card.json"},
	} {
		recorder := httptest.NewRecorder()
		public.ServeHTTP(recorder, httptest.NewRequest(request.method, request.path, nil))
		if recorder.Code != http.StatusNotFound || recorder.Body.String() == "console" {
			t.Errorf("%s %s answered %d %q, want a reserved 404", request.method, request.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestReencryptDryRunAcceptsConfigurationFlagsBeforeIt(t *testing.T) {
	for _, options := range [][]string{
		{"--dry-run", "--database-url", "invalid"},
		{"--database-url", "invalid", "--dry-run"},
		{"--database-url", "invalid", "--dry-run=true"},
		{"--database-url", "invalid", "--dry-run=false"},
	} {
		args := append([]string{"master-key", "reencrypt"}, options...)
		if err := run(t.Context(), append(args, "--help")); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("reencrypt %v: %v, want help after accepted flags", options, err)
		}
	}
}

func TestA2AUsesInferenceAdmissionAndPerimeter(t *testing.T) {
	management := admission.NewPool(1)
	occupied := management.AcquirePermit()
	defer occupied.Release()
	inference := admission.NewPool(1)
	public := &admission.Public{Inference: inference, Management: management, InferenceEnabled: true, Reject: rejectPublic}
	handler := Perimeter("https://example.invalid", public.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })))
	for _, path := range []string{"/a2a/agent", "/a2a/agent/.well-known/agent-card.json"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, nil))
		if recorder.Code != http.StatusOK || recorder.Header().Get("Cross-Origin-Resource-Policy") != "" || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("A2A %s status=%d headers=%v", path, recorder.Code, recorder.Header())
		}
	}
	managementResponse := httptest.NewRecorder()
	handler.ServeHTTP(managementResponse, httptest.NewRequest(http.MethodGet, "/api/v1/a2a-tasks", nil))
	if managementResponse.Code != http.StatusServiceUnavailable {
		t.Fatal("operator A2A tasks bypassed the occupied management pool")
	}
	busyInference := inference.AcquirePermit()
	defer busyInference.Release()
	inferenceResponse := httptest.NewRecorder()
	handler.ServeHTTP(inferenceResponse, httptest.NewRequest(http.MethodPost, "/a2a/agent", nil))
	if inferenceResponse.Code != http.StatusServiceUnavailable {
		t.Fatal("A2A bypassed the occupied inference pool")
	}
	filtered := managementNetwork([]netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	for path, want := range map[string]int{"/a2a/agent": http.StatusOK, "/api/v1/a2a-tasks": http.StatusForbidden} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, path, nil)
		request.RemoteAddr = "198.51.100.1:1234"
		filtered.ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("network filter %s status=%d, want %d", path, response.Code, want)
		}
	}
}
