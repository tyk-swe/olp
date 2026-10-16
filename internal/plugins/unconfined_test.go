package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// unconfinedFixture is an unconfined plugin directory holding the fixture
// plugin, built natively, as the executable named fixture.
func unconfinedFixture(t *testing.T, limits Limits, log *slog.Logger) (*Unconfined, ExecutableFile) {
	t.Helper()
	dir := t.TempDir()
	testutil.BuildExecutablePlugin(t, filepath.Join(dir, "fixture"), "./internal/plugins/testdata/fixture")
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	u := NewUnconfined(dir, limits, log)
	files, err := u.Executables()
	if err != nil || len(files) != 1 {
		t.Fatalf("executables %+v: %v", files, err)
	}
	return u, files[0]
}

// newUnconfinedHost is a host whose only plugin is the unconfined plugin
// file in the unconfined tier u.
func newUnconfinedHost(t *testing.T, u *Unconfined, file ExecutableFile) *Host {
	t.Helper()
	host := NewHost(newTestRuntime(t, DefaultLimits, u.log), u, &pluginTable{executable: file.Name})
	t.Cleanup(func() { host.Close(context.Background()) })
	return host
}

func sign(t *testing.T, host *Host, digest, credential string) (abi.SignResult, error) {
	t.Helper()
	return host.Sign(t.Context(), digest, fixtureProvider, signRequest(credential, []byte("{}")), []string{credential})
}

// Other files are not executables OLP lists.

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// An executable that doesn't speak the ABI over stdio, or speaks another
// version of it, is refused when the owner reviews it.
func TestUnconfinedTierRefusesExecutablesThatDontSpeakItsABI(t *testing.T) {
	t.Parallel()
	u := NewUnconfined(t.TempDir(), DefaultLimits, slog.New(slog.DiscardHandler))
	for name, tc := range map[string]struct{ body, code string }{
		"otherabi": {`echo '{"abi_version":2}'; cat >/dev/null`, CodeABIUnsupported},
		"silent":   {`exit 0`, CodeExecutableInvalid},
		"chatty":   {`echo 'hello'; cat >/dev/null`, CodeExecutableInvalid},
	} {
		script(t, u, name, tc.body)
		if _, _, err := u.Inspect(t.Context(), name); !isCode(err, tc.code) {
			t.Errorf("%s: want %s, got %v", name, tc.code, err)
		}
	}
}

// Concurrent calls are served concurrently: one waiting for its
// cancellation doesn't hold up another.

// Started after the loop, so the loop's time limit passes first.

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// script writes an executable shell script into the unconfined tier's
// directory and returns it.
func script(t *testing.T, u *Unconfined, name, body string) ExecutableFile {
	t.Helper()
	if err := os.WriteFile(filepath.Join(u.dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := u.executable(name)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// The plugin answered the call its caller's deadline cancelled, so the
// call's time limit passes without OLP stopping the plugin.

// Both record costs are divisible by three, so no concurrent order
// can exactly exhaust the power-of-two budget without a warning.
