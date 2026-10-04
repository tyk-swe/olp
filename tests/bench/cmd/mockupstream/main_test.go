//go:build bench

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// start runs the command and returns the origin it printed and a stop function
// that returns its error.
func start(t *testing.T, args ...string) (origin string, stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	var stderr bytes.Buffer
	var result error
	finished := make(chan struct{})
	go func() { result = run(ctx, args, pw, &stderr); pw.Close(); close(finished) }()
	line, err := bufio.NewReader(pr).ReadString('\n')
	if err != nil {
		cancel()
		<-finished
		t.Fatalf("no listening line: %v, %v\n%s", err, result, stderr.String())
	}
	go io.Copy(io.Discard, pr)
	var listening struct{ Listener, Address string }
	if err := json.Unmarshal([]byte(line), &listening); err != nil || listening.Listener != "mock" || listening.Address == "" {
		cancel()
		t.Fatalf("listening line %q: %v", line, err)
	}
	stop = func() error { cancel(); <-finished; return result }
	t.Cleanup(func() { stop() })
	return "http://" + listening.Address, stop
}

func post(t *testing.T, url, body string, headers ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(got)
}

const chat = `{"model":"%s","messages":[{"role":"user","content":"hi"}]}`

func TestRunPrintsItsAddressAndServes(t *testing.T) {
	origin, stop := start(t, "-addr", "127.0.0.1:0", "-output-tokens", "3", "-ttft-ms", "30")
	begin := time.Now()
	status, body := post(t, origin+"/v1/chat/completions", strings.ReplaceAll(chat, "%s", "m"))
	if status != 200 || !strings.Contains(body, `"completion_tokens":3`) || time.Since(begin) < 30*time.Millisecond {
		t.Fatalf("%d after %v: %s", status, time.Since(begin), body)
	}
	if err := stop(); err != nil {
		t.Fatalf("clean shutdown returned %v", err)
	}
}

func TestModelFlagsAndConfigFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "mock.json")
	if err := os.WriteFile(file, []byte(`{"default":{"output_tokens":5,"status":0},"models":{"from-file":{"output_tokens":2},"overridden":{"status":429}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// The file's default survives; flags override field by field; a -model
	// flag overlays the file's rule for the same model.
	origin, _ := start(t, "-config", file, "-fail-first-n", "1", "-model", "primary:status=503", "-model", "overridden:status=500,output_tokens=7")
	url := origin + "/v1/chat/completions"
	for _, tc := range []struct {
		model  string
		status int
		want   string
	}{
		{"primary", 503, ``},
		{"from-file", 503, ``},
		{"from-file", 200, `"completion_tokens":2`},
		{"unlisted", 503, ``},
		{"unlisted", 200, `"completion_tokens":5`},
		{"overridden", 500, ``},
	} {
		status, body := post(t, url, strings.ReplaceAll(chat, "%s", tc.model))
		if status != tc.status || !strings.Contains(body, tc.want) {
			t.Errorf("%s: %d %s, want %d %s", tc.model, status, body, tc.status, tc.want)
		}
	}
	resp, err := http.Get(origin + "/_mock/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var spec struct {
		Default struct {
			FailFirstN   int `json:"fail_first_n"`
			OutputTokens int `json:"output_tokens"`
		}
		Models map[string]struct {
			Status       int `json:"status"`
			OutputTokens int `json:"output_tokens"`
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil || spec.Default.FailFirstN != 1 || spec.Default.OutputTokens != 5 || spec.Models["overridden"].Status != 500 || spec.Models["overridden"].OutputTokens != 7 || spec.Models["primary"].OutputTokens != 5 {
		t.Fatalf("effective configuration %+v %v", spec, err)
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-h"}, io.Discard, &out); err != nil || !strings.Contains(out.String(), "-addr") && !strings.Contains(out.String(), "-rate") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestRunRejectsBadInput(t *testing.T) {
	for _, args := range [][]string{
		{"-nonsense"},
		{"stray"},
		{"-output-tokens", "0"},
		{"-status", "302"},
		{"-fail-mode", "explode"},
		{"-ttft-ms", "-1"},
		{"-model", "no-colon"},
		{"-model", ":status=503"},
		{"-model", "m:status=teapot"},
		{"-model", "m:unknown=1"},
		{"-config", filepath.Join(t.TempDir(), "missing.json")},
		{"-addr", "256.256.256.256:1"},
	} {
		if err := run(context.Background(), args, io.Discard, io.Discard); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"default":{"nope":1}}`), 0o600)
	if err := run(context.Background(), []string{"-config", bad}, io.Discard, io.Discard); err == nil {
		t.Error("a config file with an unknown field was accepted")
	}
}
