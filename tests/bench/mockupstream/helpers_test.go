//go:build bench

package mockupstream

import (
	"bufio"
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/tests/bench/benchtest"
)

func newMock(t testing.TB, cfg Config) *Server {
	t.Helper()
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func defaultMock(t testing.TB) *Server { return newMock(t, Config{Default: DefaultBehavior()}) }

// client serves requests to s in process.
func client(s *Server) *http.Client {
	return &http.Client{Transport: benchtest.HandlerTransport{Handler: s}}
}

// request is one test request.
type request struct {
	path    string
	body    string
	headers map[string]string
}

func (r request) build(t testing.TB) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "http://mock"+r.path, strings.NewReader(r.body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	return req
}

// frame is one server-sent event with the time it was read.
type frame struct {
	event, data string
	at          time.Duration
}

// readFrames reads a stream to its end, stamping each frame with the time
// since start, and returns the read error that ended it, if any.
func readFrames(body io.Reader, start time.Time) ([]frame, error) {
	br := bufio.NewReader(body)
	var frames []frame
	var cur frame
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(line, "event:"):
			cur.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			cur.data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			cur.at = time.Since(start)
		case line == "" && cur.data != "":
			frames = append(frames, cur)
			cur = frame{}
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return frames, err
		}
	}
}

// openAIBody is a request body of the shape OLP sends: the keys sorted, so
// the model and stream flag follow the messages.
func openAIBody(model string, stream bool, prompt string) string {
	return `{"max_tokens":16,"messages":[{"role":"user","content":"` + prompt + `"}],"model":"` + model + `","stream":` + map[bool]string{true: "true", false: "false"}[stream] + `}`
}

func responsesBody(model string, stream bool, prompt string) string {
	return `{"input":"` + prompt + `","max_output_tokens":16,"model":"` + model + `","stream":` + map[bool]string{true: "true", false: "false"}[stream] + `}`
}

func anthropicBody(model string, stream bool, prompt string) string {
	return openAIBody(model, stream, prompt)
}

const geminiBody = `{"contents":[{"role":"user","parts":[{"text":"hello"}]}],"generationConfig":{"maxOutputTokens":16}}`

func (s *Server) post(t testing.TB, r request) (*http.Response, []byte) {
	t.Helper()
	resp, err := client(s).Do(r.build(t))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response: %v", err)
	}
	return resp, body
}

func wantText(n int) string { return string(text(nil, n)) }

func mustContain(t testing.TB, haystack []byte, needles ...string) {
	t.Helper()
	for _, n := range needles {
		if !bytes.Contains(haystack, []byte(n)) {
			t.Fatalf("response lacks %q:\n%s", n, haystack)
		}
	}
}
