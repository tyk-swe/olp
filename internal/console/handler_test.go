package console_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/console"
)

func TestAssetsSPAAndRootConfinement(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>console</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "_app/immutable"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "_app/immutable/app.js"), []byte("script"), 0600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("outside root"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	h, close, err := console.Handler(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	for _, tc := range []struct {
		path   string
		status int
		body   string
	}{
		{"/", 200, "console"}, {"/providers/draft", 200, "console"},
		{"/_app/immutable/app.js", 200, "script"}, {"/missing.js", 404, ""}, {"/escape.txt", 404, ""},
	} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) || strings.Contains(w.Body.String(), "outside root") {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body)
		}
	}
}

func TestPrecompressedVariants(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"index.html":                  "index identity",
		"index.html.br":               "index br",
		"_app/immutable/app.js":       "app identity",
		"_app/immutable/app.js.br":    "app br",
		"_app/immutable/app.js.gz":    "app gzip",
		"_app/immutable/style.css":    "style identity",
		"_app/immutable/style.css.gz": "style gzip",
		"favicon.svg":                 "favicon identity",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	h, close, err := console.Handler(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer close()
	for _, tc := range []struct {
		name            string
		method          string
		path            string
		acceptEncoding  string
		body            string
		contentEncoding string
		contentType     string
		vary            bool
		cacheControl    string
	}{
		{name: "br preferred", path: "/_app/immutable/app.js", acceptEncoding: "br, gzip", body: "app br", contentEncoding: "br", contentType: "text/javascript", vary: true, cacheControl: "public, max-age=31536000, immutable"},
		{name: "gzip only", path: "/_app/immutable/app.js", acceptEncoding: "gzip", body: "app gzip", contentEncoding: "gzip", vary: true, cacheControl: "public, max-age=31536000, immutable"},
		{name: "refused codings serve identity", path: "/_app/immutable/app.js", acceptEncoding: "gzip;q=0, br;q=0", body: "app identity", vary: true, cacheControl: "public, max-age=31536000, immutable"},
		{name: "higher q wins", path: "/_app/immutable/app.js", acceptEncoding: "br;q=0.5, gzip", body: "app gzip", contentEncoding: "gzip", vary: true, cacheControl: "public, max-age=31536000, immutable"},
		{name: "wildcard prefers br", path: "/_app/immutable/app.js", acceptEncoding: "*", body: "app br", contentEncoding: "br", vary: true, cacheControl: "public, max-age=31536000, immutable"},
		{name: "no header serves identity", path: "/_app/immutable/app.js", body: "app identity", vary: true, cacheControl: "public, max-age=31536000, immutable"},
		{name: "css without br serves identity", path: "/_app/immutable/style.css", acceptEncoding: "br", body: "style identity", vary: true, cacheControl: "public, max-age=31536000, immutable"},
		{name: "css gzip", path: "/_app/immutable/style.css", acceptEncoding: "gzip, br", body: "style gzip", contentEncoding: "gzip", vary: true, cacheControl: "public, max-age=31536000, immutable"},
		{name: "no variants", path: "/favicon.svg", acceptEncoding: "br", body: "favicon identity", cacheControl: "no-cache"},
		{name: "index br", path: "/", acceptEncoding: "br", body: "index br", contentEncoding: "br", contentType: "text/html; charset=utf-8", vary: true, cacheControl: "no-cache"},
		{name: "spa fallback br", path: "/providers/x", acceptEncoding: "br", body: "index br", contentEncoding: "br", contentType: "text/html; charset=utf-8", vary: true, cacheControl: "no-cache"},
		{name: "head compressed variant", method: http.MethodHead, path: "/_app/immutable/app.js", acceptEncoding: "br", contentEncoding: "br", vary: true, cacheControl: "public, max-age=31536000, immutable"},
	} {
		method := tc.method
		if method == "" {
			method = http.MethodGet
		}
		request := httptest.NewRequest(method, tc.path, nil)
		if tc.acceptEncoding != "" {
			request.Header.Set("Accept-Encoding", tc.acceptEncoding)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, request)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", tc.name, w.Code)
		}
		if tc.body != "" && w.Body.String() != tc.body {
			t.Fatalf("%s: body %q", tc.name, w.Body.String())
		}
		if got := w.Header().Get("Content-Encoding"); got != tc.contentEncoding {
			t.Fatalf("%s: Content-Encoding %q", tc.name, got)
		}
		if tc.contentType != "" && !strings.Contains(w.Header().Get("Content-Type"), tc.contentType) {
			t.Fatalf("%s: Content-Type %q", tc.name, w.Header().Get("Content-Type"))
		}
		if got := w.Header().Get("Vary") == "Accept-Encoding"; got != tc.vary {
			t.Fatalf("%s: Vary %q", tc.name, w.Header().Get("Vary"))
		}
		if got := w.Header().Get("Cache-Control"); got != tc.cacheControl {
			t.Fatalf("%s: Cache-Control %q", tc.name, got)
		}
	}
}
