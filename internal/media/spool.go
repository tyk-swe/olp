// Package media provides the bounded private filesystem spool for staged
// request and response media, the durable media-job records behind the
// asynchronous video surface, the reconciliation worker that keeps those
// records converged with providers, and the media management API.
//
// The spool enforces a byte-accurate capacity bound across concurrent
// uploads, keeps staged artifacts under opaque handles, and guarantees
// cleanup ownership outlives request cancellation.
package media

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/oif"
)

const (
	// ReadChunkBytes bounds every filesystem read and write step.
	ReadChunkBytes = 64 * 1024
	// DefaultCapacityBytes is the production default spool size (1 GiB).
	DefaultCapacityBytes int64 = 1024 * 1024 * 1024
	// MinCapacityBytes is the smallest supported production spool. Multipart
	// admission reserves fixed worst-case endpoint budgets, so a smaller
	// volume cannot safely serve the public media API.
	MinCapacityBytes int64 = 256 * 1024 * 1024
	// HandleBytes is the opaque handle length: 32 lowercase hex characters.
	HandleBytes = 32

	cleanupConcurrency    = 4
	cleanupInitialBackoff = 25 * time.Millisecond
	cleanupMaxBackoff     = 2 * time.Second
)

var (
	// ErrUnavailable reports a filesystem or capacity failure.
	ErrUnavailable = errors.New("media spool unavailable")
	// ErrNotFound reports an unknown or already-removed handle.
	ErrNotFound = errors.New("media handle not found")
	// ErrInvalidHandle reports a malformed handle supplied by a caller.
	ErrInvalidHandle = errors.New("invalid media handle")
	// ErrInvalidFilename reports an empty, oversized, or unsafe filename.
	ErrInvalidFilename = errors.New("invalid media filename")
	// ErrZeroLimit reports an upload configured with no length bound.
	ErrZeroLimit = errors.New("media uploads require a length bound")
)

// TooLargeError reports a media body that exceeded its configured bound.
type TooLargeError struct{ Maximum int64 }

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("media body exceeds the configured limit of %d bytes", e.Maximum)
}

// TooLarge reports whether err is a media body bound violation.
func TooLarge(err error) bool {
	var target *TooLargeError
	return errors.As(err, &target)
}

// Handle is an opaque spool artifact identifier.
type Handle string

// NewHandle returns a fresh unguessable handle.
func NewHandle() Handle {
	id := uuid.Must(uuid.NewV7())
	return Handle(strings.ReplaceAll(id.String(), "-", ""))
}

// ValidateHandle rejects malformed handles before any filesystem use.
func ValidateHandle(value string) error {
	if len(value) != HandleBytes {
		return ErrInvalidHandle
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return ErrInvalidHandle
		}
	}
	return nil
}

// Artifact describes a committed spool object.
type Artifact struct {
	Handle        Handle
	Digest        string // SHA-256 of the exact committed bytes, lowercase hex
	ContentType   string
	ContentLength int64
}

// BlobReference uses this spool's opaque handle and original byte identity.
// Missing caller Content-Type stays absent on the Part; the generic MIME here
// describes storage bytes and does not invent a native request control.
func (a Artifact) BlobReference() (oif.BlobReference, error) {
	mediaType := a.ContentType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return oif.NewBlobReference(string(a.Handle), a.Digest, mediaType, a.ContentLength)
}

// Upload is a single bounded inbound media stream.
type Upload struct {
	Filename      string
	ContentType   string
	MaximumLength int64
	Body          io.Reader
}

// Opened is a spool artifact opened for reading.
type Opened struct {
	Artifact Artifact
	Filename string
	File     *os.File
}

type spoolEntry struct {
	path          string
	filename      string
	digest        string
	contentType   string
	contentLength int64
}

// Spool is a per-process, private filesystem spool for bounded request and
// response media. usedBytes covers both in-flight reservations and committed
// artifacts so concurrent uploads cannot overrun capacity.
type Spool struct {
	root     string
	owner    *os.File
	capacity int64
	log      *slog.Logger

	mu      sync.Mutex
	entries map[string]spoolEntry

	usedBytes  atomic.Int64
	janitorSem chan struct{}
	closed     atomic.Bool
}

// NewSpool reclaims abandoned spools and creates a private, capacity-bounded
// spool. An ownership lock protects each live spool from restart cleanup.
func NewSpool(baseDir string, capacity int64, log *slog.Logger) (*Spool, error) {
	if capacity < MinCapacityBytes {
		return nil, fmt.Errorf("media spool capacity must be at least %d bytes", MinCapacityBytes)
	}
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, err
	}
	root, owner, err := openSpoolDirectory(baseDir)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	s := &Spool{
		root:       root,
		owner:      owner,
		capacity:   capacity,
		log:        log,
		entries:    map[string]spoolEntry{},
		janitorSem: make(chan struct{}, cleanupConcurrency),
	}
	return s, nil
}

// CapacityBytes reports the configured spool bound.
func (s *Spool) CapacityBytes() int64 { return s.capacity }

// UsedBytes reports bytes currently committed or reserved.
func (s *Spool) UsedBytes() int64 { return s.usedBytes.Load() }

func (s *Spool) tryReserve(bytes int64) bool {
	for {
		used := s.usedBytes.Load()
		next := used + bytes
		if next < used || next > s.capacity {
			return false
		}
		if s.usedBytes.CompareAndSwap(used, next) {
			return true
		}
	}
}

func (s *Spool) release(bytes int64) {
	if bytes != 0 {
		s.usedBytes.Add(-bytes)
	}
}

// SafeFilename reduces an untrusted filename to a safe basename and applies
// the length and character policy shared by request and response media.
func SafeFilename(value string) (string, error) {
	filename := path.Base(strings.ReplaceAll(value, "\\", "/"))
	if filename == "" || filename == "." || filename == ".." || filename == "/" {
		return "", ErrInvalidFilename
	}
	if strings.IndexByte(filename, 0) >= 0 || len(filename) > 255 {
		return "", ErrInvalidFilename
	}
	for _, r := range filename {
		if r < 0x20 || r == 0x7f {
			return "", ErrInvalidFilename
		}
	}
	return filename, nil
}

// Put streams an upload into the spool, enforcing both the per-upload length
// bound and the global capacity reservation.
func (s *Spool) Put(ctx context.Context, upload Upload) (*Artifact, error) {
	if s.closed.Load() {
		return nil, ErrUnavailable
	}
	if upload.MaximumLength <= 0 {
		return nil, ErrZeroLimit
	}
	filename, err := SafeFilename(upload.Filename)
	if err != nil {
		return nil, err
	}
	token := string(NewHandle())
	target := filepath.Join(s.root, token)
	pending := &pendingWrite{spool: s, path: target}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, ErrUnavailable
	}
	var written int64
	hash := sha256.New()
	buffer := make([]byte, ReadChunkBytes)
	for {
		if err = ctx.Err(); err != nil {
			file.Close()
			pending.abort(err)
			return nil, ErrUnavailable
		}
		n, readErr := upload.Body.Read(buffer)
		if n > 0 {
			chunk := buffer[:n]
			next := written + int64(n)
			if next < written || next > upload.MaximumLength {
				file.Close()
				pending.abort(nil)
				return nil, &TooLargeError{Maximum: upload.MaximumLength}
			}
			if !pending.reserve(int64(n)) {
				file.Close()
				pending.abort(nil)
				return nil, ErrUnavailable
			}
			var nwrite int
			nwrite, err = file.Write(chunk)
			if err == nil && nwrite != len(chunk) {
				err = io.ErrShortWrite
			}
			if err != nil {
				file.Close()
				pending.abort(err)
				return nil, ErrUnavailable
			}
			_, _ = hash.Write(chunk)
			written = next
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			file.Close()
			pending.abort(readErr)
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, readErr)
		}
	}
	if err = file.Close(); err != nil {
		pending.abort(err)
		return nil, ErrUnavailable
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	s.mu.Lock()
	s.entries[token] = spoolEntry{
		path:          target,
		filename:      filename,
		digest:        digest,
		contentType:   upload.ContentType,
		contentLength: written,
	}
	s.mu.Unlock()
	pending.commit()
	return &Artifact{
		Handle:        Handle(token),
		Digest:        digest,
		ContentType:   upload.ContentType,
		ContentLength: written,
	}, nil
}

// pendingWrite owns an in-flight reservation until the artifact commits; the
// partial file is removed before the reserved bytes are released.
type pendingWrite struct {
	spool     *Spool
	path      string
	reserved  int64
	committed bool
}

func (p *pendingWrite) reserve(bytes int64) bool {
	if !p.spool.tryReserve(bytes) {
		return false
	}
	p.reserved += bytes
	return true
}

func (p *pendingWrite) commit() { p.committed = true }

func (p *pendingWrite) abort(cause error) {
	if p.committed {
		return
	}
	err := os.Remove(p.path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		p.spool.release(p.reserved)
		return
	}
	if cause != nil {
		p.spool.log.Warn("failed to remove an incomplete media spool write; scheduled for retry",
			"error", err, "cause", cause)
	}
	p.spool.enqueueCleanup(&cleanupJob{path: p.path, bytes: p.reserved, committedEntry: false})
}

// Open returns the artifact metadata and its file for reading.
func (s *Spool) Open(handle Handle) (*Opened, error) {
	if err := ValidateHandle(string(handle)); err != nil {
		return nil, err
	}
	s.mu.Lock()
	entry, ok := s.entries[string(handle)]
	s.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	file, err := os.Open(entry.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.mu.Lock()
			delete(s.entries, string(handle))
			s.mu.Unlock()
			s.release(entry.contentLength)
			return nil, ErrNotFound
		}
		return nil, ErrUnavailable
	}
	return &Opened{
		Artifact: Artifact{
			Handle:        handle,
			Digest:        entry.digest,
			ContentType:   entry.contentType,
			ContentLength: entry.contentLength,
		},
		Filename: entry.filename,
		File:     file,
	}, nil
}

// Remove deletes an artifact and releases its bytes once the file is gone.
// Removal continues in the janitor when the first unlink fails so a request
// returning early cannot strand reserved capacity.
func (s *Spool) Remove(handle Handle) error {
	if err := ValidateHandle(string(handle)); err != nil {
		return err
	}
	s.mu.Lock()
	entry, ok := s.entries[string(handle)]
	if ok {
		delete(s.entries, string(handle))
	}
	s.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	err := os.Remove(entry.path)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		s.release(entry.contentLength)
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return nil
	}
	s.log.Warn("failed to remove a media spool artifact; scheduled for retry", "error", err)
	s.enqueueCleanup(&cleanupJob{
		path:           entry.path,
		bytes:          entry.contentLength,
		committedEntry: true,
		handle:         string(handle),
		entry:          entry,
	})
	return ErrUnavailable
}

// cleanupJob retries filesystem removal until the bytes can be released.
// For committed entries the job owns the entry bookkeeping: if the job is
// abandoned the entry is reinserted so a later remove can retry honestly.
type cleanupJob struct {
	path           string
	bytes          int64
	committedEntry bool
	handle         string
	entry          spoolEntry
}

func (s *Spool) enqueueCleanup(job *cleanupJob) {
	go func() {
		s.janitorSem <- struct{}{}
		defer func() { <-s.janitorSem }()
		backoff := cleanupInitialBackoff
		var attempts int64
		for {
			err := os.Remove(job.path)
			if err == nil || errors.Is(err, os.ErrNotExist) {
				s.release(job.bytes)
				return
			}
			attempts++
			if attempts == 1 || attempts%60 == 0 {
				s.log.Warn("media spool janitor could not remove an artifact",
					"error", err, "attempts", attempts)
			}
			time.Sleep(backoff)
			backoff *= 2
			if backoff > cleanupMaxBackoff {
				backoff = cleanupMaxBackoff
			}
		}
	}()
}

// Close removes this process's spool directory tree.
func (s *Spool) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	defer s.owner.Close()
	return os.RemoveAll(s.root)
}

// PutBytes is a convenience wrapper for staging a fully buffered payload.
func (s *Spool) PutBytes(ctx context.Context, filename, contentType string, data []byte, maximum int64) (*Artifact, error) {
	return s.Put(ctx, Upload{
		Filename:      filename,
		ContentType:   contentType,
		MaximumLength: maximum,
		Body:          strings.NewReader(string(data)),
	})
}

// constantTimeHandleEqual compares handles without leaking length-prefix
// timing; primarily used by tests asserting handle hygiene.
func constantTimeHandleEqual(a, b Handle) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
