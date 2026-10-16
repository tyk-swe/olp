package media

import (
	"bytes"
	"mime/multipart"
	"net/textproto"
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
