package staging

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/oif"
)

type Part struct {
	Handle      Handle
	Digest      string
	Filename    string
	ContentType string
	Size        int64
	Limit       int64
}

func (p Part) BlobReference() (oif.BlobReference, error) {
	return Artifact{Handle: p.Handle, Digest: p.Digest, ContentType: p.ContentType, ContentLength: p.Size}.BlobReference()
}

type Value interface{ value() }

type Text string

func (Text) value() {}
func (Part) value() {}

type Field struct {
	Name  string
	Value Value
}

type Form struct {
	text       map[string][]string
	files      map[string][]Part
	source     []Field
	normalized bool
	spool      *Spool
	handles    []Handle
	armed      bool
	lease      *Lease
	err        error
}

func newForm(spool *Spool, lease *Lease) *Form {
	return &Form{text: map[string][]string{}, files: map[string][]Part{}, spool: spool, armed: true, lease: lease}
}

func (f *Form) SourceFields() ([]Field, bool) {
	return slices.Clone(f.source), f.normalized
}

func (f *Form) Handles() []Handle { return f.handles }

func (f *Form) Disarm() {
	f.armed = false
	f.releaseLease()
}

func (f *Form) Cleanup() {
	if !f.armed {
		f.releaseLease()
		return
	}
	for len(f.handles) > 0 {
		handle := f.handles[len(f.handles)-1]
		if err := f.spool.Remove(handle); err != nil && !errors.Is(err, ErrNotFound) {
			f.detach()
			return
		}
		f.handles = f.handles[:len(f.handles)-1]
	}
	f.armed = false
	f.releaseLease()
}

func (f *Form) releaseLease() {
	f.lease.Release()
	f.lease = nil
}

func (f *Form) detach() {
	spool, handles, lease := f.spool, f.handles, f.lease
	f.handles, f.lease, f.armed = nil, nil, false
	go func() {
		for _, handle := range handles {
			if err := spool.Remove(handle); err != nil && !errors.Is(err, ErrNotFound) {
				spool.log.Warn("staged upload cleanup failed", "error", err)
			}
		}
		lease.Release()
	}()
}

func (f *Form) Err() error { return f.err }

func (f *Form) fail(message string) {
	if f.err == nil {
		f.err = errors.New(message)
	}
}

func (f *Form) Required(name string) string {
	value := f.Optional(name)
	if value == nil {
		f.fail("The " + name + " field is required.")
		return ""
	}
	return *value
}

func (f *Form) Optional(name string) *string {
	values, ok := f.text[name]
	if !ok {
		return nil
	}
	delete(f.text, name)
	if len(values) != 1 {
		f.fail("The " + name + " field must appear at most once.")
		return nil
	}
	return &values[0]
}

func OptionalParse[T any](f *Form, name string, parse func(string) (T, error)) *T {
	value := f.Optional(name)
	if value == nil {
		return nil
	}
	parsed, err := parse(*value)
	if err != nil {
		f.fail("The " + name + " field is invalid.")
		return nil
	}
	return &parsed
}

func (f *Form) OptionalInt(name string) *int {
	return OptionalParse(f, name, strconv.Atoi)
}

func (f *Form) OptionalFloat(name string) *float64 {
	return OptionalParse(f, name, func(value string) (float64, error) { return strconv.ParseFloat(value, 64) })
}

func (f *Form) OptionalBool(name string) *bool {
	return OptionalParse(f, name, strconv.ParseBool)
}

func (f *Form) TakeRepeated(name string) []string {
	arrayName := name + "[]"
	for field := range f.text {
		if strings.HasPrefix(field, name+"[") && field != arrayName {
			f.fail("The " + field + " field is invalid; use " + name + " or " + arrayName + ".")
			return nil
		}
	}
	bare, hasBare := f.text[name]
	array, hasArray := f.text[arrayName]
	delete(f.text, name)
	delete(f.text, arrayName)
	switch {
	case hasBare && hasArray:
		f.fail("The " + name + " and " + arrayName + " fields cannot be mixed.")
		return nil
	case hasBare:
		return bare
	default:
		return array
	}
}

func (f *Form) TakeSingleFile(name string) *Part {
	values, ok := f.files[name]
	if !ok {
		return nil
	}
	delete(f.files, name)
	if len(values) != 1 {
		f.fail("The " + name + " file must appear at most once.")
		return nil
	}
	return &values[0]
}

func (f *Form) TakeFilesWithPrefix(prefix string) []Part {
	indexedPrefix := prefix + "["
	for name := range f.text {
		if name == prefix || strings.HasPrefix(name, indexedPrefix) {
			f.fail("The " + name + " field must be a file.")
			return nil
		}
	}
	type ordered struct {
		order [2]int
		name  string
	}
	var keys []ordered
	for name := range f.files {
		if name != prefix && !strings.HasPrefix(name, indexedPrefix) {
			continue
		}
		order, ok := fileArrayOrder(name, prefix)
		if !ok {
			f.fail("The " + name + " file field is invalid; use " + prefix + ", " + prefix +
				"[], or " + prefix + "[N] with a nonnegative integer index and no leading zeros.")
			return nil
		}
		keys = append(keys, ordered{order: order, name: name})
	}
	slices.SortStableFunc(keys, func(a, b ordered) int { return slices.Compare(a.order[:], b.order[:]) })
	var parts []Part
	for _, key := range keys {
		parts = append(parts, f.files[key.name]...)
		delete(f.files, key.name)
	}
	return parts
}

func fileArrayOrder(name, prefix string) ([2]int, bool) {
	if name == prefix {
		return [2]int{0, 0}, true
	}
	inner, opened := strings.CutPrefix(strings.TrimPrefix(name, prefix), "[")
	index, closed := strings.CutSuffix(inner, "]")
	if !opened || !closed {
		return [2]int{}, false
	}
	if index == "" {
		return [2]int{2, 0}, true
	}
	if strings.Trim(index, "0123456789") != "" || index != "0" && strings.HasPrefix(index, "0") {
		return [2]int{}, false
	}
	value, err := strconv.Atoi(index)
	return [2]int{1, value}, err == nil
}

func (f *Form) TakeExtensions() map[string]string {
	if len(f.files) > 0 {
		f.fail("The multipart request contains an unsupported file field.")
		return nil
	}
	extra := make(map[string]string, len(f.text))
	for name, values := range f.text {
		if len(values) != 1 {
			f.fail("The unsupported " + name + " field cannot be repeated.")
			return nil
		}
		extra[name] = values[0]
	}
	f.text = map[string][]string{}
	return extra
}
