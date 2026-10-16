package billing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// fixtureServer serves the named fixtures in order, recording requests.
type fixtureServer struct {
	t       *testing.T
	pages   [][]byte
	calls   atomic.Int64
	queries []url.Values
	server  *httptest.Server
}

func newFixtureServer(t *testing.T, path string, pages ...string) *fixtureServer {
	t.Helper()
	s := &fixtureServer{t: t}
	for _, page := range pages {
		s.pages = append(s.pages, readFixture(t, page))
	}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := s.calls.Add(1) - 1
		s.queries = append(s.queries, r.URL.Query())
		if r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if int(call) >= len(s.pages) {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(s.pages[call])
	}))
	t.Cleanup(s.server.Close)
	return s
}

// The empty bucket still produces the day so the store records zero.

// 123.45 + 67.89 cents → $1.9134 across both workspaces.

func TestValidateScope(t *testing.T) {
	for _, tc := range []struct {
		source string
		raw    string
		valid  bool
	}{
		{SourceOpenAI, `{"project_ids":["proj_abc"]}`, true},
		{SourceOpenAI, `{}`, true},
		{SourceOpenAI, `{"project_ids":["x"]}`, false},
		{SourceOpenAI, `{"project_ids":["proj_` + strings.Repeat("a", 64) + `"]}`, true},
		{SourceOpenAI, `{"project_ids":["proj_` + strings.Repeat("a", 65) + `"]}`, false},
		{SourceOpenAI, `{"workspace_ids":["w"]}`, false},
		{SourceOpenAI, `{"unknown":1}`, false},
		{SourceOpenAI, `not json`, false},
		{SourceAnthropic, `{"workspace_ids":["wrkspc_abc","default"]}`, true},
		{SourceAnthropic, `{"workspace_ids":["default"]}`, true},
		{SourceAnthropic, `{"workspace_ids":["wrkspc_"]}`, false},
		{SourceAnthropic, `{"project_ids":["proj_1"]}`, false},
	} {
		_, err := ValidateScope(tc.source, json.RawMessage(tc.raw))
		if tc.valid != (err == nil) {
			t.Fatalf("%s %s: valid=%v err=%v", tc.source, tc.raw, tc.valid, err)
		}
	}
	tooMany := `{"project_ids":[` + strings.Repeat(`"proj_a",`, 101)[:len(`"proj_a",`)*101-1] + `]}`
	if _, err := ValidateScope(SourceOpenAI, json.RawMessage(tooMany)); err == nil {
		t.Fatal("101 project ids must fail")
	}
}
