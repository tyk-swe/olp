package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	r, err := NewRuntime(t.Context(), Interpreted, DefaultLimits, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	host := NewHost(r, u, &pluginTable{executable: file.Name})
	t.Cleanup(func() {
		host.Close(context.Background())
		r.Close(context.Background())
	})
	return host
}

func sign(t *testing.T, host *Host, digest, credential string) (abi.SignResult, error) {
	t.Helper()
	return host.Sign(t.Context(), digest, fixtureProvider, signRequest(credential, []byte("{}")), []string{credential})
}

// The owner sees each executable in the directory by name, digest and size,
// and reviews the manifest it declares.
func TestUnconfinedTierListsItsExecutablesAndTheirManifests(t *testing.T) {
	t.Parallel()
	u, file := unconfinedFixture(t, DefaultLimits, nil)
	executable, err := os.ReadFile(filepath.Join(u.dir, "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if want := (ExecutableFile{Name: "fixture", Digest: digestOf(executable), Size: int64(len(executable))}); file != want {
		t.Fatalf("executable %+v, want %+v", file, want)
	}
	// Other files are not executables OLP lists.
	for name, mode := range map[string]os.FileMode{"notes.txt": 0o644, ".hidden": 0o755, "-flag": 0o755} {
		if err = os.WriteFile(filepath.Join(u.dir, name), []byte("#!/bin/sh\n"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Mkdir(filepath.Join(u.dir, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if files, err := u.Executables(); err != nil || !reflect.DeepEqual(files, []ExecutableFile{file}) {
		t.Fatalf("executables %+v: %v", files, err)
	}
	reviewed, manifest, err := u.Inspect(t.Context(), "fixture")
	if err != nil || reviewed != file || manifest.Name != "fixture" || manifest.Profiles[0].ID != "fixture-chat" {
		t.Fatalf("inspected %+v %+v: %v", reviewed, manifest, err)
	}
	for _, name := range []string{"notes.txt", "missing", "../fixture"} {
		if _, _, err = u.Inspect(t.Context(), name); !isCode(err, CodeExecutableUnknown) {
			t.Fatalf("inspecting %s: %v", name, err)
		}
	}
}

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func isCode(err error, code string) bool {
	refusal, ok := errors.AsType[*Error](err)
	return ok && refusal.Code == code
}

// An executable that doesn't speak the ABI over stdio, or speaks another
// version of it, is refused when the owner reviews it.
func TestUnconfinedTierRefusesExecutablesThatDontSpeakItsABI(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, script := range map[string]string{
		"otherabi": `echo '{"abi_version":2}'; cat >/dev/null`,
		"silent":   `exit 0`,
		"chatty":   `echo 'hello'; cat >/dev/null`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	u := NewUnconfined(dir, DefaultLimits, slog.New(slog.DiscardHandler))
	for name, code := range map[string]string{"otherabi": CodeABIUnsupported, "silent": CodeExecutableInvalid, "chatty": CodeExecutableInvalid} {
		if _, _, err := u.Inspect(t.Context(), name); !isCode(err, code) {
			t.Errorf("%s: want %s, got %v", name, code, err)
		}
	}
}

// One process serves an unconfined plugin's calls, so its state carries
// from one call to the next, as it does for a native program.
func TestUnconfinedPluginSignsInOneProcess(t *testing.T) {
	t.Parallel()
	u, file := unconfinedFixture(t, DefaultLimits, nil)
	host := newUnconfinedHost(t, u, file)
	for want := 1; want <= 3; want++ {
		result, err := sign(t, host, file.Digest, "sk-fixture")
		if err != nil || result.Headers["X-Fixture-Calls"] != strconv.Itoa(want) {
			t.Fatalf("call %d: %+v %v", want, result, err)
		}
	}
	// Concurrent calls are served concurrently: one waiting for its
	// cancellation doesn't hold up another.
	waiting, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	var waited error
	wg.Go(func() {
		_, waited = host.Sign(waiting, file.Digest, fixtureProvider, signRequest("wait", nil), nil)
	})
	if _, err := sign(t, host, file.Digest, "sk-fixture"); err != nil {
		t.Fatal(err)
	}
	cancel()
	wg.Wait()
	if !errors.Is(waited, context.Canceled) {
		t.Fatalf("the cancelled call returned %v", waited)
	}
}

// A process that exits fails the call it was serving, and the next call
// starts the plugin again.
func TestUnconfinedPluginRestartsAfterItExits(t *testing.T) {
	t.Parallel()
	u, file := unconfinedFixture(t, DefaultLimits, nil)
	host := newUnconfinedHost(t, u, file)
	if _, err := sign(t, host, file.Digest, "sk-fixture"); err != nil {
		t.Fatal(err)
	}
	_, err := sign(t, host, file.Digest, "exit")
	if !isCode(err, CodeFailed) || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("a call that stopped the plugin returned %v", err)
	}
	if result, err := sign(t, host, file.Digest, "sk-fixture"); err != nil || result.Headers["X-Fixture-Calls"] != "1" {
		t.Fatalf("the restarted plugin signed %+v: %v", result, err)
	}
}

// A call the plugin leaves unanswered past the time limit fails, and the
// plugin, which may be stuck, is stopped with the calls in flight on it and
// started again for the next call.
func TestUnconfinedPluginRestartsAfterATimeout(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits
	limits.Time = time.Second
	u, file := unconfinedFixture(t, limits, nil)
	host := newUnconfinedHost(t, u, file)
	if _, err := sign(t, host, file.Digest, "sk-fixture"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var inFlight error
	wg.Go(func() {
		// Started after the loop, so the loop's time limit passes first.
		time.Sleep(200 * time.Millisecond)
		_, inFlight = host.Sign(t.Context(), file.Digest, fixtureProvider, signRequest("wait", nil), nil)
	})
	if _, err := sign(t, host, file.Digest, "loop"); !isCode(err, CodeTimedOut) {
		t.Fatalf("a call that never returns returned %v", err)
	}
	wg.Wait()
	if !isCode(inFlight, CodeFailed) || !strings.Contains(inFlight.Error(), "unanswered past its 1s time limit") {
		t.Fatalf("a call in flight on the stopped plugin returned %v", inFlight)
	}
	if result, err := sign(t, host, file.Digest, "sk-fixture"); err != nil || result.Headers["X-Fixture-Calls"] != "1" {
		t.Fatalf("the restarted plugin signed %+v: %v", result, err)
	}
}

// What an unconfined plugin logs for a call is attributed to it and
// redacted of the call's secrets, as a confined plugin's is.
func TestUnconfinedPluginLogsAreRedacted(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewJSONHandler(&lockedWriter{&mu, &output}, nil))
	u, file := unconfinedFixture(t, DefaultLimits, log)
	host := newUnconfinedHost(t, u, file)
	if _, err := sign(t, host, file.Digest, "log:sk-fixture-secret"); err != nil {
		t.Fatal(err)
	}
	_, err := sign(t, host, file.Digest, "fail:sk-fixture-secret")
	if reported, ok := errors.AsType[*abi.Error](err); !ok || reported.Code != "fixture_failed" || strings.Contains(reported.Message, fixtureSecret) {
		t.Fatalf("a reported failure %v", err)
	}
	mu.Lock()
	logged := output.String()
	mu.Unlock()
	if strings.Contains(logged, fixtureSecret) || !strings.Contains(logged, `"msg":"signing with [REDACTED]"`) || !strings.Contains(logged, `"plugin_method":"sign"`) {
		t.Fatalf("plugin log:\n%s", logged)
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// An unconfined plugin runs only as the executable an owner permitted, and
// only where the deployment enables the unconfined tier.
func TestUnconfinedPluginRunsOnlyAsPermitted(t *testing.T) {
	t.Parallel()
	u, file := unconfinedFixture(t, DefaultLimits, nil)
	host := newUnconfinedHost(t, u, file)
	if _, err := sign(t, host, strings.Repeat("0", 64), "sk-fixture"); !isCode(err, CodeExecutableChanged) {
		t.Fatalf("a changed executable ran: %v", err)
	}
	r, err := NewRuntime(t.Context(), Interpreted, DefaultLimits, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close(context.Background())
	disabled := NewHost(r, nil, &pluginTable{executable: file.Name})
	defer disabled.Close(context.Background())
	if _, err = sign(t, disabled, file.Digest, "sk-fixture"); !isCode(err, CodeUnconfinedDisabled) {
		t.Fatalf("an unconfined plugin ran without the tier: %v", err)
	}
}
