package access

import (
	"bytes"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// UUID equality is by value, not pointer identity.

var update = flag.Bool("update", false, "rewrite golden files from the current behavior")

// golden compares got with testdata/name, or rewrites it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s changed; review the difference as a change of policy and rerun with -update:\n%s", path, got)
	}
}

// docRegion holds the marked region of docs/security.md to rendered, or
// rewrites it under -update.
func docRegion(t *testing.T, name, rendered string) {
	t.Helper()
	path := "../../docs/security.md"
	document, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	start, end := "<!-- "+name+" -->\n", "<!-- /"+name+" -->"
	i, j := bytes.Index(document, []byte(start)), bytes.Index(document, []byte(end))
	if i < 0 || j < i {
		t.Fatalf("docs/security.md has no %s region", name)
	}
	current := string(document[i+len(start) : j])
	if current == rendered {
		return
	}
	if !*update {
		t.Fatalf("docs/security.md describes %s differently from the code; rerun with -update", name)
	}
	updated := append(append(append([]byte{}, document[:i+len(start)]...), rendered...), document[j:]...)
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		t.Fatal(err)
	}
}

// productionSources lists every non-test Go source under internal/ and cmd/,
// at any depth.
func productionSources(t *testing.T) []string {
	t.Helper()
	var sources []string
	for _, root := range []string{"..", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && !entry.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				sources = append(sources, path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(sources) < 100 {
		t.Fatalf("found only %d sources", len(sources))
	}
	return sources
}
