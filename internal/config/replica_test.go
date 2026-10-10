package config

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeReadDatabaseSupportsMountedURLsAndFlagPrecedence(t *testing.T) {
	file := filepath.Join(t.TempDir(), "replica-url")
	os.WriteFile(file, []byte("postgres://replica/db\n"), 0600)
	env := map[string]string{"OLP_DATABASE_URL": "postgres://primary/db", "OLP_DATABASE_READ_URL_FILE": file}
	getenv := func(name string) string { return env[name] }
	c, err := Parse([]string{"gateway"}, getenv, io.Discard)
	if err != nil || c.DatabaseReadURL != "postgres://replica/db" {
		t.Fatalf("replica=%s err=%v", c.DatabaseReadURL, err)
	}
	delete(env, "OLP_DATABASE_READ_URL_FILE")
	env["OLP_DATABASE_READ_URL"] = "postgres://environment/db"
	c, err = Parse([]string{"gateway", "--database-read-url", "postgres://flag/db"}, getenv, io.Discard)
	if err != nil || c.DatabaseReadURL != "postgres://flag/db" {
		t.Fatalf("replica=%s err=%v", c.DatabaseReadURL, err)
	}
	env["OLP_DATABASE_READ_URL"] = "https://private:secret@example.com/db"
	_, err = Parse([]string{"gateway"}, getenv, io.Discard)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("invalid replica error = %v", err)
	}
	env["OLP_DATABASE_READ_URL_FILE"] = file
	if _, err = Parse([]string{"gateway"}, getenv, io.Discard); err == nil {
		t.Fatal("inline and mounted replica accepted together")
	}
}
