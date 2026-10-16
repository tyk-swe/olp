//go:build integration

package integration_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

type operationFixture struct {
	*httptest.Server
	mu           sync.Mutex
	calls        []strictProviderCall
	response     string
	responseType string
	expectedPath string
	policy       any
}

func newOperationFixture(t *testing.T, path, response string) *operationFixture {
	t.Helper()
	f := &operationFixture{response: response, responseType: "application/json", expectedPath: path}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			writeJSON(w, map[string]any{"data": []any{map[string]string{"id": vendorModel}}})
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, strictProviderCall{body, r.Header.Clone(), r.URL.Query()})
		response := f.response
		contentType := f.responseType
		f.mu.Unlock()
		if r.URL.Path != path {
			t.Errorf("native path %s, want %s", r.URL.Path, path)
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(f.Close)
	return f
}
func (f *operationFixture) snapshot() []strictProviderCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]strictProviderCall(nil), f.calls...)
}
func (f *operationFixture) result(value string) { f.mu.Lock(); defer f.mu.Unlock(); f.response = value }
func (f *operationFixture) resultType(value string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.responseType = value
}
