package console_test

import (
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
