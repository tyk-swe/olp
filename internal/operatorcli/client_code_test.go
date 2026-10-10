package operatorcli

import (
	"bytes"
	"context"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGeneratedClientSourceEscapesMountedPathsAndDoesNotCopyCredentials(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "mounted ' key")
	if err := os.WriteFile(key, []byte("private-fixture-key"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, client := range []string{"openai", "anthropic", "gemini"} {
		for _, format := range []string{"javascript", "python", "go"} {
			t.Run(client+"/"+format, func(t *testing.T) {
				var out bytes.Buffer
				runner := Runner{Out: &out}
				err := runner.Run(context.Background(), []string{"client-env", client, "--format", format, "--url", "http://127.0.0.1:8080", "--key-file", key, "--model", "route\"quoted"})
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(out.String(), "private-fixture-key") || !strings.Contains(out.String(), "mounted ' key") {
					t.Fatal("key was embedded or path lost")
				}
				if format == "go" {
					if _, err := parser.ParseFile(token.NewFileSet(), "config.go", out.Bytes(), parser.AllErrors); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestClientCodeRejectsUnsupportedFormatsAndSurfaceConfusion(t *testing.T) {
	for _, entry := range [][3]string{{"agents", "python", "openai"}, {"agents", "javascript", "anthropic"}, {"langchain", "go", "openai"}, {"openai", "unknown", "openai"}} {
		if _, err := clientCode(entry[0], entry[1], entry[2], "https://olp.example.com", "/run/key", "assistant"); err == nil {
			t.Fatalf("accepted %v", entry)
		}
	}
}
