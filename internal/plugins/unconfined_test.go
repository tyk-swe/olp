package plugins

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	host := NewHost(newTestRuntime(t, DefaultLimits, u.log), u, &pluginTable{executable: file.Name})
	t.Cleanup(func() { host.Close(context.Background()) })
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
	disabled := NewHost(newTestRuntime(t, DefaultLimits, nil), nil, &pluginTable{executable: file.Name})
	defer disabled.Close(context.Background())
	if _, err := sign(t, disabled, file.Digest, "sk-fixture"); !isCode(err, CodeUnconfinedDisabled) {
		t.Fatalf("an unconfined plugin ran without the tier: %v", err)
	}
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

// A caller whose own deadline passes before the plugin answers cancels its
// call and nothing else: the plugin's other calls, and the process serving
// them, carry on.
func TestUnconfinedCallersDeadlineCancelsOnlyItsCall(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits
	limits.Time = 2 * time.Second
	u, file := unconfinedFixture(t, limits, nil)
	host := newUnconfinedHost(t, u, file)
	if _, err := sign(t, host, file.Digest, "sk-fixture"); err != nil {
		t.Fatal(err)
	}
	waiting, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	var waited error
	wg.Go(func() {
		_, waited = host.Sign(waiting, file.Digest, fixtureProvider, signRequest("wait", nil), nil)
	})
	hurried, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	if _, err := host.Sign(hurried, file.Digest, fixtureProvider, signRequest("wait", nil), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a call past its caller's deadline returned %v", err)
	}
	time.Sleep(time.Second)
	cancel()
	wg.Wait()
	if !errors.Is(waited, context.Canceled) {
		t.Fatalf("a call in flight returned %v", waited)
	}
	// The plugin answered the call its caller's deadline cancelled, so the
	// call's time limit passes without OLP stopping the plugin.
	time.Sleep(limits.Time - time.Second + 500*time.Millisecond)
	if result, err := sign(t, host, file.Digest, "sk-fixture"); err != nil || result.Headers["X-Fixture-Calls"] != "4" {
		t.Fatalf("the plugin was stopped: %+v %v", result, err)
	}
}

// OLP runs a sealed copy of the executable whose digest it checked, never
// the file by its path, so what runs is what it hashed.
func TestUnconfinedPluginRunsTheCopyItHashed(t *testing.T) {
	t.Parallel()
	u, file := unconfinedFixture(t, DefaultLimits, nil)
	executable := u.Load(file.Digest, file.Name)
	defer executable.Close(context.Background())
	if _, err := inspect(t.Context(), executable, true); err != nil {
		t.Fatal(err)
	}
	executable.mu.Lock()
	pid := executable.running.cmd.Process.Pid
	executable.mu.Unlock()
	image, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	if err != nil || !strings.HasPrefix(image, "/memfd:olp-plugin") {
		t.Fatalf("the plugin runs %q, not the sealed copy: %v", image, err)
	}
}

// A plugin that exits fails its calls at once and is reaped, and what it
// started is stopped with it, even while that still holds its output.
func TestUnconfinedPluginIsReapedWithWhatItStarted(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits
	limits.Time = 5 * time.Second
	u := NewUnconfined(t.TempDir(), limits, slog.New(slog.DiscardHandler))
	file := script(t, u, "forking", `sleep 60 &
echo $! > sleeper
echo '{"abi_version":1}'
read request
exit 3`)
	host := newUnconfinedHost(t, u, file)
	started := time.Now()
	_, err := sign(t, host, file.Digest, "sk-fixture")
	if !isCode(err, CodeFailed) || !strings.Contains(err.Error(), "exit status 3") || time.Since(started) > 2*time.Second {
		t.Fatalf("a call on a plugin that exited returned %v after %s", err, time.Since(started))
	}
	pid, err := os.ReadFile(filepath.Join(u.dir, "sleeper"))
	if err != nil {
		t.Fatal(err)
	}
	stat := fmt.Sprintf("/proc/%s/stat", strings.TrimSpace(string(pid)))
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		state, err := os.ReadFile(stat)
		if errors.Is(err, os.ErrNotExist) || err == nil && strings.Contains(string(state), ") Z ") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("what the plugin started outlived it: %s", state)
		}
	}
}

// A plugin that exits is reaped at once, even while something it started
// outside its process group holds its output open.
func TestUnconfinedPluginIsReapedWhileItsOutputIsHeld(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits
	limits.Time = 5 * time.Second
	u := NewUnconfined(t.TempDir(), limits, slog.New(slog.DiscardHandler))
	file := script(t, u, "escaping", `setsid sh -c 'echo $$ > escaped; exec sleep 60' &
while [ ! -s escaped ]; do sleep 0.01; done
echo '{"abi_version":1}'
read request
exit 3`)
	t.Cleanup(func() {
		if pid, err := os.ReadFile(filepath.Join(u.dir, "escaped")); err == nil {
			if escaped, err := strconv.Atoi(strings.TrimSpace(string(pid))); err == nil {
				syscall.Kill(escaped, syscall.SIGKILL)
			}
		}
	})
	host := newUnconfinedHost(t, u, file)
	started := time.Now()
	_, err := sign(t, host, file.Digest, "sk-fixture")
	if !isCode(err, CodeFailed) || !strings.Contains(err.Error(), "exit status 3") || time.Since(started) > drainTime+time.Second {
		t.Fatalf("a call on a plugin that exited returned %v after %s", err, time.Since(started))
	}
}

// Calls waiting for the plugin to start share one start, and a caller that
// stops waiting leaves it to the others.
func TestUnconfinedCallsShareOneStart(t *testing.T) {
	t.Parallel()
	u, _ := unconfinedFixture(t, DefaultLimits, nil)
	file := script(t, u, "slow", `echo started >> starts
sleep 1
exec ./fixture`)
	host := newUnconfinedHost(t, u, file)
	hurried, stop := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer stop()
	started := time.Now()
	if _, err := host.Sign(hurried, file.Digest, fixtureProvider, signRequest("sk-fixture", nil), nil); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 500*time.Millisecond {
		t.Fatalf("a caller that stopped waiting returned %v after %s", err, time.Since(started))
	}
	var wg sync.WaitGroup
	calls := make([]string, 3)
	for i := range calls {
		wg.Go(func() {
			result, err := sign(t, host, file.Digest, "sk-fixture")
			if err != nil {
				t.Error(err)
			}
			calls[i] = result.Headers["X-Fixture-Calls"]
		})
	}
	wg.Wait()
	slices.Sort(calls)
	starts, err := os.ReadFile(filepath.Join(u.dir, "starts"))
	if err != nil || string(starts) != "started\n" || !slices.Equal(calls, []string{"1", "2", "3"}) {
		t.Fatalf("the plugin started %q and served %v: %v", starts, calls, err)
	}
}

// What an unconfined plugin writes to standard error while serving a call
// reaches OLP on another pipe than its response, possibly after it, and is
// redacted of the call's secrets all the same.
func TestUnconfinedPluginStandardErrorIsRedactedAfterItsResponse(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewJSONHandler(&lockedWriter{&mu, &output}, nil))
	u, file := unconfinedFixture(t, DefaultLimits, log)
	host := newUnconfinedHost(t, u, file)
	if _, err := sign(t, host, file.Digest, "stderr:"+fixtureSecret); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		logged := output.String()
		mu.Unlock()
		if strings.Contains(logged, "signed with") {
			if strings.Contains(logged, fixtureSecret) || !strings.Contains(logged, `"msg":"signed with [REDACTED]"`) {
				t.Fatalf("plugin log:\n%s", logged)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the plugin's standard error was not logged:\n%s", logged)
		}
	}
}

func TestUnconfinedProcessLogsShareOneBudget(t *testing.T) {
	for _, source := range []string{"stderr", "context-free", "mixed", "redaction updates", "concurrent"} {
		t.Run(source, func(t *testing.T) {
			var logged bytes.Buffer
			p := &process{log: slog.New(slog.NewJSONHandler(&logged, nil)), calls: map[uint64]*pending{}}
			message := strings.Repeat("x", 100)
			emit := func(i int) {
				if source == "redaction updates" || source == "concurrent" {
					p.mu.Lock()
					p.calls[1] = &pending{secrets: []string{fmt.Sprintf("secret-%d", i)}}
					p.mu.Unlock()
				}
				if source == "stderr" || (source == "mixed" || source == "concurrent") && i%2 == 0 {
					p.reading.Add(1)
					p.logStderr(strings.NewReader(message + "\n"))
				} else {
					callOutput(p.grants(0)).record(abi.LogRecord{Message: message})
				}
			}
			var wg sync.WaitGroup
			for i := range 1000 {
				if source == "concurrent" {
					wg.Go(func() { emit(i) })
				} else {
					emit(i)
				}
			}
			wg.Wait()
			spent, warnings := budgetSpent(t, &logged)
			if spent == 0 || spent > maxCallLog || warnings != 1 {
				t.Fatalf("process logs spent %d bytes of a %d byte budget, with %d warnings", spent, maxCallLog, warnings)
			}
		})
	}
}

func TestUnconfinedProcessLogRedactionTracksItsCalls(t *testing.T) {
	var logged bytes.Buffer
	p := &process{log: slog.New(slog.NewJSONHandler(&logged, nil)), limit: time.Minute,
		calls: map[uint64]*pending{}, ended: map[string]time.Time{}}
	write := func(message string) {
		p.reading.Add(1)
		p.logStderr(strings.NewReader(message + "\n"))
	}
	write("before any call")
	p.calls[1] = &pending{secrets: []string{"first-secret"}}
	write("active first-secret")
	p.forget(1)
	p.calls[2] = &pending{secrets: []string{"second-secret"}}
	callOutput(p.grants(0)).record(abi.LogRecord{Message: "ended first-secret; active second-secret"})
	p.ended["first-secret"] = time.Now().Add(-time.Second)
	write("expired first-secret; active second-secret")
	lines := logLines(t, &logged)
	want := []string{"before any call", "active [REDACTED]", "ended [REDACTED]; active [REDACTED]", "expired first-secret; active [REDACTED]"}
	if len(lines) != len(want) {
		t.Fatalf("got %d records, want %d", len(lines), len(want))
	}
	for i, message := range want {
		if lines[i].Message != message {
			t.Errorf("record %d: got %q, want %q", i, lines[i].Message, message)
		}
	}
}
