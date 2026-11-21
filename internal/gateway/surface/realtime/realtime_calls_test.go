package realtime

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/iotest"
)

func TestParseWebRTCCallRequestRequiresCompleteMultipart(t *testing.T) {
	t.Parallel()
	const sdp = "v=0\r\n"
	const session = `{"type":"realtime"}`
	body, contentType, err := buildWebRTCCallsBody([]byte(sdp), []byte(session))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		body  io.Reader
		valid bool
	}{
		{"clean end of form", bytes.NewReader(body), true},
		{"EOF before any multipart data", iotest.ErrReader(io.EOF), false},
		{"body read failure", iotest.ErrReader(io.ErrClosedPipe), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", test.body)
			request.Header.Set("Content-Type", contentType)
			parsed, failure := parseWebRTCCallRequest(request)
			if !test.valid {
				if parsed != nil || failure == nil || failure.Status != http.StatusBadRequest || failure.Code != "invalid_value" || failure.Param != nil {
					t.Fatalf("got request %+v and error %+v, want a form-level bad request", parsed, failure)
				}
				return
			}
			if failure != nil || parsed == nil || string(parsed.sdp) != sdp || string(parsed.session) != session {
				t.Fatalf("got request %+v and error %+v, want both form parts unchanged", parsed, failure)
			}
		})
	}
}
