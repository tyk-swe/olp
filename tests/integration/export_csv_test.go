//go:build integration

package integration_test

import (
	"net/http"
)

func (h *accessHarness) download(b *browser, method, path string, headers map[string]string) (int, http.Header, string) {
	h.t.Helper()
	response, raw := h.do(b, method, path, nil, headers)
	response.Body.Close()
	return response.StatusCode, response.Header, string(raw)
}
