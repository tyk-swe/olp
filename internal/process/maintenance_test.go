package process

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDebitReconciliationStopsAfterCancellationWithUnavailableLedger(t *testing.T) {
	pool, err := pgxpool.New(t.Context(), "postgres://olp:olp@127.0.0.1:1/olp?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan struct{})
	go func() {
		runDebitReconciliation(ctx, pool, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled debit worker waited for the unavailable ledger or next reconciliation interval")
	}
}

func passwordFixture(t *testing.T, content []byte, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(path, content, perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPasswordFileRejectsUnsafeInputs(t *testing.T) {
	for name, tc := range map[string]struct {
		content []byte
		perm    os.FileMode
	}{
		"group readable": {[]byte("a sufficiently long passphrase"), 0o640},
		"other writable": {[]byte("a sufficiently long passphrase"), 0o602},
		"oversized":      {[]byte(strings.Repeat("p", 4098)), 0o600},
		"not utf8":       {[]byte{'p', 'a', 's', 's', 0xff, 0xfe}, 0o600},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := readPasswordFile(passwordFixture(t, tc.content, tc.perm)); err == nil {
				t.Fatal("accepted an unsafe password file")
			}
		})
	}
	if _, err := readPasswordFile(t.TempDir()); err == nil {
		t.Fatal("accepted a directory as a password file")
	}
	if _, err := readPasswordFile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("accepted a missing password file")
	}
	if got, err := readPasswordFile(passwordFixture(t, []byte(strings.Repeat("p", 4097)), 0o600)); err != nil || len(got) != 4097 {
		t.Fatal("rejected a password file at the size boundary", err)
	}
}
