package staging

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

	"github.com/tyk-swe/olp/internal/domain/oif"
)

const (
	readChunkBytes             = 64 * 1024
	DefaultCapacityBytes int64 = 1024 * 1024 * 1024
	MinCapacityBytes     int64 = 256 * 1024 * 1024
	handleBytes                = 32
	maxSpoolFiles              = 4096

	cleanupConcurrency    = 4
	cleanupMaxAttempts    = 5
	cleanupInitialBackoff = 25 * time.Millisecond
	cleanupMaxBackoff     = 2 * time.Second
)

var (
	ErrUnavailable     = errors.New("media spool unavailable")
	ErrNotFound        = errors.New("media handle not found")
	ErrInvalidHandle   = errors.New("invalid media handle")
	ErrInvalidFilename = errors.New("invalid media filename")
	ErrZeroLimit       = errors.New("media uploads require a length bound")
)

type TooLargeError struct{ Maximum int64 }

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("media body exceeds the configured limit of %d bytes", e.Maximum)
}

type Handle string

func newHandle() Handle {
	value := make([]byte, handleBytes/2)
	rand.Read(value)
	return Handle(hex.EncodeToString(value))
}

func validateHandle(value string) error {
	if len(value) != handleBytes {
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

type Artifact struct {
	Handle        Handle
	Digest        string
	ContentType   string
	ContentLength int64
}

func (a Artifact) BlobReference() (oif.BlobReference, error) {
	mediaType := a.ContentType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return oif.NewBlobReference(string(a.Handle), a.Digest, mediaType, a.ContentLength)
}

type Upload struct {
	Filename      string
	ContentType   string
	MaximumLength int64
	Body          io.Reader
}

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

type Spool struct {
	root     string
	owner    *os.File
	capacity int64
	log      *slog.Logger

	mu      sync.Mutex
	entries map[string]spoolEntry

	usedBytes    atomic.Int64
	fileSlots    chan struct{}
	janitorMu    sync.Mutex
	janitorReady *sync.Cond
	janitorQueue []cleanupJob
	janitorDone  chan struct{}
	janitors     sync.WaitGroup
	closed       atomic.Bool
}

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
	s := &Spool{
		root:        root,
		owner:       owner,
		capacity:    capacity,
		log:         log,
		entries:     map[string]spoolEntry{},
		fileSlots:   make(chan struct{}, maxSpoolFiles),
		janitorDone: make(chan struct{}),
	}
	s.janitorReady = sync.NewCond(&s.janitorMu)
	s.janitors.Add(cleanupConcurrency)
	for range cleanupConcurrency {
		go s.janitor()
	}
	return s, nil
}

func (s *Spool) CapacityBytes() int64 { return s.capacity }

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

func (s *Spool) releaseFile(bytes int64) {
	if bytes != 0 {
		s.usedBytes.Add(-bytes)
	}
	<-s.fileSlots
}

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
	select {
	case s.fileSlots <- struct{}{}:
	default:
		return nil, ErrUnavailable
	}
	token := string(newHandle())
	target := filepath.Join(s.root, token)
	pending := &pendingWrite{spool: s, path: target}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.releaseFile(0)
		return nil, ErrUnavailable
	}
	var written int64
	hash := sha256.New()
	buffer := make([]byte, readChunkBytes)
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
		p.spool.releaseFile(p.reserved)
		return
	}
	if cause != nil {
		p.spool.log.Warn("failed to remove an incomplete media spool write; scheduled for retry",
			"error", err, "cause", cause)
	}
	p.spool.enqueueCleanup(cleanupJob{path: p.path, bytes: p.reserved})
}

func (s *Spool) Open(handle Handle) (*Opened, error) {
	if err := validateHandle(string(handle)); err != nil {
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
			current, present := s.entries[string(handle)]
			owned := present && current == entry
			if owned {
				delete(s.entries, string(handle))
			}
			s.mu.Unlock()
			if owned {
				s.releaseFile(entry.contentLength)
			}
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

func (s *Spool) Remove(handle Handle) error {
	if err := validateHandle(string(handle)); err != nil {
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
		s.releaseFile(entry.contentLength)
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return nil
	}
	s.log.Warn("failed to remove a media spool artifact; scheduled for retry", "error", err)
	s.enqueueCleanup(cleanupJob{path: entry.path, bytes: entry.contentLength})
	return ErrUnavailable
}

type cleanupJob struct {
	path  string
	bytes int64
}

func (s *Spool) enqueueCleanup(job cleanupJob) {
	s.janitorMu.Lock()
	defer s.janitorMu.Unlock()
	if s.closed.Load() {
		s.releaseFile(job.bytes)
		return
	}
	s.janitorQueue = append(s.janitorQueue, job)
	s.janitorReady.Signal()
}

func (s *Spool) nextCleanup() (cleanupJob, bool) {
	s.janitorMu.Lock()
	defer s.janitorMu.Unlock()
	for len(s.janitorQueue) == 0 && !s.closed.Load() {
		s.janitorReady.Wait()
	}
	if s.closed.Load() {
		return cleanupJob{}, false
	}
	job := s.janitorQueue[0]
	s.janitorQueue[0] = cleanupJob{}
	s.janitorQueue = s.janitorQueue[1:]
	return job, true
}

func (s *Spool) janitor() {
	defer s.janitors.Done()
	for {
		job, ok := s.nextCleanup()
		if !ok {
			return
		}
		s.clean(job)
	}
}

func (s *Spool) clean(job cleanupJob) {
	defer s.releaseFile(job.bytes)
	backoff := cleanupInitialBackoff
	for attempts := int64(1); ; attempts++ {
		select {
		case <-s.janitorDone:
			return
		default:
		}
		err := os.Remove(job.path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return
		}
		if attempts == 1 || attempts%60 == 0 {
			s.log.Warn("media spool janitor could not remove an artifact",
				"error", err, "attempts", attempts)
		}
		if attempts >= cleanupMaxAttempts {
			return
		}
		select {
		case <-s.janitorDone:
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > cleanupMaxBackoff {
			backoff = cleanupMaxBackoff
		}
	}
}

func (s *Spool) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	close(s.janitorDone)
	s.janitorMu.Lock()
	pending := s.janitorQueue
	s.janitorQueue = nil
	s.janitorReady.Broadcast()
	s.janitorMu.Unlock()
	s.janitors.Wait()
	for _, job := range pending {
		s.releaseFile(job.bytes)
	}
	defer s.owner.Close()
	return os.RemoveAll(s.root)
}
