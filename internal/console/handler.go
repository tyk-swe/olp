// Package console serves the separately built client-only SvelteKit console.
package console

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path"
	"regexp"
	"strings"
)

// Handler permits Vite development when the asset directory is absent. os.Root
// confines packaged assets, including symlinks, to the configured directory.
func Handler(directory string) (http.Handler, func() error, error) {
	root, err := os.OpenRoot(directory)
	if errors.Is(err, fs.ErrNotExist) {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Console assets are unavailable; build the console or use Vite.", http.StatusServiceUnavailable)
		}), func() error { return nil }, nil
	}
	if err != nil {
		return nil, nil, errors.New("cannot open OLP_CONSOLE_DIR")
	}
	assets := root.FS()
	index, err := root.ReadFile("index.html")
	if err != nil {
		root.Close()
		return nil, nil, errors.New("OLP_CONSOLE_DIR has no readable index.html")
	}
	indexInfo, err := fs.Stat(assets, "index.html")
	if err != nil {
		root.Close()
		return nil, nil, errors.New("cannot inspect console index")
	}
	scriptPolicy := "'self'"
	for _, script := range regexp.MustCompile(`(?s)<script(?:\s[^>]*)?>(.*?)</script>`).FindAllSubmatch(index, -1) {
		hash := sha256.Sum256(script[1])
		scriptPolicy += " 'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) + "'"
	}
	csp := "default-src 'self'; script-src " + scriptPolicy + "; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'"
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", csp)
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if name == "" || name == "." {
			name = "index.html"
		}
		info, err := fs.Stat(assets, name)
		if err == nil && !info.IsDir() && name != "index.html" {
			if strings.HasPrefix(name, "_app/immutable/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
			files.ServeHTTP(w, r)
			return
		}
		if name != "index.html" && (path.Ext(name) != "" || strings.HasPrefix(name, "_app/")) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", indexInfo.ModTime(), bytes.NewReader(index))
	}), root.Close, nil
}
