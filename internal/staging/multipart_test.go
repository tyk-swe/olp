package staging

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const uploadKey = "key"

type member struct {
	name     string
	filename string
	value    string
	headers  map[string]string
}

func text(name, value string) member { return member{name: name, value: value} }

func file(name, filename, value string) member {
	return member{name: name, filename: filename, value: value}
}

func newSpool(t *testing.T) *Spool {
	t.Helper()
	spool, err := NewSpool(t.TempDir(), MinCapacityBytes, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { spool.Close() })
	return spool
}

func upload(t *testing.T, members ...member) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, m := range members {
		disposition := `form-data; name="` + m.name + `"`
		if m.filename != "" {
			disposition += `; filename="` + m.filename + `"`
		}
		header := textproto.MIMEHeader{"Content-Disposition": {disposition}}
		for name, value := range m.headers {
			header.Set(name, value)
		}
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, m.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	return request
}

func admit(t *testing.T, admission *Admission) *Lease {
	t.Helper()
	lease := admission.TryAdmit(uploadKey, 1<<20)
	if lease == nil {
		t.Fatal("the upload was not admitted")
	}
	return lease
}

func TestParseMultipartStagesFilesAndKeepsCallerOrder(t *testing.T) {
	spool := newSpool(t)
	state := NewAdmission(MinCapacityBytes)
	const audio = "RIFF fixture audio"
	request := upload(t,
		text("model", "whisper"),
		member{name: "file", filename: "../../etc/clip.wav", value: audio, headers: map[string]string{"Content-Type": "audio/wav"}},
		text("language", "\ufeffen"),
	)

	form, failure := ParseMultipart(t.Context(), request, spool, admit(t, state), 1024, 1)
	if failure != nil {
		t.Fatal(failure)
	}
	fields, _ := form.SourceFields()
	names := make([]string, len(fields))
	for i, field := range fields {
		names[i] = field.Name
	}
	if want := []string{"model", "file", "language"}; !slices.Equal(names, want) {
		t.Fatalf("got fields %v, want %v", names, want)
	}
	if language := form.Required("language"); language != "en" {
		t.Fatalf("got language %q, want en", language)
	}
	part := form.TakeSingleFile("file")
	if form.Err() != nil || part == nil {
		t.Fatalf("got file %v, %v", part, form.Err())
	}
	digest := sha256.Sum256([]byte(audio))
	want := Part{
		Handle: part.Handle, Digest: hex.EncodeToString(digest[:]), Filename: "clip.wav",
		ContentType: "audio/wav", Size: int64(len(audio)), Limit: 1024,
	}
	if *part != want {
		t.Fatalf("got part %+v, want %+v", *part, want)
	}
	opened, err := spool.Open(part.Handle)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.File.Close()
	staged, err := io.ReadAll(opened.File)
	if err != nil {
		t.Fatal(err)
	}
	if string(staged) != audio || opened.Filename != "clip.wav" {
		t.Fatalf("got staged %q named %q", staged, opened.Filename)
	}
	if stored := (Artifact{Handle: want.Handle, Digest: want.Digest, ContentType: want.ContentType, ContentLength: want.Size}); opened.Artifact != stored {
		t.Fatalf("got stored artifact %+v, want %+v", opened.Artifact, stored)
	}
	if filepath.Dir(opened.File.Name()) != spool.root {
		t.Fatalf("the upload was staged at %s, outside the spool", opened.File.Name())
	}

	if state.TryAdmit(uploadKey, 1) != nil {
		t.Fatal("the key started a second upload while the first still owns its staged file")
	}
	form.Disarm()
	form.Cleanup()
	if spool.UsedBytes() != int64(len(audio)) {
		t.Fatalf("got %d staged bytes after handing the file to the request, want %d", spool.UsedBytes(), len(audio))
	}
	if state.TryAdmit(uploadKey, 1) == nil {
		t.Fatal("the key could not upload again after its form was handed over")
	}
}

func TestParseMultipartReportsWhenItChangedWhatTheCallerSent(t *testing.T) {
	cases := map[string]struct {
		member  member
		changed bool
	}{
		"plain text":                   {text("prompt", "hello"), false},
		"a typed file":                 {member{name: "file", filename: "clip.wav", value: "x", headers: map[string]string{"Content-Type": "audio/wav"}}, false},
		"a filename reduced to a name": {file("file", "uploads/clip.wav", "x"), true},
		"a stripped byte order mark":   {text("prompt", "\ufeffhello"), true},
		"typed text":                   {member{name: "prompt", value: "hello", headers: map[string]string{"Content-Type": "text/plain"}}, true},
		"a header the form drops":      {member{name: "prompt", value: "hello", headers: map[string]string{"X-Note": "kept nowhere"}}, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			state := NewAdmission(MinCapacityBytes)
			form, err := ParseMultipart(t.Context(), upload(t, c.member), newSpool(t), admit(t, state), 64, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer form.Cleanup()
			if _, changed := form.SourceFields(); changed != c.changed {
				t.Fatalf("got changed %t, want %t", changed, c.changed)
			}
		})
	}
}

func TestParseMultipartRefusesAndCleansUpRequestsOutsideItsLimits(t *testing.T) {
	staged := file("file", "clip.wav", "staged before the refusal")
	fields := func(count int) []member {
		members := make([]member, count)
		for i := range members {
			members[i] = text("field", "x")
		}
		return members
	}
	cases := map[string]struct {
		members []member
		mutate  func(*http.Request)
		kind    Kind
	}{
		"more files than the endpoint takes": {
			members: []member{staged, file("extra", "second.wav", "x"), file("third", "third.wav", "x")},
			kind:    Invalid,
		},
		"a file over the size limit": {
			members: []member{staged, file("extra", "big.wav", strings.Repeat("x", 65))},
			kind:    TooLarge,
		},
		"a text field over 64 KiB": {
			members: []member{staged, text("prompt", strings.Repeat("x", maxTextFieldBytes+1))},
			kind:    Invalid,
		},
		"text fields over the aggregate limit": {
			members: append([]member{staged}, slices.Repeat([]member{text("prompt", strings.Repeat("x", maxTextFieldBytes))}, maxTextTotalBytes/maxTextFieldBytes+1)...),
			kind:    Invalid,
		},
		"more fields than the parser takes": {
			members: append([]member{staged}, fields(maxFields)...),
			kind:    Invalid,
		},
		"a field without a name": {
			members: []member{staged, text("", "x")},
			kind:    Invalid,
		},
		"text that is not UTF-8": {
			members: []member{staged, text("prompt", "\xff\xfe")},
			kind:    Invalid,
		},
		"a filename that names no file": {
			members: []member{staged, file("extra", "..", "x")},
			kind:    Invalid,
		},
		"a body over the request limit": {
			members: []member{staged, text("prompt", strings.Repeat("x", 4096))},
			mutate: func(r *http.Request) {
				r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, 1024)
			},
			kind: TooLarge,
		},
		"a truncated body": {
			members: []member{staged, text("prompt", "cut short")},
			mutate: func(r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body[:len(body)-8]))
			},
			kind: Invalid,
		},
		"a boundary with a quote in it": {
			members: []member{staged},
			mutate: func(r *http.Request) {
				r.Header.Set("Content-Type", `multipart/form-data; boundary="a\"b"`)
			},
			kind: Invalid,
		},
		"a boundary ending in a space": {
			members: []member{staged},
			mutate: func(r *http.Request) {
				r.Header.Set("Content-Type", `multipart/form-data; boundary="audio upload "`)
			},
			kind: Invalid,
		},
		"a body that is not multipart": {
			members: []member{staged},
			mutate: func(r *http.Request) {
				r.Header.Set("Content-Type", "application/json")
			},
			kind: Invalid,
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			spool := newSpool(t)
			state := NewAdmission(MinCapacityBytes)
			request := upload(t, c.members...)
			if c.mutate != nil {
				c.mutate(request)
			}

			form, failure := ParseMultipart(t.Context(), request, spool, admit(t, state), 64, 2)
			if failure == nil {
				form.Cleanup()
				t.Fatal("the request was accepted")
			}
			if failure.Kind != c.kind {
				t.Fatalf("got kind %d (%s), want %d", failure.Kind, failure.Message, c.kind)
			}
			if spool.UsedBytes() != 0 {
				t.Fatalf("the refused request left %d bytes staged", spool.UsedBytes())
			}
			left, err := os.ReadDir(spool.root)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 1 {
				t.Fatalf("the refused request left %d files in the spool, want only the ownership lock", len(left))
			}
			if state.TryAdmit(uploadKey, 1) == nil {
				t.Fatal("the refused request kept its upload reservation")
			}
		})
	}
}

func TestParseMultipartRequiresAnAdmittedUpload(t *testing.T) {
	_, err := ParseMultipart(t.Context(), upload(t, text("model", "whisper")), newSpool(t), nil, 64, 1)
	if err == nil || err.Kind != Overloaded {
		t.Fatalf("got %v, want an overloaded refusal", err)
	}
}

func TestParseMultipartAcceptsQuotedBoundarySpaces(t *testing.T) {
	spool := newSpool(t)
	state := NewAdmission(MinCapacityBytes)
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.SetBoundary("audio upload boundary"); err != nil {
		t.Fatal(err)
	}
	if err := writer.WriteField("model", "transcribe"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "clip.wav")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, "fixture audio"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	form, failure := ParseMultipart(t.Context(), request, spool, admit(t, state), 64, 1)
	if failure != nil {
		t.Fatal(failure)
	}
	defer form.Cleanup()
	file := form.TakeSingleFile("file")
	if form.Required("model") != "transcribe" || file == nil || file.Size != int64(len("fixture audio")) || form.Err() != nil {
		t.Fatalf("uploaded fields were not preserved, file %v, error %v", file, form.Err())
	}
}

func TestAdmissionBoundsStagedUploadsPerKeyAndInTotal(t *testing.T) {
	state := NewAdmission(100)
	if state.TryAdmit("idle", 0) != nil || state.TryAdmit("idle", 51) != nil {
		t.Fatal("a reservation outside half the spool capacity was admitted")
	}
	first := state.TryAdmit("first", 30)
	if first == nil {
		t.Fatal("the first upload was refused")
	}
	if state.TryAdmit("first", 10) != nil {
		t.Fatal("one key ran two uploads at once")
	}
	if state.TryAdmit("second", 30) != nil {
		t.Fatal("uploads were promised more than half the spool capacity")
	}
	if state.TryAdmit("second", 20) == nil {
		t.Fatal("a refused upload kept its key busy")
	}
	first.Release()
	first.Release()
	if state.TryAdmit("third", 30) == nil {
		t.Fatal("a released reservation was not given back")
	}
	if state.TryAdmit("fourth", 1) != nil {
		t.Fatal("releasing twice gave back more than was reserved")
	}
}

func TestFormOrdersFileArraysAndRefusesAmbiguousFields(t *testing.T) {
	parse := func(t *testing.T, members ...member) *Form {
		t.Helper()
		state := NewAdmission(MinCapacityBytes)
		form, err := ParseMultipart(t.Context(), upload(t, members...), newSpool(t), admit(t, state), 64, 16)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(form.Cleanup)
		return form
	}

	t.Run("file arrays come back bare, then by index, then appended", func(t *testing.T) {
		form := parse(t,
			file("image[]", "appended.png", "x"), file("image[10]", "ten.png", "x"),
			file("image[2]", "two.png", "x"), file("image", "bare.png", "x"), file("mask", "mask.png", "x"),
		)
		parts := form.TakeFilesWithPrefix("image")
		if form.Err() != nil {
			t.Fatal(form.Err())
		}
		names := make([]string, len(parts))
		for i, part := range parts {
			names[i] = part.Filename
		}
		if want := []string{"bare.png", "two.png", "ten.png", "appended.png"}; !slices.Equal(names, want) {
			t.Fatalf("got %v, want %v", names, want)
		}
		if mask := form.TakeSingleFile("mask"); form.Err() != nil || mask == nil || mask.Filename != "mask.png" {
			t.Fatalf("got mask %v, %v", mask, form.Err())
		}
	})

	refused := map[string]struct {
		members []member
		take    func(*Form)
	}{
		"a file index with a leading zero": {
			[]member{file("image[01]", "a.png", "x")},
			func(f *Form) { f.TakeFilesWithPrefix("image") },
		},
		"a file index that is not a number": {
			[]member{file("image[x]", "a.png", "x")},
			func(f *Form) { f.TakeFilesWithPrefix("image") },
		},
		"text where a file belongs": {
			[]member{text("image[0]", "x")},
			func(f *Form) { f.TakeFilesWithPrefix("image") },
		},
		"a single file sent twice": {
			[]member{file("clip", "a.wav", "x"), file("clip", "b.wav", "x")},
			func(f *Form) { f.TakeSingleFile("clip") },
		},
		"a single value sent twice": {
			[]member{text("language", "en"), text("language", "fr")},
			func(f *Form) { f.Optional("language") },
		},
		"a missing required value": {
			[]member{text("language", "en")},
			func(f *Form) { f.Required("model") },
		},
		"a number that is not one": {
			[]member{text("n", "many")},
			func(f *Form) { f.OptionalInt("n") },
		},
		"bare and array values mixed": {
			[]member{text("include", "a"), text("include[]", "b")},
			func(f *Form) { f.TakeRepeated("include") },
		},
		"an indexed text array": {
			[]member{text("include[0]", "a")},
			func(f *Form) { f.TakeRepeated("include") },
		},
		"an unknown field sent twice": {
			[]member{text("extra", "a"), text("extra", "b")},
			func(f *Form) { f.TakeExtensions() },
		},
		"an unknown file": {
			[]member{file("extra", "a.bin", "x")},
			func(f *Form) { f.TakeExtensions() },
		},
	}
	for name, c := range refused {
		t.Run(name, func(t *testing.T) {
			form := parse(t, c.members...)
			c.take(form)
			if form.Err() == nil {
				t.Fatal("the form accepted the field")
			}
		})
	}

	t.Run("repeated text accepts either spelling", func(t *testing.T) {
		form := parse(t, text("include", "a"), text("include", "b"), text("tag[]", "c"), text("n", "3"))
		include := form.TakeRepeated("include")
		tags := form.TakeRepeated("tag")
		count := form.OptionalInt("n")
		extra := form.TakeExtensions()
		if form.Err() != nil || !slices.Equal(include, []string{"a", "b"}) || !slices.Equal(tags, []string{"c"}) || count == nil || *count != 3 {
			t.Fatalf("got include %v, tags %v, count %v, %v", include, tags, count, form.Err())
		}
		if len(extra) != 0 {
			t.Fatalf("consumed fields came back as extensions: %v", extra)
		}
	})
}
