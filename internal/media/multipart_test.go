package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
)

// buildMultipart constructs a multipart request body for tests.
func buildMultipart(t *testing.T, fields map[string]string, files map[string][]byte) (string, *bytes.Buffer) {
	t.Helper()
	return buildMultipartTyped(t, fields, files, "application/octet-stream")
}

// buildMultipartTyped constructs a multipart body whose file parts carry an
// explicit content type.
func buildMultipartTyped(t *testing.T, fields map[string]string, files map[string][]byte, contentType string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for name, value := range fields {
		if err := writer.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	for name, data := range files {
		header := textproto.MIMEHeader{}
		header.Set("Content-Disposition", `form-data; name="`+name+`"; filename="`+name+`.bin"`)
		header.Set("Content-Type", contentType)
		part, err := writer.CreatePart(header)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(data)
	}
	writer.Close()
	return writer.FormDataContentType(), &buf
}

func parseAdmission(t *testing.T, state *AdmissionState, bytes int64) *Admission {
	t.Helper()
	lease := state.TryAdmit("key-1", bytes)
	if lease == nil {
		t.Fatal("admission refused")
	}
	return &Admission{Lease: lease, Route: RouteAdmission{Kind: RouteUnrestricted}}
}

func TestValidateBoundaryPolicy(t *testing.T) {
	for _, bad := range []string{
		"",
		"application/json",
		"multipart/form-data",
		"multipart/form-data; boundary=" + strings.Repeat("a", 201),
		`multipart/form-data; boundary="bad\boundary"`,
	} {
		if err := ValidateBoundary(bad); err == nil {
			t.Fatalf("boundary %q accepted", bad)
		}
	}
	if err := ValidateBoundary("multipart/form-data; boundary=valid-boundary"); err != nil {
		t.Fatal(err)
	}
}

func TestParseMultipartStagesFilesAndFields(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	state := NewAdmissionState(MinCapacityBytes)
	contentType, body := buildMultipart(t,
		map[string]string{"model": "image-model", "prompt": "a photo"},
		map[string][]byte{"image": bytes.Repeat([]byte("i"), 4096)})
	r := httptest.NewRequest("POST", "/v1/images/edits", body)
	r.Header.Set("Content-Type", contentType)
	form, err := ParseMultipart(context.Background(), r, spool,
		parseAdmission(t, state, int64(body.Len())), 1<<20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer form.Cleanup()
	model, failure := form.Required("model")
	if failure != nil || model != "image-model" {
		t.Fatalf("model: %q %v", model, failure)
	}
	part, failure := form.TakeSingleFile("image")
	if failure != nil || part == nil || part.Size != 4096 {
		t.Fatalf("file: %+v %v", part, failure)
	}
	expected := sha256.Sum256(bytes.Repeat([]byte("i"), 4096))
	if part.Digest != hex.EncodeToString(expected[:]) {
		t.Fatal("multipart part lost the exact staged byte digest")
	}
	ref, err := part.BlobReference()
	if err != nil || ref.ID() != string(part.Handle) || ref.Digest() != part.Digest || ref.Size() != part.Size {
		t.Fatal("multipart part has no complete OIF byte identity", err)
	}
	if _, failure := form.TakeExtensions(); failure != nil {
		t.Fatal(failure)
	}
	opened, err := spool.Open(part.Handle)
	if err != nil {
		t.Fatal(err)
	}
	opened.File.Close()
	if opened.Artifact.Digest != part.Digest {
		t.Fatal("multipart/spool digest drifted across handoff")
	}
}

func TestParseMultipartFileLimits(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	state := NewAdmissionState(MinCapacityBytes)
	contentType, body := buildMultipart(t, nil,
		map[string][]byte{"image": bytes.Repeat([]byte("i"), 4096)})
	r := httptest.NewRequest("POST", "/v1/images/edits", body)
	r.Header.Set("Content-Type", contentType)
	if _, err := ParseMultipart(context.Background(), r, spool,
		parseAdmission(t, state, int64(body.Len())), 100, 4); err == nil {
		t.Fatal("oversized file admitted")
	}
	if spool.UsedBytes() != 0 {
		t.Fatal("rejected parse leaked spool bytes")
	}
}

func TestParseMultipartRejectsTooManyFiles(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	state := NewAdmissionState(MinCapacityBytes)
	contentType, body := buildMultipart(t, nil, map[string][]byte{
		"a": []byte("1"), "b": []byte("2"), "c": []byte("3"),
	})
	r := httptest.NewRequest("POST", "/v1/images/edits", body)
	r.Header.Set("Content-Type", contentType)
	if _, err := ParseMultipart(context.Background(), r, spool,
		parseAdmission(t, state, int64(body.Len())), 1<<20, 2); err == nil {
		t.Fatal("file-count overflow admitted")
	}
	if spool.UsedBytes() != 0 {
		t.Fatal("rejected parse leaked spool bytes")
	}
}

func TestParseMultipartSerializesPerKey(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	state := NewAdmissionState(MinCapacityBytes)
	contentType, body := buildMultipart(t, map[string]string{"model": "m"}, nil)
	r := httptest.NewRequest("POST", "/v1/images/generations", body)
	r.Header.Set("Content-Type", contentType)
	lease := state.TryAdmit("key-1", 1<<20)
	if lease == nil {
		t.Fatal("first admission refused")
	}
	if state.TryAdmit("key-1", 1<<20) != nil {
		t.Fatal("concurrent parse for one key admitted")
	}
	form, err := ParseMultipart(context.Background(), r, spool,
		&Admission{Lease: lease, Route: RouteAdmission{Kind: RouteUnrestricted}}, 1<<20, 4)
	if err != nil {
		t.Fatal(err)
	}
	if state.TryAdmit("key-1", 1<<20) != nil {
		t.Fatal("parse released the lease while staged files persist")
	}
	form.Cleanup()
	if state.TryAdmit("key-1", 1<<20) == nil {
		t.Fatal("lease not released after cleanup")
	}
}

func TestParseMultipartRejectsDeniedAdmissionBeforeReading(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	state := NewAdmissionState(MinCapacityBytes)
	lease := state.TryAdmit("same-key", 1<<20)
	defer lease.Release()
	denied := state.TryAdmit("same-key", 1<<20)
	if denied != nil {
		t.Fatal("second admission unexpectedly succeeded")
	}
	for _, admission := range []*Admission{nil, {Lease: denied}} {
		contentType, body := buildMultipart(t, nil, map[string][]byte{"image": []byte("abc")})
		size := body.Len()
		r := httptest.NewRequest("POST", "/v1/images/edits", body)
		r.Header.Set("Content-Type", contentType)
		_, err := ParseMultipart(t.Context(), r, spool, admission, 1<<20, 1)
		var failure *Error
		if !errors.As(err, &failure) || failure.Status != http.StatusServiceUnavailable {
			t.Fatalf("denied admission accepted: %v", err)
		}
		if body.Len() != size || spool.UsedBytes() != 0 {
			t.Fatal("denied admission read or staged request bytes")
		}
	}
}

func TestFormHelpersValidate(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	state := NewAdmissionState(MinCapacityBytes)
	contentType, body := buildMultipart(t,
		map[string]string{"model": "m", "n": "3", "stream": "true"}, nil)
	r := httptest.NewRequest("POST", "/v1/images/generations", body)
	r.Header.Set("Content-Type", contentType)
	form, err := ParseMultipart(context.Background(), r, spool,
		parseAdmission(t, state, int64(body.Len())), 1<<20, 4)
	if err != nil {
		t.Fatal(err)
	}
	defer form.Cleanup()
	if _, failure := form.Required("missing"); failure == nil {
		t.Fatal("missing required field accepted")
	}
	n, failure := form.OptionalInt("n")
	if failure != nil || n == nil || *n != 3 {
		t.Fatalf("OptionalInt: %v %v", n, failure)
	}
	stream, failure := form.OptionalBool("stream")
	if failure != nil || stream == nil || !*stream {
		t.Fatalf("OptionalBool: %v %v", stream, failure)
	}
}

func TestParseMultipartDecodesQuotedPrintableAsNormalizedSource(t *testing.T) {
	for _, tc := range []struct {
		name     string
		encoding string
		file     bool
	}{
		{"plain text", "", false},
		{"quoted-printable text", "quoted-printable", false},
		{"quoted-printable file", "Quoted-Printable", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spool := testSpool(t, MinCapacityBytes)
			state := NewAdmissionState(MinCapacityBytes)
			var buf bytes.Buffer
			writer := multipart.NewWriter(&buf)
			header := textproto.MIMEHeader{}
			header.Set("Content-Disposition", `form-data; name="prompt"`)
			if tc.file {
				header.Set("Content-Disposition", `form-data; name="image"; filename="image.png"`)
				header.Set("Content-Type", "image/png")
			}
			raw, want := "caf\xc3\xa9", "caf\xc3\xa9"
			if tc.encoding != "" {
				header.Set("Content-Transfer-Encoding", tc.encoding)
				raw = "caf=C3=A9"
			}
			part, err := writer.CreatePart(header)
			if err != nil {
				t.Fatal(err)
			}
			part.Write([]byte(raw))
			writer.Close()
			r := httptest.NewRequest("POST", "/v1/images/edits", &buf)
			r.Header.Set("Content-Type", writer.FormDataContentType())
			form, err := ParseMultipart(t.Context(), r, spool, parseAdmission(t, state, 1<<20), 1<<20, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer form.Cleanup()
			fields, normalized := form.SourceFields()
			if normalized != (tc.encoding != "") {
				t.Fatalf("normalized=%v for Content-Transfer-Encoding %q", normalized, tc.encoding)
			}
			if tc.file {
				digest := sha256.Sum256([]byte(want))
				if len(fields) != 1 || fields[0].File == nil || fields[0].File.Digest != hex.EncodeToString(digest[:]) {
					t.Fatal("legacy file part did not receive the decoded bytes")
				}
				return
			}
			if len(fields) != 1 || fields[0].Text == nil || *fields[0].Text != want {
				t.Fatalf("legacy text part did not receive the decoded value: %+v", fields)
			}
		})
	}
}
