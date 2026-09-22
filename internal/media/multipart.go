package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/tyk-swe/olp/internal/oif"
)

const (
	// MultipartTotalDeadline bounds the entire parser lifetime so a peer
	// trickling valid frames cannot occupy an admission lease indefinitely.
	MultipartTotalDeadline = 5 * time.Minute
	// MaxFields bounds the number of multipart parts per request.
	MaxFields = 128
	// MaxTextFieldBytes bounds one text field.
	MaxTextFieldBytes = 64 * 1024
	// MaxTextTotalBytes bounds all text fields together.
	MaxTextTotalBytes = 512 * 1024
)

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Error is a request-visible media failure with an HTTP status and a
// stable machine-readable code.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Fail builds a media request error.
func Fail(status int, code, message string) *Error {
	return &Error{Status: status, Code: code, Message: message}
}

func invalidRequest(message string) *Error {
	return Fail(http.StatusBadRequest, "invalid_request", message)
}

func payloadTooLarge(code string) *Error {
	return Fail(http.StatusRequestEntityTooLarge, code, "The media body exceeds the configured limit.")
}

// SpoolError maps a spool failure to a request-visible error.
func SpoolError(err error) *Error {
	var bodyLimit *http.MaxBytesError
	switch {
	case TooLarge(err), errors.As(err, &bodyLimit):
		return payloadTooLarge("media_too_large")
	case errors.Is(err, ErrInvalidFilename), errors.Is(err, ErrInvalidHandle), errors.Is(err, ErrZeroLimit):
		return invalidRequest(err.Error())
	default:
		return Fail(http.StatusServiceUnavailable, "media_spool_unavailable", "The media staging area is unavailable.")
	}
}

// RouteAdmissionKind controls how the parser authorizes the model field.
type RouteAdmissionKind int

const (
	// RouteUnrestricted imposes no model-field check.
	RouteUnrestricted RouteAdmissionKind = iota
	// RouteRequireAuthorizedModel requires the model field to name a route
	// the API key is authorized for.
	RouteRequireAuthorizedModel
	// RouteExpected requires the model field to equal the X-OLP-Route header.
	RouteExpected
)

// RouteAdmission is the route policy enforced while parsing.
type RouteAdmission struct {
	Kind     RouteAdmissionKind
	Allowed  []string
	Expected string
}

func (a RouteAdmission) requiresAuthorizedModel() bool {
	return a.Kind == RouteRequireAuthorizedModel
}

// AdmissionState bounds the worst-case upload bytes promised to untrusted
// parsers and serializes concurrent multipart parses per API key.
type AdmissionState struct {
	budget   int64
	reserved atomic.Int64

	mu     sync.Mutex
	active map[string]struct{}
}

// NewAdmissionState creates admission control over half the spool capacity.
// The spool itself continues to enforce byte-accurate accounting for all
// request and response media.
func NewAdmissionState(capacity int64) *AdmissionState {
	return &AdmissionState{budget: capacity / 2, active: map[string]struct{}{}}
}

// ParserLease owns a fixed admission reservation until Release.
type ParserLease struct {
	state       *AdmissionState
	keyID       string
	reservation int64
	released    atomic.Bool
}

// TryAdmit reserves reservationBytes for one parser. A key may hold only one
// parser lease at a time so a client cannot fan out staged uploads.
func (a *AdmissionState) TryAdmit(keyID string, reservationBytes int64) *ParserLease {
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
		next := current + reservationBytes
		if next > a.budget {
			a.mu.Lock()
			delete(a.active, keyID)
			a.mu.Unlock()
			return nil
		}
		if a.reserved.CompareAndSwap(current, next) {
			break
		}
	}
	return &ParserLease{state: a, keyID: keyID, reservation: reservationBytes}
}

// Release returns the reservation exactly once.
func (l *ParserLease) Release() {
	if l == nil || l.released.Swap(true) {
		return
	}
	l.state.reserved.Add(-l.reservation)
	l.state.mu.Lock()
	delete(l.state.active, l.keyID)
	l.state.mu.Unlock()
}

// Admission couples the parser lease with the route policy.
type Admission struct {
	Route RouteAdmission
	Lease *ParserLease
}

// Release frees the parser lease if still held.
func (a *Admission) Release() {
	if a != nil && a.Lease != nil {
		a.Lease.Release()
	}
}

// Part is one staged file field.
type Part struct {
	Handle      Handle
	Digest      string // SHA-256 of the exact staged file bytes
	Filename    string
	ContentType string
	Size        int64
	Limit       int64
}

// BlobReference identifies this already-authorized staged part, never a URL.
// ContentType remains the caller's value, including omission.
func (p Part) BlobReference() (oif.BlobReference, error) {
	return (Artifact{Handle: p.Handle, Digest: p.Digest, ContentType: p.ContentType, ContentLength: p.Size}).BlobReference()
}

// Form holds a parsed multipart body. Staged files stay reserved in the
// spool until Disarm hands ownership to request execution or Cleanup
// removes them.
type Form struct {
	text    map[string][]string
	files   map[string][]Part
	spool   *Spool
	handles []Handle
	armed   bool
	lease   *ParserLease
}

func newForm(spool *Spool, lease *ParserLease) *Form {
	return &Form{
		text:  map[string][]string{},
		files: map[string][]Part{},
		spool: spool,
		armed: true,
		lease: lease,
	}
}

// Disarm transfers staged-handle ownership to request execution; the parser
// reservation is released since it no longer needs to cover cleanup.
func (f *Form) Disarm() {
	f.armed = false
	if f.lease != nil {
		f.lease.Release()
		f.lease = nil
	}
}

// Handles reports the staged artifact handles still owned by the form.
func (f *Form) Handles() []Handle { return f.handles }

// Cleanup removes every staged artifact still owned by the form and frees
// the admission lease. It is safe to call once after parse failure or when a
// request is abandoned before Disarm.
func (f *Form) Cleanup() {
	if !f.armed {
		if f.lease != nil {
			f.lease.Release()
			f.lease = nil
		}
		return
	}
	for len(f.handles) > 0 {
		handle := f.handles[len(f.handles)-1]
		if err := f.spool.Remove(handle); err != nil && !errors.Is(err, ErrNotFound) {
			// Leave the remaining handles armed: the janitor already owns
			// this artifact, and a detached cleanup pass retries the rest so
			// the lease cannot outlive its staged files.
			f.detach()
			return
		}
		f.handles = f.handles[:len(f.handles)-1]
	}
	f.armed = false
	if f.lease != nil {
		f.lease.Release()
		f.lease = nil
	}
}

// detach schedules a best-effort deletion for staged artifacts whose removal
// was interrupted, keeping the lease attached until the task completes.
func (f *Form) detach() {
	spool := f.spool
	handles := f.handles
	lease := f.lease
	f.handles = nil
	f.lease = nil
	f.armed = false
	go func() {
		for _, handle := range handles {
			_ = spool.Remove(handle)
		}
		if lease != nil {
			lease.Release()
		}
	}()
}

// Required returns the single text value for name or an error.
func (f *Form) Required(name string) (string, *Error) {
	value, err := f.Optional(name)
	if err != nil {
		return "", err
	}
	if value == nil {
		return "", invalidRequest("The " + name + " field is required.")
	}
	return *value, nil
}

// Optional returns the single text value for name, or nil.
func (f *Form) Optional(name string) (*string, *Error) {
	values, ok := f.text[name]
	if !ok {
		return nil, nil
	}
	delete(f.text, name)
	if len(values) != 1 {
		return nil, invalidRequest("The " + name + " field must appear at most once.")
	}
	return &values[0], nil
}

// OptionalParse applies parse to the optional field value.
func OptionalParse[T any](f *Form, name string, parse func(string) (T, error)) (*T, *Error) {
	value, err := f.Optional(name)
	if err != nil || value == nil {
		return nil, err
	}
	parsed, perr := parse(*value)
	if perr != nil {
		return nil, invalidRequest("The " + name + " field is invalid.")
	}
	return &parsed, nil
}

// OptionalInt parses an optional integer field.
func (f *Form) OptionalInt(name string) (*int, *Error) {
	return OptionalParse(f, name, func(value string) (int, error) {
		return strconv.Atoi(value)
	})
}

// OptionalFloat parses an optional floating-point field.
func (f *Form) OptionalFloat(name string) (*float64, *Error) {
	return OptionalParse(f, name, func(value string) (float64, error) {
		return strconv.ParseFloat(value, 64)
	})
}

// OptionalBool parses an optional boolean field.
func (f *Form) OptionalBool(name string) (*bool, *Error) {
	return OptionalParse(f, name, func(value string) (bool, error) {
		return strconv.ParseBool(value)
	})
}

// TakeRepeated collects repeated text fields using either the bare name or
// the name[] array convention; indexed name[N] fields are rejected.
func (f *Form) TakeRepeated(name string) ([]string, *Error) {
	arrayName := name + "[]"
	indexedPrefix := name + "["
	for field := range f.text {
		if strings.HasPrefix(field, indexedPrefix) && field != arrayName {
			return nil, invalidRequest("The " + field + " field is invalid; use " + name + " or " + arrayName + ".")
		}
	}
	bare, hasBare := f.text[name]
	array, hasArray := f.text[arrayName]
	delete(f.text, name)
	delete(f.text, arrayName)
	if hasBare && hasArray {
		return nil, invalidRequest("The " + name + " and " + arrayName + " fields cannot be mixed.")
	}
	if hasBare {
		return bare, nil
	}
	if hasArray {
		return array, nil
	}
	return nil, nil
}

// TakeSingleFile returns the file part for name, requiring it to appear at
// most once.
func (f *Form) TakeSingleFile(name string) (*Part, *Error) {
	values, ok := f.files[name]
	if !ok {
		return nil, nil
	}
	delete(f.files, name)
	if len(values) != 1 {
		return nil, invalidRequest("The " + name + " file must appear at most once.")
	}
	return &values[0], nil
}

// TakeFilesWithPrefix collects the bare, indexed prefix[N], and prefix[]
// file fields in array order, rejecting non-integer or leading-zero indexes.
func (f *Form) TakeFilesWithPrefix(prefix string) ([]Part, *Error) {
	indexedPrefix := prefix + "["
	for name := range f.text {
		if name == prefix || strings.HasPrefix(name, indexedPrefix) {
			return nil, invalidRequest("The " + name + " field must be a file.")
		}
	}
	var keys []orderedPart
	for name := range f.files {
		if name != prefix && !strings.HasPrefix(name, indexedPrefix) {
			continue
		}
		order, err := fileArrayOrder(name, prefix)
		if err != nil {
			return nil, err
		}
		keys = append(keys, orderedPart{order: order, name: name})
	}
	slices.SortStableFunc(keys, func(a, b orderedPart) int {
		return slices.Compare(a.order[:], b.order[:])
	})
	var parts []Part
	for _, key := range keys {
		parts = append(parts, f.files[key.name]...)
		delete(f.files, key.name)
	}
	return parts, nil
}

type orderedPart struct {
	order [2]int
	name  string
}

func fileArrayOrder(name, prefix string) ([2]int, *Error) {
	if name == prefix {
		return [2]int{0, 0}, nil
	}
	suffix, ok := strings.CutPrefix(name, prefix)
	if !ok || !strings.HasPrefix(suffix, "[") || !strings.HasSuffix(suffix, "]") {
		return invalidFileField(name, prefix)
	}
	index := suffix[1 : len(suffix)-1]
	if index == "" {
		return [2]int{2, 0}, nil
	}
	for i := 0; i < len(index); i++ {
		if index[i] < '0' || index[i] > '9' {
			return invalidFileField(name, prefix)
		}
	}
	if index != "0" && strings.HasPrefix(index, "0") {
		return invalidFileField(name, prefix)
	}
	value, err := strconv.Atoi(index)
	if err != nil {
		return invalidFileField(name, prefix)
	}
	return [2]int{1, value}, nil
}

func invalidFileField(name, prefix string) ([2]int, *Error) {
	return [2]int{}, invalidRequest("The " + name + " file field is invalid; use " + prefix + ", " +
		prefix + "[], or " + prefix + "[N] with a nonnegative integer index and no leading zeros.")
}

// TakeExtensions consumes all remaining fields as unstructured extras.
// Unsupported file fields are rejected rather than silently dropped.
func (f *Form) TakeExtensions() (map[string]string, *Error) {
	if len(f.files) > 0 {
		return nil, invalidRequest("The multipart request contains an unsupported file field.")
	}
	extra := make(map[string]string, len(f.text))
	for name, values := range f.text {
		if len(values) != 1 {
			return nil, invalidRequest("The unsupported " + name + " field cannot be repeated.")
		}
		extra[name] = values[0]
	}
	f.text = map[string][]string{}
	return extra, nil
}

// ValidateBoundary enforces the multipart boundary policy before any body
// bytes are read.
func ValidateBoundary(contentType string) *Error {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		return invalidRequest("A multipart/form-data request requires a valid boundary no longer than 200 bytes.")
	}
	boundary := params["boundary"]
	if boundary == "" || len(boundary) > 200 {
		return invalidRequest("A multipart/form-data request requires a valid boundary no longer than 200 bytes.")
	}
	for i := 0; i < len(boundary); i++ {
		c := boundary[i]
		if c < 0x21 || c > 0x7e || c == '"' || c == '\\' {
			return invalidRequest("A multipart/form-data request requires a valid boundary no longer than 200 bytes.")
		}
	}
	return nil
}

// ParseMultipart streams a bounded multipart body into text fields and
// spooled file parts. The caller must have set a read deadline of at most
// MultipartTotalDeadline before invoking ParseMultipart; staged artifacts are
// cleaned up on every failure path.
func ParseMultipart(ctx context.Context, r *http.Request, spool *Spool, admission *Admission, maximumFileBytes int64, maximumFiles int) (*Form, error) {
	if admission == nil || admission.Lease == nil {
		return nil, Fail(http.StatusServiceUnavailable, "media_admission_overloaded", "The media upload capacity is busy; retry shortly.")
	}
	if failure := ValidateBoundary(r.Header.Get("Content-Type")); failure != nil {
		admission.Release()
		return nil, failure
	}
	_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	output := newForm(spool, admission.Lease)
	err := parseMultipartFields(ctx, r, spool, multipart.NewReader(r.Body, params["boundary"]),
		maximumFileBytes, maximumFiles, admission, output)
	if err == nil {
		// Multipart ends at its closing boundary; the raw-body cap must also
		// cover any epilogue, including bytes buffered by the multipart reader.
		if _, drainErr := io.Copy(io.Discard, r.Body); drainErr != nil {
			err = multipartReadError(drainErr)
		}
	}
	if err != nil {
		output.Cleanup()
		return nil, err
	}
	return output, nil
}

func multipartReadError(err error) *Error {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return payloadTooLarge("media_too_large")
	}
	return invalidRequest(fmt.Sprintf("The multipart request is invalid: %v", err))
}

func parseMultipartFields(ctx context.Context, r *http.Request, spool *Spool, reader *multipart.Reader,
	maximumFileBytes int64, maximumFiles int, admission *Admission, output *Form) error {
	var fieldCount, fileCount, textBytes int
	var authorizedModelSeen bool
	for {
		if err := ctx.Err(); err != nil {
			return Fail(http.StatusRequestTimeout, "multipart_parser_timeout", "The multipart parser timed out.")
		}
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return multipartReadError(err)
		}
		fieldCount++
		if fieldCount > MaxFields {
			part.Close()
			return invalidRequest("The multipart request contains too many fields.")
		}
		name := part.FormName()
		if name == "" {
			part.Close()
			return invalidRequest("A multipart field has no name.")
		}
		if rawFilename, isFile := partFilename(part); isFile {
			fileCount++
			if fileCount > maximumFiles {
				part.Close()
				return invalidRequest("The multipart request contains too many files.")
			}
			if err := storeMultipartFile(ctx, spool, part, rawFilename, name, maximumFileBytes, output); err != nil {
				part.Close()
				return err
			}
		} else {
			if err := storeMultipartText(part, name, admission, output, &textBytes, &authorizedModelSeen); err != nil {
				part.Close()
				return err
			}
		}
		part.Close()
	}
	if admission != nil && admission.Route.requiresAuthorizedModel() && !authorizedModelSeen {
		return invalidRequest("A route-restricted multipart request must include a model field naming an authorized route.")
	}
	return nil
}

// partFilename reports the raw filename parameter when the part is a file
// upload, keeping the file/text decision independent of basename sanitation.
func partFilename(part *multipart.Part) (string, bool) {
	disposition := part.Header.Get("Content-Disposition")
	if disposition == "" {
		return "", false
	}
	_, params, err := mime.ParseMediaType(disposition)
	if err != nil {
		return "", false
	}
	filename, ok := params["filename"]
	return filename, ok
}

func storeMultipartFile(ctx context.Context, spool *Spool, part *multipart.Part, rawFilename, name string,
	maximumFileBytes int64, output *Form) error {
	filename, err := SafeFilename(rawFilename)
	if err != nil {
		return invalidRequest("The multipart filename is invalid.")
	}
	contentType := part.Header.Get("Content-Type")
	artifact, err := spool.Put(ctx, Upload{
		Filename:      filename,
		ContentType:   contentType,
		MaximumLength: maximumFileBytes,
		Body:          part,
	})
	if err != nil {
		return SpoolError(err)
	}
	output.handles = append(output.handles, artifact.Handle)
	output.files[name] = append(output.files[name], Part{
		Handle:      artifact.Handle,
		Digest:      artifact.Digest,
		Filename:    filename,
		ContentType: contentType,
		Size:        artifact.ContentLength,
		Limit:       maximumFileBytes,
	})
	return nil
}

func storeMultipartText(part *multipart.Part, name string, admission *Admission, output *Form,
	textBytes *int, authorizedModelSeen *bool) error {
	var field bytes.Buffer
	buffer := make([]byte, ReadChunkBytes)
	for {
		n, err := part.Read(buffer)
		if n > 0 {
			if field.Len()+n > MaxTextFieldBytes {
				return invalidRequest("A multipart text field exceeded 64 KiB.")
			}
			*textBytes += n
			if *textBytes > MaxTextTotalBytes {
				return invalidRequest("Multipart text fields exceeded the 512 KiB aggregate limit.")
			}
			field.Write(buffer[:n])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return multipartReadError(err)
		}
	}
	raw := bytes.TrimPrefix(field.Bytes(), utf8BOM)
	if !utf8.Valid(raw) {
		return invalidRequest("The multipart field " + name + " is not valid UTF-8.")
	}
	text := string(raw)
	if name == "model" && admission != nil {
		switch admission.Route.Kind {
		case RouteExpected:
			if text != admission.Route.Expected {
				return invalidRequest("X-OLP-Route must match the multipart model field.")
			}
			*authorizedModelSeen = true
		case RouteRequireAuthorizedModel:
			allowed := slices.Contains(admission.Route.Allowed, text)
			if !allowed {
				return Fail(http.StatusForbidden, "forbidden",
					"The API key is not authorized for the multipart model route.")
			}
			*authorizedModelSeen = true
		default:
			*authorizedModelSeen = true
		}
	}
	output.text[name] = append(output.text[name], text)
	return nil
}
