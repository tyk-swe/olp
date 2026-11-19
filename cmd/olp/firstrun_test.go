package main

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/sys/unix"
)

func TestInitSecretsProducesAStartableInstallationAndKeepsExistingFiles(t *testing.T) {
	t.Setenv("PGPASSWORD", "")
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "unused"))
	bundledURL := func(dir string) string {
		query := url.Values{"passfile": {filepath.Join(dir, "pgpass")}}
		return "postgres://olp@postgres:5432/olp?" + query.Encode()
	}
	dir, databaseDir := t.TempDir(), t.TempDir()
	var created bytes.Buffer
	if err := initSecrets([]string{"--dir", dir, "--database-dir", databaseDir}, &created); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadSecrets(context.Background(), Config{Mode: All, AuthHMACKeyFile: filepath.Join(dir, "olp_auth_hmac_key"), MasterKeyFile: filepath.Join(dir, "olp_master_key")}, "installation"); err != nil {
		t.Fatalf("generated keys do not load: %v", err)
	}
	t.Setenv("OLP_BOOTSTRAP_TOKEN_FILE", filepath.Join(dir, "olp_bootstrap_token"))
	t.Setenv("OLP_PUBLIC_ORIGIN", "https://olp.example/")
	var printed bytes.Buffer
	if err := setupToken(os.Getenv, &printed); err != nil {
		t.Fatal(err)
	}
	token := read(t, filepath.Join(dir, "olp_bootstrap_token"))
	if !strings.Contains(printed.String(), "https://olp.example/setup") || !strings.Contains(printed.String(), token) {
		t.Fatalf("setup-token printed %q", printed.String())
	}
	database, err := pgconn.ParseConfig(bundledURL(dir))
	if err != nil {
		t.Fatal(err)
	}
	if want := read(t, filepath.Join(databaseDir, "postgres_password")); database.Password != want || len(want) < 32 {
		t.Fatalf("pgpass password does not match the PostgreSQL password file")
	}

	before := snapshot(t, dir, databaseDir)
	var rerun bytes.Buffer
	if err := initSecrets([]string{"--dir", dir, "--database-dir", databaseDir}, &rerun); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir, databaseDir); after != before || rerun.Len() != 0 {
		t.Fatalf("rerun changed existing secrets or reported creating files: %q", rerun.String())
	}

	t.Run("preserves the existing PostgreSQL password", func(t *testing.T) {
		dir, databaseDir := t.TempDir(), t.TempDir()
		const password = "  legacy:secret\\\t "
		if err := os.WriteFile(filepath.Join(databaseDir, "postgres_password"), []byte(password), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := initSecrets([]string{"--dir", dir, "--database-dir", databaseDir}, io.Discard); err != nil {
			t.Fatal(err)
		}
		const databaseURL = "postgres://olp@postgres:5432/olp"
		env := map[string]string{
			"OLP_DATABASE_PASSWORD_FILE":     filepath.Join(dir, "postgres_password"),
			"OLP_DATABASE_PASSWORD_FILE_URL": databaseURL,
		}
		customPassfile := filepath.Join(t.TempDir(), "pgpass")
		const customPassword = "external-password"
		if err := os.WriteFile(customPassfile, []byte("*:5432:olp:olp:"+customPassword+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		passfile := url.Values{"passfile": {customPassfile}}.Encode()

		for _, tc := range []struct{ name, url, want string }{
			{"bundled URL", databaseURL, password},
			{"bundled URL with options", databaseURL + "?sslmode=disable&connect_timeout=5", password},
			{"bundled URL with implicit database", "postgres://olp@postgres:5432?sslmode=disable", password},
			{"query host and port select the bundled endpoint", "postgresql://olp@external:1234/olp?host=postgres&port=5432", password},
			{"query host selects an external endpoint", databaseURL + "?host=external", ""},
			{"query port selects a different endpoint", databaseURL + "?port=5433", ""},
			{"query user selects another account", databaseURL + "?user=another", ""},
			{"URL selects another database", "postgres://olp@postgres:5432/other", ""},
			{"fallback selects an external endpoint", databaseURL + "?host=postgres,external", ""},
			{"URL supplies its own password", databaseURL + "?password=" + customPassword, customPassword},
			{"bundled URL supplies a passfile", databaseURL + "?" + passfile, customPassword},
			{"external URL supplies a passfile", "postgres://olp@external:5432/olp?" + passfile, customPassword},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c, err := parseWithFlags([]string{"all"}, func(key string) string {
					if key == "OLP_DATABASE_URL" {
						return tc.url
					}
					return env[key]
				}, io.Discard, nil)
				if err != nil {
					t.Fatal(err)
				}
				database, err := pgconn.ParseConfig(c.DatabaseURL)
				if err != nil || database.Password != tc.want {
					t.Fatal("the connection did not use the expected password")
				}
			})
		}
	})

	t.Run("refuses a nonregular bootstrap token", func(t *testing.T) {
		dir, databaseDir := t.TempDir(), t.TempDir()
		tokenPath := filepath.Join(dir, "olp_bootstrap_token")
		if err := os.Mkdir(tokenPath, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := initSecrets([]string{"--dir", dir, "--database-dir", databaseDir}, io.Discard); err == nil {
			t.Fatal("initializer accepted a directory in place of the bootstrap token")
		}
		t.Setenv("OLP_BOOTSTRAP_TOKEN_FILE", tokenPath)
		if err := setupToken(os.Getenv, io.Discard); err == nil {
			t.Fatal("setup-token accepted a directory in place of the bootstrap token")
		}
	})

	t.Run("reads a projected bootstrap token", func(t *testing.T) {
		dir := t.TempDir()
		const revision = "..revision"
		if err := os.Mkdir(filepath.Join(dir, revision), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, revision, "olp_bootstrap_token"), []byte(token+"\n"), 0o440); err != nil {
			t.Fatal(err)
		}
		projectedFile := filepath.Join(dir, "olp_bootstrap_token")
		if err := os.Symlink(revision, filepath.Join(dir, "..data")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("..data/olp_bootstrap_token", projectedFile); err != nil {
			t.Fatal(err)
		}
		t.Setenv("OLP_BOOTSTRAP_TOKEN_FILE", projectedFile)
		var printed bytes.Buffer
		if err := setupToken(os.Getenv, &printed); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(printed.String(), token) {
			t.Fatal("setup-token did not print the mounted token")
		}
	})
}

func TestMountedDatabasePasswordRejectsInvalidFiles(t *testing.T) {
	for _, test := range []struct {
		name   string
		create func(string) error
	}{
		{"world readable", func(path string) error {
			if err := os.WriteFile(path, []byte("fixture\n"), 0o600); err != nil {
				return err
			}
			return os.Chmod(path, 0o644)
		}},
		{"oversized", func(path string) error { return os.WriteFile(path, bytes.Repeat([]byte("x"), 65537), 0o600) }},
		{"named pipe", func(path string) error { return unix.Mkfifo(path, 0o600) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "postgres_password")
			if err := test.create(path); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{
				"OLP_DATABASE_URL":           "postgres://olp@postgres:5432/olp",
				"OLP_DATABASE_PASSWORD_FILE": path,
			}
			result := make(chan error, 1)
			go func() {
				_, err := Parse([]string{"all"}, func(name string) string { return env[name] }, io.Discard)
				result <- err
			}()
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), "OLP_DATABASE_PASSWORD_FILE") {
					t.Fatalf("expected a mounted password error, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("reading the mounted password blocked startup")
			}
		})
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func snapshot(t *testing.T, dirs ...string) string {
	t.Helper()
	var out strings.Builder
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			out.WriteString(entry.Name() + "=" + read(t, filepath.Join(dir, entry.Name())) + "\n")
		}
	}
	return out.String()
}
