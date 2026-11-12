package staging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

const (
	maxFields         = 128
	maxTextFieldBytes = 64 * 1024
	maxTextTotalBytes = 512 * 1024
)

type Kind int

const (
	Invalid Kind = iota
	TooLarge
	Overloaded
	Timeout
	Unavailable
)

type Error struct {
	Kind    Kind
	Message string
}

func (e *Error) Error() string { return e.Message }

func invalid(message string) *Error { return &Error{Kind: Invalid, Message: message} }

var (
	errTooLarge   = &Error{Kind: TooLarge, Message: "The media body exceeds the configured limit."}
	errOverloaded = &Error{Kind: Overloaded, Message: "The media upload capacity is busy; retry shortly."}
	errTimeout    = &Error{Kind: Timeout, Message: "The multipart parser timed out."}
	errBoundary   = invalid("A multipart/form-data request requires a valid boundary no longer than 200 bytes.")
)

func spoolFailure(err error) *Error {
	_, tooLarge := errors.AsType[*TooLargeError](err)
	_, bodyLimit := errors.AsType[*http.MaxBytesError](err)
	switch {
	case tooLarge, bodyLimit:
		return errTooLarge
	case errors.Is(err, ErrInvalidFilename), errors.Is(err, ErrInvalidHandle), errors.Is(err, ErrZeroLimit):
		return invalid(err.Error())
	default:
		return &Error{Kind: Unavailable, Message: "The media staging area is unavailable."}
	}
}

type Admission struct {
	budget   int64
	reserved atomic.Int64

	mu     sync.Mutex
	active map[string]struct{}
}

func NewAdmission(spoolCapacity int64) *Admission {
	return &Admission{budget: spoolCapacity / 2, active: map[string]struct{}{}}
}

type Lease struct {
	admission   *Admission
	keyID       string
	reservation int64
	released    atomic.Bool
}

func (a *Admission) TryAdmit(keyID string, reservationBytes int64) *Lease {
	if reservationBytes <= 0 || reservationBytes > a.budget {
		return nil
	}
	a.mu.Lock()
	if _, busy := a.active[keyID]; busy {
		a.mu.Unlock()
		return nil
	}
	a.active[keyID] = struct{}{}
	a.mu.Unlock()
	for {
		current := a.reserved.Load()
		if current+reservationBytes > a.budget {
			a.mu.Lock()
			delete(a.active, keyID)
			a.mu.Unlock()
			return nil
		}
		if a.reserved.CompareAndSwap(current, current+reservationBytes) {
			return &Lease{admission: a, keyID: keyID, reservation: reservationBytes}
		}
	}
}

func (l *Lease) Release() {
	if l == nil || l.released.Swap(true) {
		return
	}
	l.admission.reserved.Add(-l.reservation)
	l.admission.mu.Lock()
	delete(l.admission.active, l.keyID)
	l.admission.mu.Unlock()
}

func ParseMultipart(ctx context.Context, r *http.Request, spool *Spool, lease *Lease, maximumFileBytes int64, maximumFiles int) (*Form, *Error) {
	if lease == nil {
		return nil, errOverloaded
	}
	boundary, ok := multipartBoundary(r.Header.Get("Content-Type"))
	if !ok {
		lease.Release()
		return nil, errBoundary
	}
	output := newForm(spool, lease)
	if err := parseFields(ctx, spool, multipart.NewReader(r.Body, boundary), maximumFileBytes, maximumFiles, output); err != nil {
		output.Cleanup()
		return nil, err
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		output.Cleanup()
		return nil, readFailure(err)
	}
	return output, nil
}

func multipartBoundary(contentType string) (string, bool) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return "", false
	}
	boundary := params["boundary"]
	if boundary == "" || len(boundary) > 200 || strings.HasSuffix(boundary, " ") {
		return "", false
	}
	for i := range len(boundary) {
		if c := boundary[i]; c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			return "", false
		}
	}
	return boundary, true
}

func readFailure(err error) *Error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return errTooLarge
	}
	return invalid(fmt.Sprintf("The multipart request is invalid: %v", err))
}

func parseFields(ctx context.Context, spool *Spool, reader *multipart.Reader, maximumFileBytes int64, maximumFiles int, output *Form) *Error {
	var fieldCount, fileCount, textBytes int
	for {
		if ctx.Err() != nil {
			return errTimeout
		}
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return readFailure(err)
		}
		fieldCount++
		failure := storePart(ctx, spool, part, fieldCount, &fileCount, &textBytes, maximumFileBytes, maximumFiles, output)
		part.Close()
		if failure != nil {
			return failure
		}
	}
}

func storePart(ctx context.Context, spool *Spool, part *multipart.Part, fieldCount int, fileCount, textBytes *int,
	maximumFileBytes int64, maximumFiles int, output *Form) *Error {
	if fieldCount > maxFields {
		return invalid("The multipart request contains too many fields.")
	}
	name := part.FormName()
	if name == "" {
		return invalid("A multipart field has no name.")
	}
	for header := range part.Header {
		if header != "Content-Disposition" && header != "Content-Type" {
			output.normalized = true
		}
	}
	rawFilename, isFile := partFilename(part)
	if !isFile {
		return storeText(part, name, output, textBytes)
	}
	*fileCount++
	if *fileCount > maximumFiles {
		return invalid("The multipart request contains too many files.")
	}
	return storeFile(ctx, spool, part, rawFilename, name, maximumFileBytes, output)
}

func partFilename(part *multipart.Part) (string, bool) {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return "", false
	}
	filename, ok := params["filename"]
	return filename, ok
}

func storeFile(ctx context.Context, spool *Spool, part *multipart.Part, rawFilename, name string, maximumFileBytes int64, output *Form) *Error {
	filename, err := SafeFilename(rawFilename)
	if err != nil {
		return invalid("The multipart filename is invalid.")
	}
	if filename != rawFilename {
		output.normalized = true
	}
	contentType := part.Header.Get("Content-Type")
	artifact, err := spool.Put(ctx, Upload{Filename: filename, ContentType: contentType, MaximumLength: maximumFileBytes, Body: part})
	if err != nil {
		return spoolFailure(err)
	}
	stored := Part{
		Handle:      artifact.Handle,
		Digest:      artifact.Digest,
		Filename:    filename,
		ContentType: contentType,
		Size:        artifact.ContentLength,
		Limit:       maximumFileBytes,
	}
	output.handles = append(output.handles, artifact.Handle)
	output.files[name] = append(output.files[name], stored)
	output.source = append(output.source, Field{Name: name, Value: stored})
	return nil
}

func storeText(part *multipart.Part, name string, output *Form, textBytes *int) *Error {
	var field bytes.Buffer
	buffer := make([]byte, readChunkBytes)
	for {
		n, err := part.Read(buffer)
		if field.Len()+n > maxTextFieldBytes {
			return invalid("A multipart text field exceeded 64 KiB.")
		}
		*textBytes += n
		if *textBytes > maxTextTotalBytes {
			return invalid("Multipart text fields exceeded the 512 KiB aggregate limit.")
		}
		field.Write(buffer[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return readFailure(err)
		}
	}
	raw, trimmed := bytes.CutPrefix(field.Bytes(), []byte{0xEF, 0xBB, 0xBF})
	if trimmed || part.Header.Get("Content-Type") != "" {
		output.normalized = true
	}
	if !utf8.Valid(raw) {
		return invalid("The multipart field " + name + " is not valid UTF-8.")
	}
	text := string(raw)
	output.text[name] = append(output.text[name], text)
	output.source = append(output.source, Field{Name: name, Value: Text(text)})
	return nil
}
