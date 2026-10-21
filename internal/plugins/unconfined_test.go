package plugins

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// Other files are not executables OLP lists.

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
