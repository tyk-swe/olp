package staging

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
			func(err error) bool { return errors.Is(err, ErrUnavailable) },
		},
		"an upload without a limit": {
			strings.NewReader("1"), 0,
			func(err error) bool { return errors.Is(err, ErrZeroLimit) },
		},
		"an upload whose source fails": {
			io.MultiReader(strings.NewReader("12"), brokenSource{}), 64,
			func(err error) bool { return errors.Is(err, ErrUnavailable) },
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

type brokenSource struct{}

func (brokenSource) Read([]byte) (int, error) { return 0, errors.New("source closed") }

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
