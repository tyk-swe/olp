// Package console serves the separately built client-only SvelteKit console.
package console

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
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
	indexVariants := map[string][]byte{}
	for _, variant := range []struct{ coding, file string }{
		{"br", "index.html.br"},
		{"gzip", "index.html.gz"},
	} {
		compressed, err := root.ReadFile(variant.file)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			root.Close()
			return nil, nil, errors.New("cannot read compressed console index")
		}
		indexVariants[variant.coding] = compressed
	}
	indexInfo, err := fs.Stat(assets, "index.html")
	if err != nil {
		root.Close()
		return nil, nil, errors.New("cannot inspect console index")
	}
	var scriptPolicy strings.Builder
	scriptPolicy.WriteString("'self'")
	for _, script := range regexp.MustCompile(`(?s)<script(?:\s[^>]*)?>(.*?)</script>`).FindAllSubmatch(index, -1) {
		hash := sha256.Sum256(script[1])
		scriptPolicy.WriteString(" 'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) + "'")
	}
	csp := "default-src 'self'; script-src " + scriptPolicy.String() + "; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'"
	files := http.FileServerFS(assets)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
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
			variants := map[string]fs.FileInfo{}
			for _, variant := range []struct{ coding, file string }{
				{"br", name + ".br"},
				{"gzip", name + ".gz"},
			} {
				variantInfo, err := fs.Stat(assets, variant.file)
				if err == nil && !variantInfo.IsDir() {
					variants[variant.coding] = variantInfo
				}
			}
			if len(variants) == 0 {
				files.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Vary", "Accept-Encoding")
			coding := acceptedCoding(r.Header.Get("Accept-Encoding"), variants["br"] != nil, variants["gzip"] != nil)
			if coding == "" {
				files.ServeHTTP(w, r)
				return
			}
			variantName := name + ".br"
			if coding == "gzip" {
				variantName = name + ".gz"
			}
			file, err := assets.Open(variantName)
			if err != nil {
				files.ServeHTTP(w, r)
				return
			}
			seeker, seekable := file.(io.ReadSeeker)
			if !seekable {
				file.Close()
				files.ServeHTTP(w, r)
				return
			}
			defer file.Close()
			w.Header().Set("Content-Encoding", coding)
			contentType := mime.TypeByExtension(path.Ext(name))
			if contentType == "" {
				contentType = "application/octet-stream"
			}
			w.Header().Set("Content-Type", contentType)
			http.ServeContent(w, r, name, variants[coding].ModTime(), seeker)
			return
		}
		if name != "index.html" && (path.Ext(name) != "" || strings.HasPrefix(name, "_app/")) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		body := index
		if len(indexVariants) > 0 {
			w.Header().Set("Vary", "Accept-Encoding")
			_, hasBr := indexVariants["br"]
			_, hasGzip := indexVariants["gzip"]
			if coding := acceptedCoding(r.Header.Get("Accept-Encoding"), hasBr, hasGzip); coding != "" {
				body = indexVariants[coding]
				w.Header().Set("Content-Encoding", coding)
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
			}
		}
		http.ServeContent(w, r, "index.html", indexInfo.ModTime(), bytes.NewReader(body))
	}), root.Close, nil
}

// acceptedCoding returns the available compression coding preferred by the
// request's Accept-Encoding — br wins ties — or "" when every variant is
// refused or unlisted. A coding's quality is its own q-value, falling back to
// the wildcard's; zero is a refusal.
func acceptedCoding(header string, hasBr, hasGzip bool) string {
	qualities := map[string]float64{}
	for _, entry := range strings.Split(header, ",") {
		coding, parameters, _ := strings.Cut(entry, ";")
		coding = strings.ToLower(strings.TrimSpace(coding))
		if coding == "" {
			continue
		}
		quality := 1.0
		for _, parameter := range strings.Split(parameters, ";") {
			key, value, found := strings.Cut(parameter, "=")
			if !found || !strings.EqualFold(strings.TrimSpace(key), "q") {
				continue
			}
			if parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil {
				quality = parsed
			}
		}
		qualities[coding] = quality
	}
	preferred := ""
	preferredQuality := 0.0
	for _, candidate := range []struct {
		coding string
		exists bool
	}{
		{"br", hasBr},
		{"gzip", hasGzip},
	} {
		if !candidate.exists {
			continue
		}
		quality, listed := qualities[candidate.coding]
		if !listed {
			quality = qualities["*"]
		}
		if quality > preferredQuality {
			preferred, preferredQuality = candidate.coding, quality
		}
	}
	return preferred
}
