package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestManagementCLIListPages(t *testing.T) {
	t.Parallel()
	const start = "start+=?&"
	const cursor = "https://unused.invalid/bad?limit=200&search=other"
	const first = `{"items":[{"id":"z","count":9007199254740993}],"next_cursor":"` + cursor + `"}`
	const last = `{"items":[{"id":"a"}],"next_cursor":null}`
	const combined = `{"items":[{"id":"z","count":9007199254740993},{"id":"a"}]}`
	token := cliInputFile(t, "fixture-token\n")

	for _, test := range []struct {
		name, path, filter, value, flag, start, want string
		args                                         []string
		cursors                                      []string
		pages                                        []string
	}{
		{name: "default single page", path: "/api/v1/providers", filter: "search", value: "staging", args: []string{"providers", "list"},
			cursors: []string{""}, pages: []string{first}, want: first},
		{name: "false stays single page", path: "/api/v1/providers", filter: "search", value: "staging", args: []string{"providers", "list"},
			flag: "--all-pages=false", cursors: []string{""}, pages: []string{first}, want: first},
		{name: "providers", path: "/api/v1/providers", filter: "search", value: "staging & preview", args: []string{"providers", "list"},
			flag: "--all-pages=true", cursors: []string{"", cursor}, pages: []string{first, last}, want: combined},
		{name: "empty providers", path: "/api/v1/providers", filter: "search", value: "no matches", args: []string{"providers", "list"},
			flag: "--all-pages=true", cursors: []string{""}, pages: []string{`{"items":[],"next_cursor":null}`}, want: `{"items":[]}`},
		{name: "keys from explicit cursor", path: "/api/v1/api-keys", filter: "created_by", value: "b6e5d9a9-f090-4a97-bdb6-6ade78caede2", args: []string{"api", "list_api_keys"},
			flag: "--all-pages=true", start: start, cursors: []string{start, cursor}, pages: []string{first, last}, want: combined},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				page := int(calls.Add(1)) - 1
				if page >= len(test.cursors) {
					t.Errorf("unexpected page request %d", page+1)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				wantQuery := url.Values{"limit": {"25"}, test.filter: {test.value}}
				if test.cursors[page] != "" {
					wantQuery.Set("cursor", test.cursors[page])
				}
				if r.Method != http.MethodGet || r.URL.Path != test.path || r.URL.Query().Encode() != wantQuery.Encode() || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Errorf("page %d: got %s %s, want GET %s?%s with management authorization", page+1, r.Method, r.URL, test.path, wantQuery.Encode())
				}
				io.WriteString(w, test.pages[page])
			}))
			defer server.Close()
			args := append(slices.Clone(test.args), "--query", test.filter+"="+test.value, "--query", "limit=25")
			if test.start != "" {
				args = append(args, "--query", "cursor="+test.start)
			}
			if test.flag != "" {
				args = append(args, test.flag)
			}
			out, stderr, exit := runManagementCLI(t, server.URL, token, "json", args...)
			if exit != 0 || stderr != "" || out != test.want+"\n" || int(calls.Load()) != len(test.cursors) {
				t.Fatalf("exit=%d stdout=%q stderr=%q requests=%d", exit, out, stderr, calls.Load())
			}
		})
	}
}

func TestManagementCLIPaginationFailurePreservesOutput(t *testing.T) {
	t.Parallel()
	const prefix = `{"items":[{"id":"first"}],"next_cursor":"second"}`
	const prior = "existing inventory\n"
	token := cliInputFile(t, "fixture-token\n")

	for _, test := range []struct {
		name, start, second, want string
		status                    int
		pages                     int32
	}{
		{name: "refused second page", pages: 2, status: 403,
			second: `{"type":"https://openllmproxy.dev/problems/permission_denied","detail":"private"}`,
			want:   `{"kind":"api","status":403,"code":"permission_denied"}`},
		{name: "redirected second page", pages: 2, status: 302, want: `{"kind":"api","status":302}`},
		{name: "repeated cursor", pages: 2, second: prefix, want: `{"kind":"invalid_response"}`},
		{name: "repeated starting cursor", pages: 1, start: "second", want: `{"kind":"invalid_response"}`},
		{name: "null items", pages: 2, second: `{"items":null}`, want: `{"kind":"invalid_response"}`},
		{name: "invalid item", pages: 2, second: `{"items":[null]}`, want: `{"kind":"invalid_response"}`},
		{name: "invalid cursor", pages: 2, second: `{"items":[],"next_cursor":1}`, want: `{"kind":"invalid_response"}`},
		{name: "empty cursor", pages: 2, second: `{"items":[],"next_cursor":""}`, want: `{"kind":"invalid_response"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, useFile := range []bool{false, true} {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch page := calls.Add(1); {
					case r.URL.Path != "/api/v1/providers" || page > test.pages:
						t.Errorf("unexpected request for page %d: %s", page, r.URL)
						w.WriteHeader(http.StatusBadRequest)
					case page == 1:
						io.WriteString(w, prefix)
					default:
						if test.status == http.StatusFound {
							w.Header().Set("Location", "/must-not-follow")
						}
						if test.status != 0 {
							w.WriteHeader(test.status)
						}
						io.WriteString(w, test.second)
					}
				}))
				t.Cleanup(server.Close)
				args := []string{"providers", "list", "--all-pages=true"}
				if test.start != "" {
					args = append(args, "--query", "cursor="+test.start)
				}
				output := cliInputFile(t, prior)
				if useFile {
					args = append(args, "--output", output)
				}
				requireCLIFailure(t, server.URL, token, "json", test.want, args...)
				data, err := os.ReadFile(output)
				if err != nil || string(data) != prior || calls.Load() != test.pages {
					t.Fatalf("file=%q err=%v requests=%d, want original file and %d requests", data, err, calls.Load(), test.pages)
				}
			}
		})
	}
}

func TestManagementCLIPaginationCaps(t *testing.T) {
	t.Parallel()
	token := cliInputFile(t, "fixture-token\n")
	for _, test := range []struct {
		name  string
		pages int32
		body  func(int32) string
	}{
		{name: "page cap", pages: 100, body: func(page int32) string {
			return fmt.Sprintf(`{"items":[],"next_cursor":"page-%d"}`, page+1)
		}},
		{name: "aggregate byte cap", pages: 2, body: func(page int32) string {
			if page == 1 {
				return `{"items":[],"next_cursor":"second"}` + strings.Repeat(" ", 8<<20)
			}
			return `{"items":[]}` + strings.Repeat(" ", 8<<20)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				page := calls.Add(1)
				if page > test.pages {
					t.Errorf("unexpected page %d", page)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				io.WriteString(w, test.body(page))
			}))
			defer server.Close()
			const prior = "existing inventory\n"
			output := cliInputFile(t, prior)
			requireCLIFailure(t, server.URL, token, "json", `{"kind":"invalid_response"}`, "keys", "list", "--all-pages=true", "--output", output)
			data, err := os.ReadFile(output)
			if err != nil || string(data) != prior || calls.Load() != test.pages {
				t.Fatalf("file=%q err=%v requests=%d, want original file and %d requests", data, err, calls.Load(), test.pages)
			}
		})
	}
}

func TestManagementCLIRejectsUnsupportedPaginationBeforeDispatch(t *testing.T) {
	t.Parallel()
	token := cliInputFile(t, "fixture-token\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()
	for _, args := range [][]string{
		{"keys", "create", "--body-file", cliInputFile(t, `{"name":"fixture-key"}`), "--idempotency-key", "create-test", "--all-pages=true"},
		{"providers", "get", "fixture-provider", "--all-pages=false"},
		{"keys", "list", "--all-pages=yes"},
		{"keys", "list", "--all-pages="},
	} {
		requireCLIFailure(t, server.URL, token, "json", `{"kind":"invalid_input"}`, args...)
	}
}
