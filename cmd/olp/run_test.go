package main

import (
	"net/http"
	"net/http/httptest"
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
