package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestInitSecretsProducesAStartableInstallationAndKeepsExistingFiles(t *testing.T) {
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
	t.Setenv("PGPASSFILE", filepath.Join(dir, "pgpass"))
	database, err := pgconn.ParseConfig("postgres://olp@postgres:5432/olp")
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
		env := map[string]string{
			"OLP_DATABASE_PASSWORD_FILE": filepath.Join(dir, "postgres_password"),
			"OLP_DATABASE_URL":           "postgres://olp@postgres:5432/olp",
		}
		c, err := parseWithFlags([]string{"all"}, func(key string) string { return env[key] }, io.Discard, nil)
		if err != nil {
			t.Fatal(err)
		}
		database, err := pgconn.ParseConfig(c.DatabaseURL)
		if err != nil {
			t.Fatal("cannot parse the database URL")
		}
		if database.Password != password {
			t.Fatal("OLP did not use the original password")
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
	})
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
