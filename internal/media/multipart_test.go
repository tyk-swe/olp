package media

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

func admit(t *testing.T, state *AdmissionState, route RouteAdmission) *Admission {
	t.Helper()
	lease := state.TryAdmit(uploadKey, 1<<20)
	if lease == nil {
		t.Fatal("the upload was not admitted")
	}
	return &Admission{Route: route, Lease: lease}
}

func refusal(t *testing.T, err error) *Error {
	t.Helper()
	failure, ok := errors.AsType[*Error](err)
	if !ok {
		t.Fatalf("got %v, want a media error", err)
	}
	return failure
}

func TestParseMultipartStagesFilesAndKeepsCallerOrder(t *testing.T) {
	spool := newSpool(t)
	state := NewAdmissionState(MinCapacityBytes)
	const audio = "RIFF fixture audio"
	request := upload(t,
		text("model", "whisper"),
		member{name: "file", filename: "../../etc/clip.wav", value: audio, headers: map[string]string{"Content-Type": "audio/wav"}},
		text("language", "\ufeffen"),
	)

	form, err := ParseMultipart(t.Context(), request, spool, admit(t, state, RouteAdmission{}), 1024, 1)
	if err != nil {
		t.Fatal(err)
	}
	fields, _ := form.SourceFields()
	names := make([]string, len(fields))
	for i, field := range fields {
		names[i] = field.Name
	}
	if want := []string{"model", "file", "language"}; !slices.Equal(names, want) {
		t.Fatalf("got fields %v, want %v", names, want)
	}
	if language, failure := form.Required("language"); failure != nil || language != "en" {
		t.Fatalf("got language %q, %v, want en", language, failure)
	}
	part, failure := form.TakeSingleFile("file")
	if failure != nil || part == nil {
		t.Fatalf("got file %v, %v", part, failure)
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
			state := NewAdmissionState(MinCapacityBytes)
			form, err := ParseMultipart(t.Context(), upload(t, c.member), newSpool(t), admit(t, state, RouteAdmission{}), 64, 1)
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
		status  int
		code    string
	}{
		"more files than the endpoint takes": {
			members: []member{staged, file("extra", "second.wav", "x"), file("third", "third.wav", "x")},
			status:  http.StatusBadRequest, code: "invalid_request",
		},
		"a file over the size limit": {
			members: []member{staged, file("extra", "big.wav", strings.Repeat("x", 65))},
			status:  http.StatusRequestEntityTooLarge, code: "media_too_large",
		},
		"a text field over 64 KiB": {
			members: []member{staged, text("prompt", strings.Repeat("x", MaxTextFieldBytes+1))},
			status:  http.StatusBadRequest, code: "invalid_request",
		},
		"text fields over the aggregate limit": {
			members: append([]member{staged}, slices.Repeat([]member{text("prompt", strings.Repeat("x", MaxTextFieldBytes))}, MaxTextTotalBytes/MaxTextFieldBytes+1)...),
			status:  http.StatusBadRequest, code: "invalid_request",
		},
		"more fields than the parser takes": {
			members: append([]member{staged}, fields(MaxFields)...),
			status:  http.StatusBadRequest, code: "invalid_request",
		},
		"a field without a name": {
			members: []member{staged, text("", "x")},
			status:  http.StatusBadRequest, code: "invalid_request",
		},
		"text that is not UTF-8": {
			members: []member{staged, text("prompt", "\xff\xfe")},
			status:  http.StatusBadRequest, code: "invalid_request",
		},
		"a filename that names no file": {
			members: []member{staged, file("extra", "..", "x")},
			status:  http.StatusBadRequest, code: "invalid_request",
		},
		"a body over the request limit": {
			members: []member{staged, text("prompt", strings.Repeat("x", 4096))},
			mutate: func(r *http.Request) {
				r.Body = http.MaxBytesReader(httptest.NewRecorder(), r.Body, 1024)
			},
			status: http.StatusRequestEntityTooLarge, code: "media_too_large",
		},
		"a truncated body": {
			members: []member{staged, text("prompt", "cut short")},
			mutate: func(r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				r.Body = io.NopCloser(bytes.NewReader(body[:len(body)-8]))
			},
			status: http.StatusBadRequest, code: "invalid_request",
		},
		"a boundary with a quote in it": {
			members: []member{staged},
			mutate: func(r *http.Request) {
				r.Header.Set("Content-Type", `multipart/form-data; boundary="a\"b"`)
			},
			status: http.StatusBadRequest, code: "invalid_request",
		},
		"a body that is not multipart": {
			members: []member{staged},
			mutate: func(r *http.Request) {
				r.Header.Set("Content-Type", "application/json")
			},
			status: http.StatusBadRequest, code: "invalid_request",
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			spool := newSpool(t)
			state := NewAdmissionState(MinCapacityBytes)
			request := upload(t, c.members...)
			if c.mutate != nil {
				c.mutate(request)
			}

			form, err := ParseMultipart(t.Context(), request, spool, admit(t, state, RouteAdmission{}), 64, 2)
			if err == nil {
				form.Cleanup()
				t.Fatal("the request was accepted")
			}
			if failure := refusal(t, err); failure.Status != c.status || failure.Code != c.code {
				t.Fatalf("got %d %s (%s), want %d %s", failure.Status, failure.Code, failure.Message, c.status, c.code)
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
	_, err := ParseMultipart(t.Context(), upload(t, text("model", "whisper")), newSpool(t), &Admission{}, 64, 1)
	if failure := refusal(t, err); failure.Status != http.StatusServiceUnavailable {
		t.Fatalf("got %d %s, want 503", failure.Status, failure.Code)
	}
}

func TestParseMultipartHoldsTheModelFieldToTheRoutePolicy(t *testing.T) {
	authorized := RouteAdmission{Kind: RouteRequireAuthorizedModel, Allowed: []string{"whisper", "scribe"}}
	expected := RouteAdmission{Kind: RouteExpected, Expected: "whisper"}
	cases := map[string]struct {
		route   RouteAdmission
		members []member
		status  int
	}{
		"an authorized route":                  {authorized, []member{text("model", "scribe")}, 0},
		"a route the key may not use":          {authorized, []member{text("model", "other")}, http.StatusForbidden},
		"no model on a restricted key":         {authorized, []member{text("prompt", "hi")}, http.StatusBadRequest},
		"a model sent as a file":               {authorized, []member{file("model", "whisper", "whisper")}, http.StatusBadRequest},
		"an authorized then a forbidden model": {authorized, []member{text("model", "scribe"), text("model", "other")}, http.StatusForbidden},
		"the route named by the header":        {expected, []member{text("model", "whisper")}, 0},
		"a model that contradicts the header":  {expected, []member{text("model", "scribe")}, http.StatusBadRequest},
		"no model on an unrestricted key":      {RouteAdmission{}, []member{text("prompt", "hi")}, 0},
		"any model on an unrestricted key":     {RouteAdmission{}, []member{text("model", "other")}, 0},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			state := NewAdmissionState(MinCapacityBytes)
			form, err := ParseMultipart(t.Context(), upload(t, c.members...), newSpool(t), admit(t, state, c.route), 64, 1)
			if c.status == 0 {
				if err != nil {
					t.Fatal(err)
				}
				form.Cleanup()
				return
			}
			if err == nil {
				form.Cleanup()
				t.Fatal("the request was accepted")
			}
			if failure := refusal(t, err); failure.Status != c.status {
				t.Fatalf("got %d %s (%s), want %d", failure.Status, failure.Code, failure.Message, c.status)
			}
		})
	}
}

func TestAdmissionBoundsStagedUploadsPerKeyAndInTotal(t *testing.T) {
	state := NewAdmissionState(100)
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
		state := NewAdmissionState(MinCapacityBytes)
		form, err := ParseMultipart(t.Context(), upload(t, members...), newSpool(t), admit(t, state, RouteAdmission{}), 64, 16)
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
		parts, failure := form.TakeFilesWithPrefix("image")
		if failure != nil {
			t.Fatal(failure)
		}
		names := make([]string, len(parts))
		for i, part := range parts {
			names[i] = part.Filename
		}
		if want := []string{"bare.png", "two.png", "ten.png", "appended.png"}; !slices.Equal(names, want) {
			t.Fatalf("got %v, want %v", names, want)
		}
		if mask, failure := form.TakeSingleFile("mask"); failure != nil || mask == nil || mask.Filename != "mask.png" {
			t.Fatalf("got mask %v, %v", mask, failure)
		}
	})

	refused := map[string]struct {
		members []member
		take    func(*Form) *Error
	}{
		"a file index with a leading zero": {
			[]member{file("image[01]", "a.png", "x")},
			func(f *Form) *Error { _, failure := f.TakeFilesWithPrefix("image"); return failure },
		},
		"a file index that is not a number": {
			[]member{file("image[x]", "a.png", "x")},
			func(f *Form) *Error { _, failure := f.TakeFilesWithPrefix("image"); return failure },
		},
		"text where a file belongs": {
			[]member{text("image[0]", "x")},
			func(f *Form) *Error { _, failure := f.TakeFilesWithPrefix("image"); return failure },
		},
		"a single file sent twice": {
			[]member{file("clip", "a.wav", "x"), file("clip", "b.wav", "x")},
			func(f *Form) *Error { _, failure := f.TakeSingleFile("clip"); return failure },
		},
		"a single value sent twice": {
			[]member{text("language", "en"), text("language", "fr")},
			func(f *Form) *Error { _, failure := f.Optional("language"); return failure },
		},
		"a missing required value": {
			[]member{text("language", "en")},
			func(f *Form) *Error { _, failure := f.Required("model"); return failure },
		},
		"a number that is not one": {
			[]member{text("n", "many")},
			func(f *Form) *Error { _, failure := f.OptionalInt("n"); return failure },
		},
		"bare and array values mixed": {
			[]member{text("include", "a"), text("include[]", "b")},
			func(f *Form) *Error { _, failure := f.TakeRepeated("include"); return failure },
		},
		"an indexed text array": {
			[]member{text("include[0]", "a")},
			func(f *Form) *Error { _, failure := f.TakeRepeated("include"); return failure },
		},
		"an unknown field sent twice": {
			[]member{text("extra", "a"), text("extra", "b")},
			func(f *Form) *Error { _, failure := f.TakeExtensions(); return failure },
		},
		"an unknown file": {
			[]member{file("extra", "a.bin", "x")},
			func(f *Form) *Error { _, failure := f.TakeExtensions(); return failure },
		},
	}
	for name, c := range refused {
		t.Run(name, func(t *testing.T) {
			if failure := c.take(parse(t, c.members...)); failure == nil || failure.Status != http.StatusBadRequest {
				t.Fatalf("got %v, want a 400", failure)
			}
		})
	}

	t.Run("repeated text accepts either spelling", func(t *testing.T) {
		form := parse(t, text("include", "a"), text("include", "b"), text("tag[]", "c"), text("n", "3"))
		include, failure := form.TakeRepeated("include")
		if failure != nil || !slices.Equal(include, []string{"a", "b"}) {
			t.Fatalf("got %v, %v", include, failure)
		}
		tags, failure := form.TakeRepeated("tag")
		if failure != nil || !slices.Equal(tags, []string{"c"}) {
			t.Fatalf("got %v, %v", tags, failure)
		}
		count, failure := form.OptionalInt("n")
		if failure != nil || count == nil || *count != 3 {
			t.Fatalf("got %v, %v", count, failure)
		}
		extra, failure := form.TakeExtensions()
		if failure != nil || len(extra) != 0 {
			t.Fatalf("consumed fields came back as extensions: %v, %v", extra, failure)
		}
	})
}
