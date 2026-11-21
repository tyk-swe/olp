package retained

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

func TestRetainedGeminiKeyQueryParamDropped(t *testing.T) {
	t.Parallel()
	endpoint := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "context_cache", Surface: "gemini", Kind: "context_cache"}, Action: "list"}
	server := surface{kernel: &kernel.Server{}}
	query, failure := server.retainedQuery(t.Context(), &kernel.Call{}, endpoint, nil, url.Values{"key": {"secret"}, "pageSize": {"10"}})
	if failure != nil {
		t.Fatalf("retainedQuery with key: %v", failure)
	}
	if _, forwarded := query["key"]; forwarded {
		t.Fatalf("key forwarded upstream in %v", query)
	}
	if query.Get("pageSize") != "10" {
		t.Fatalf("pageSize = %v, want 10", query)
	}
	openAIEndpoint := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "vector_store", Surface: "openai", Kind: "vector_store"}, Action: "list"}
	if _, failure := server.retainedQuery(t.Context(), &kernel.Call{}, openAIEndpoint, nil, url.Values{"key": {"secret"}}); failure == nil {
		t.Fatal("key accepted on non-Gemini surface")
	}
	request := httptest.NewRequest(http.MethodGet, "/gemini/v1beta/cachedContents?key=secret&pageSize=0", nil)
	var captured *fault.Error
	status := server.listRetainedGeneric(httptest.NewRecorder(), request, &kernel.Call{}, keyauthority.Authority{}, endpoint, func(err *fault.Error) { captured = err })
	if status != 400 || captured == nil || !strings.Contains(captured.Message, "Page size") {
		t.Fatalf("list with key and pageSize=0 = %d %+v, want 400 page size error", status, captured)
	}
}

func TestRetainedUploadRefusesAMultipartBodyCutBeforeItsFinalBoundary(t *testing.T) {
	t.Parallel()
	skill := retainedapi.Endpoint{Family: retainedapi.Family{Operation: "skill"}, Action: "create", Method: http.MethodPost}
	complete := "--frame\r\nContent-Disposition: form-data; name=\"file\"; filename=\"skill.zip\"\r\n\r\ncontent\r\n--frame--\r\n"
	server := surface{kernel: &kernel.Server{Config: kernel.Config{MaxBodyBytes: 1 << 20}}}
	for name, body := range map[string]string{
		"empty":         "",
		"cut in a part": strings.TrimSuffix(complete, "--frame--\r\n"),
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/skills", strings.NewReader(body))
			request.Header.Set("Content-Type", "multipart/form-data; boundary=frame")
			if _, _, _, failure := server.readRetainedBody(request, skill); failure == nil || failure.Status != http.StatusBadRequest || failure.Code != "invalid_request" {
				t.Fatalf("truncated upload = %+v, want 400 invalid_request", failure)
			}
		})
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/skills", strings.NewReader(complete))
	request.Header.Set("Content-Type", "multipart/form-data; boundary=frame")
	if read, _, _, failure := server.readRetainedBody(request, skill); failure != nil || string(read) != complete {
		t.Fatalf("complete upload = %q %+v, want the body back", read, failure)
	}
}
