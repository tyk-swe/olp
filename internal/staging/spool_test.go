package staging

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func stage(t *testing.T, spool *Spool, body io.Reader, maximum int64) (*Artifact, error) {
	t.Helper()
	return spool.Put(t.Context(), Upload{Filename: "clip.wav", MaximumLength: maximum, Body: body})
}

func TestSpoolGivesBackEveryByteItDoesNotKeep(t *testing.T) {
	spool := newSpool(t)
	spool.capacity = 10

	kept, err := stage(t, spool, strings.NewReader("123456"), 64)
	if err != nil {
		t.Fatal(err)
	}
	refused := map[string]struct {
		body    io.Reader
		maximum int64
		is      func(error) bool
	}{
		"an upload over its own limit": {
			strings.NewReader("12345"), 4,
			func(err error) bool { _, ok := errors.AsType[*TooLargeError](err); return ok },
		},
		"an upload over the spool capacity": {
			io.MultiReader(strings.NewReader("1234"), strings.NewReader("5")), 64,
			func(err error) bool { return errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrSourceRead) },
		},
		"an upload without a limit": {
			strings.NewReader("1"), 0,
			func(err error) bool { return errors.Is(err, ErrZeroLimit) },
		},
		"an upload whose source fails": {
			io.MultiReader(strings.NewReader("12"), brokenSource{}), 64,
			func(err error) bool { return errors.Is(err, ErrUnavailable) && errors.Is(err, ErrSourceRead) },
		},
	}
	for name, c := range refused {
		t.Run(name, func(t *testing.T) {
			if _, err := stage(t, spool, c.body, c.maximum); !c.is(err) {
				t.Fatalf("got %v", err)
			}
			if spool.UsedBytes() != kept.ContentLength {
				t.Fatalf("got %d bytes in use, want only the %d kept", spool.UsedBytes(), kept.ContentLength)
			}
			left, err := os.ReadDir(spool.root)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 2 {
				t.Fatalf("got %d files in the spool, want the kept upload and the ownership lock", len(left))
			}
		})
	}

	if err := spool.Remove(kept.Handle); err != nil {
		t.Fatal(err)
	}
	if err := spool.Remove(kept.Handle); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removing twice: got %v, want not found", err)
	}
	if _, err := spool.Open(kept.Handle); !errors.Is(err, ErrNotFound) {
		t.Fatalf("opening a removed upload: got %v, want not found", err)
	}
	if spool.UsedBytes() != 0 {
		t.Fatalf("got %d bytes in use after removing everything", spool.UsedBytes())
	}
	if _, err := stage(t, spool, strings.NewReader("1234567890"), 64); err != nil {
		t.Fatalf("the freed capacity was not reusable: %v", err)
	}
}

type brokenSource struct{ beforeRead func() }

func (s brokenSource) Read([]byte) (int, error) {
	if s.beforeRead != nil {
		s.beforeRead()
	}
	return 0, io.ErrUnexpectedEOF
}

func TestSpoolRefusesHandlesAndNamesThatCouldLeaveIt(t *testing.T) {
	spool := newSpool(t)
	outside := filepath.Join(filepath.Dir(spool.root), "outside")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, handle := range []Handle{"../outside", "", "0123456789ABCDEF0123456789ABCDEF", Handle(strings.Repeat("a", handleBytes+1)), ".owner.lock"} {
		if _, err := spool.Open(handle); !errors.Is(err, ErrInvalidHandle) {
			t.Fatalf("opening %q: got %v, want an invalid handle", handle, err)
		}
		if err := spool.Remove(handle); !errors.Is(err, ErrInvalidHandle) {
			t.Fatalf("removing %q: got %v, want an invalid handle", handle, err)
		}
	}
	if _, err := spool.Open(newHandle()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("opening a handle nobody staged: got %v, want not found", err)
	}

	names := map[string]string{
		"clip.wav":               "clip.wav",
		"../../etc/passwd":       "passwd",
		`C:\Users\ada\clip.wav`:  "clip.wav",
		"/":                      "",
		"..":                     "",
		"":                       "",
		"clip\x00.wav":           "",
		"clip\n.wav":             "",
		strings.Repeat("a", 256): "",
	}
	for raw, want := range names {
		got, err := SafeFilename(raw)
		if got != want || (want == "") != errors.Is(err, ErrInvalidFilename) {
			t.Fatalf("SafeFilename(%q) = %q, %v, want %q", raw, got, err, want)
		}
	}
}

func TestNewSpoolReclaimsOnlySpoolsNobodyOwns(t *testing.T) {
	base := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	live, err := NewSpool(base, MinCapacityBytes, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { live.Close() })
	kept, err := stage(t, live, strings.NewReader("live upload"), 64)
	if err != nil {
		t.Fatal(err)
	}
	abandoned := filepath.Join(base, "olp-media-1-crashed")
	unregistered := filepath.Join(base, "olp-media-2-unregistered")
	unrelated := filepath.Join(base, "operator-notes")
	for _, dir := range []string{abandoned, unregistered, unrelated} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "leftover"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(abandoned, spoolOwnerFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewSpool(base, MinCapacityBytes, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })

	for _, dir := range []string{abandoned, unregistered} {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived a restart: %v", filepath.Base(dir), err)
		}
	}
	if _, err := os.Stat(filepath.Join(unrelated, "leftover")); err != nil {
		t.Fatalf("a directory that is not a spool was touched: %v", err)
	}
	opened, err := live.Open(kept.Handle)
	if err != nil {
		t.Fatalf("a restart elsewhere removed a live spool's upload: %v", err)
	}
	opened.File.Close()

	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closing left the spool directory behind: %v", err)
	}
}

func unremovablePath(t *testing.T, root, name string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "kept"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func blockSpoolRemoval(t *testing.T, spool *Spool, name string) string {
	t.Helper()
	if err := os.Remove(filepath.Join(spool.root, name)); err != nil {
		t.Fatal(err)
	}
	return unremovablePath(t, spool.root, name)
}

func TestSpoolCleanupGivesUpAndStopsOnClose(t *testing.T) {
	removeStuck := func(spool *Spool) string {
		t.Helper()
		artifact, err := stage(t, spool, strings.NewReader("1234567890"), 10)
		if err != nil {
			t.Fatal(err)
		}
		dir := blockSpoolRemoval(t, spool, string(artifact.Handle))
		if err := spool.Remove(artifact.Handle); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("Remove returned %v for an artifact it could not remove", err)
		}
		return dir
	}
	retrying := newSpool(t)
	stuck := removeStuck(retrying)
	deadline := time.Now().Add(3 * time.Second)
	for retrying.UsedBytes() != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if retrying.UsedBytes() != 0 {
		t.Fatalf("got %d bytes still in use for a path that cannot be removed, want the reservation released once cleanup gives up", retrying.UsedBytes())
	}
	if _, err := os.Stat(filepath.Join(stuck, "kept")); err != nil {
		t.Fatalf("the unremovable path went away, so the release did not come from giving up: %v", err)
	}
	if err := retrying.Close(); err != nil {
		t.Fatal(err)
	}

	before := runtime.NumGoroutine()
	spool := newSpool(t)
	settled := runtime.NumGoroutine()
	for range 24 {
		removeStuck(spool)
	}
	growth := 0
	until := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(until) {
		growth = max(growth, runtime.NumGoroutine()-settled)
		time.Sleep(10 * time.Millisecond)
	}
	if growth > cleanupConcurrency+2 {
		t.Fatalf("got %d extra goroutines for 24 cleanup jobs, want at most %d", growth, cleanupConcurrency+2)
	}
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if spool.UsedBytes() != 0 {
		t.Fatalf("got %d bytes still in use after closing, want every cleanup reservation released", spool.UsedBytes())
	}
	deadline = time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+4 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > before+4 {
		t.Fatalf("got %d goroutines after closing, want the cleanup workers stopped (started from %d)", got, before)
	}
}

func TestSaturatedSpoolCleanupReturnsBeforeRetryingArtifacts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		spool := newSpool(t)
		const failedRemovals = 40
		artifacts := make([]Handle, failedRemovals)
		for i := range artifacts {
			artifact, err := stage(t, spool, strings.NewReader("x"), 1)
			if err != nil {
				t.Fatal(err)
			}
			blockSpoolRemoval(t, spool, string(artifact.Handle))
			artifacts[i] = artifact.Handle
		}
		removed := make(chan error, failedRemovals)
		go func() {
			for _, handle := range artifacts {
				removed <- spool.Remove(handle)
			}
			close(removed)
		}()
		synctest.Wait()
		if len(removed) != failedRemovals {
			t.Error("Remove waited for cleanup retries")
		}
		if spool.UsedBytes() != failedRemovals {
			t.Error("spool released a reservation while artifacts still existed")
		}
		for _, handle := range artifacts {
			if err := os.Remove(filepath.Join(spool.root, string(handle), "kept")); err != nil {
				t.Fatal(err)
			}
		}
		for err := range removed {
			if !errors.Is(err, ErrUnavailable) {
				t.Errorf("Remove returned %v for an artifact it could not remove", err)
			}
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if spool.UsedBytes() != 0 {
			t.Errorf("cleanup left %d bytes reserved", spool.UsedBytes())
		}
		for _, handle := range artifacts {
			if _, err := os.Stat(filepath.Join(spool.root, string(handle))); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("cleanup left an artifact behind: %v", err)
			}
		}
	})
}

func TestAbortedEmptyUploadsConsumeSpoolSlotsUntilCleanup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const slots = 8
		spool := newSpool(t)
		spool.fileSlots = make(chan struct{}, slots)
		var blocked []string
		source := brokenSource{beforeRead: func() {
			entries, err := os.ReadDir(spool.root)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if !entry.IsDir() && entry.Name() != spoolOwnerFile {
					blocked = append(blocked, blockSpoolRemoval(t, spool, entry.Name()))
					return
				}
			}
			t.Fatal("the incomplete upload file was not found")
		}}
		for range slots {
			if _, err := stage(t, spool, source, 1); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("upload abort returned %v, want unexpected EOF", err)
			}
		}
		synctest.Wait()
		if spool.UsedBytes() != 0 {
			t.Fatal("the aborted uploads reserved payload bytes")
		}
		if _, err := stage(t, spool, strings.NewReader(""), 1); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("the spool accepted another file with all cleanup slots occupied: %v", err)
		}
		for _, dir := range blocked {
			if err := os.Remove(filepath.Join(dir, "kept")); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(time.Second)
		synctest.Wait()
		for range slots {
			if _, err := stage(t, spool, strings.NewReader(""), 1); err != nil {
				t.Fatalf("a file slot was still unavailable after cleanup: %v", err)
			}
		}
	})
}
