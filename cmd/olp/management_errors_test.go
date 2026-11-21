package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestManagementCLIProcess(t *testing.T) {
	if len(os.Args) >= 3 && os.Args[2] == "--" {
		os.Args = append(os.Args[:1], os.Args[3:]...)
		main()
		os.Exit(0)
	}
}

func managementCLICommand(t *testing.T, origin, tokenFile, format string, args ...string) *exec.Cmd {
	t.Helper()
	command := exec.CommandContext(t.Context(), os.Args[0], append([]string{"-test.run=^TestManagementCLIProcess$", "--"}, args...)...)
	command.Env = []string{"OLP_MANAGEMENT_URL=" + origin, "OLP_MANAGEMENT_TOKEN_FILE=" + tokenFile, "OLP_CLI_ERROR_FORMAT=" + format}
	return command
}

func runManagementCLI(t *testing.T, origin, tokenFile, format string, args ...string) (stdout, stderr string, status int) {
	t.Helper()
	command := managementCLICommand(t, origin, tokenFile, format, args...)
	var out, errOut strings.Builder
	command.Stdout, command.Stderr = &out, &errOut
	if err := command.Run(); err != nil {
		exit, ok := errors.AsType[*exec.ExitError](err)
		if !ok {
			t.Fatal(err)
		}
		return out.String(), errOut.String(), exit.ExitCode()
	}
	return out.String(), errOut.String(), 0
}

func cliInputFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireCLIFailure(t *testing.T, origin, token, format, want string, args ...string) {
	t.Helper()
	out, stderr, exit := runManagementCLI(t, origin, token, format, args...)
	if exit != 1 || out != "" || stderr != want+"\n" {
		t.Fatalf("exit=%d, stdout=%q, stderr=%q; want exit=1, no stdout and stderr=%q", exit, out, stderr, want+"\n")
	}
}

func TestManagementCLIStructuredErrors(t *testing.T) {
	t.Parallel()
	const problemPrefix = "https://openllmproxy.dev/problems/"
	token := cliInputFile(t, "olp_test_only_management_token\n")
	malformed := cliInputFile(t, `{"private-input":`)
	missing := filepath.Join(t.TempDir(), "private-missing-file")
	for _, test := range []struct {
		name         string
		args         []string
		status       int
		body         string
		truncate     bool
		missingToken bool
		want         string
	}{
		{name: "stale ETag", args: []string{"providers", "activate", "provider-id", "--if-match", "stale", "--idempotency-key", "deployment"},
			status: 412, body: `{"type":"` + problemPrefix + `etag_mismatch","detail":"private-server-marker"}`, want: `{"kind":"api","status":412,"code":"etag_mismatch"}`},
		{name: "forbidden", status: 403, body: `{"type":"` + problemPrefix + `permission_denied"}`, want: `{"kind":"api","status":403,"code":"permission_denied"}`},
		{name: "unknown type", status: 403, body: `{"type":"` + problemPrefix + `olp_test_only_private_type","detail":"olp_test_only_private_detail"}`, want: `{"kind":"api","status":403}`},
		{name: "foreign type", status: 403, body: `{"type":"https://example.invalid/problems/permission_denied"}`, want: `{"kind":"api","status":403}`},
		{name: "wrong type shape", status: 403, body: `{"type":{"code":"permission_denied"}}`, want: `{"kind":"api","status":403}`},
		{name: "ambiguous type", status: 403, body: `{"type":"private","type":"` + problemPrefix + `permission_denied"}`, want: `{"kind":"api","status":403}`},
		{name: "malformed problem", status: 403, body: `{"type":"` + problemPrefix + `permission_denied"}private`, want: `{"kind":"api","status":403}`},
		{name: "unreadable rejection", status: 403, body: `{"type":"` + problemPrefix + `permission_denied"}`, truncate: true, want: `{"kind":"api","status":403}`},
		{name: "malformed success", status: 200, body: "private-response-marker", want: `{"kind":"invalid_response"}`},
		{name: "unreadable success", status: 200, body: `{}`, truncate: true, want: `{"kind":"invalid_response"}`},
		{name: "invalid CSV shape", args: []string{"usage", "summary", "--query", "start=2026-01-01T00:00:00Z", "--query", "end=2026-01-02T00:00:00Z", "--format", "csv"},
			status: 200, body: `{"items":"private-response-marker"}`, want: `{"kind":"invalid_response"}`},
		{name: "output file error", args: []string{"keys", "list", "--output", filepath.Join(missing, "output")}, status: 200, body: `{}`, want: `{"kind":"local"}`},
		{name: "SDK argument validation", args: []string{"api", "activate_provider", "provider-id", "--if-match", "stale"}, want: `{"kind":"invalid_input"}`},
		{name: "invalid option", args: []string{"keys", "list", "--private-option", "private-value"}, want: `{"kind":"invalid_input"}`},
		{name: "invalid local JSON", args: []string{"keys", "create", "--body-file", malformed}, want: `{"kind":"invalid_input"}`},
		{name: "missing body file", args: []string{"keys", "create", "--body-file", missing}, want: `{"kind":"local"}`},
		{name: "missing token file", missingToken: true, want: `{"kind":"local"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.status == 0 {
					t.Error("local validation sent an HTTP request")
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.Header().Set("ETag", `"private-etag-marker"`)
				if test.truncate {
					w.Header().Set("Content-Length", "2048")
				}
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			args := test.args
			if args == nil {
				args = []string{"keys", "list"}
			}
			tokenFile := token
			if test.missingToken {
				tokenFile = missing
			}
			requireCLIFailure(t, server.URL, tokenFile, "json", test.want, args...)
		})
	}
}

func TestManagementCLIDroppedWriteResponse(t *testing.T) {
	t.Parallel()
	const payload = `{"name":"fixture-key"}`
	const created = `{"secret":"olp_test_only_created_key"}`
	token := cliInputFile(t, "olp_test_only_management_token\n")
	body := cliInputFile(t, payload)
	type request struct{ method, path, key, body string }
	var mu sync.Mutex
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		requests = append(requests, request{r.Method, r.URL.Path, r.Header.Get("Idempotency-Key"), string(data)})
		first := len(requests) == 1
		mu.Unlock()
		if first {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		w.Header().Set("ETag", `"created"`)
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, created)
	}))
	defer server.Close()
	wantRequest := request{http.MethodPost, "/api/v1/api-keys", "fixture-idempotency-key", payload}
	checkRequests := func(want ...request) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if !slices.Equal(requests, want) {
			t.Fatalf("requests = %+v; want %+v", requests, want)
		}
	}
	args := []string{"keys", "create", "--body-file", body, "--idempotency-key", wantRequest.key}
	requireCLIFailure(t, server.URL, token, "json", `{"kind":"transport"}`, args...)
	checkRequests(wantRequest)
	out, stderr, exit := runManagementCLI(t, server.URL, token, "json", args...)
	if exit != 0 || out != created+"\n" || stderr != "ETag: \"created\"\n" {
		t.Fatalf("caller retry: exit=%d, stdout=%q, stderr=%q", exit, out, stderr)
	}
	checkRequests(wantRequest, wantRequest)
}

func TestManagementCLIErrorFormatScope(t *testing.T) {
	t.Parallel()
	token := cliInputFile(t, "olp_test_only_management_token\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"type":"https://openllmproxy.dev/problems/permission_denied"}`)
	}))
	defer server.Close()
	requireCLIFailure(t, server.URL, token, "", "olp: management API returned 403 (permission_denied)", "keys", "list")
	requireCLIFailure(t, server.URL, token, "JSON", "olp: management API returned 403 (permission_denied)", "keys", "list")
	requireCLIFailure(t, server.URL, token, "json", "olp: management API returned 403 (permission_denied)", "config", "export")
	requireCLIFailure(t, server.URL, token, "json", "olp: code-setup takes named options", "code-setup", "preview", "extra")
	requireCLIFailure(t, server.URL, token, "json", "olp: setup-token takes no arguments", "setup-token", "extra")
	humanHelp, stderr, exit := runManagementCLI(t, server.URL, token, "", "keys", "help")
	if exit != 0 || stderr != "" || !strings.Contains(humanHelp, "create_api_key") {
		t.Fatalf("default help: exit=%d, stdout=%q, stderr=%q", exit, humanHelp, stderr)
	}
	out, stderr, exit := runManagementCLI(t, server.URL, token, "json", "keys", "help")
	if exit != 0 || out != humanHelp || stderr != "" {
		t.Fatalf("opt-in help: exit=%d, stdout=%q, stderr=%q", exit, out, stderr)
	}
	server.Close()
	requireCLIFailure(t, server.URL, token, "json", `{"kind":"transport"}`, "keys", "list")
	requireCLIFailure(t, "private-invalid-origin", token, "json", `{"kind":"local"}`, "keys", "list")
}

func TestManagementCLIStructuredErrorOnClosedStdout(t *testing.T) {
	t.Parallel()
	token := cliInputFile(t, "olp_test_only_management_token\n")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"fixture"`)
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	defer writer.Close()

	command := managementCLICommand(t, server.URL, token, "json", "keys", "list")
	command.Stdout = writer
	var stderr strings.Builder
	command.Stderr = &stderr
	err = command.Run()
	exit, ok := errors.AsType[*exec.ExitError](err)
	if !ok || exit.ExitCode() != 1 || stderr.String() != "{\"kind\":\"local\"}\n" {
		t.Fatalf("execution error=%v, stderr=%q; want exit=1 and one local error", err, stderr.String())
	}
}
