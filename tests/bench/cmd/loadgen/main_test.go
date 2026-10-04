//go:build bench

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/tests/bench/loadgen"
	"github.com/tyk-swe/olp/tests/bench/mockupstream"
)

// target serves a mock on loopback and returns its origin.
func target(t *testing.T) string {
	t.Helper()
	server, err := mockupstream.New(mockupstream.Config{Default: mockupstream.DefaultBehavior()})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mockupstream.Serve(ctx, ln, server) }()
	t.Cleanup(func() { cancel(); <-done })
	return "http://" + ln.Addr().String()
}

func TestRunWritesTheReport(t *testing.T) {
	origin := target(t)
	out := filepath.Join(t.TempDir(), "report.json")
	var stderr bytes.Buffer
	err := run(context.Background(), []string{
		"-url", origin, "-dialect", "anthropic", "-model", "m", "-api-key", "k", "-rate", "50", "-duration", "1s", "-warmup", "200ms",
		"-stream", "-prompt-tokens", "0,2k", "-max-tokens", "8", "-header", "x-mock-ttft-ms: 20", "-header", "x-mock-interval-ms:2",
		"-late-after", "1s", "-json", out, "-name", "direct",
	}, nil, &stderr)
	if err != nil {
		t.Fatalf("%v\n%s", err, stderr.String())
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var report loadgen.Report
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	c := report.Requests
	if report.Name != "direct" || c.Sent != 50 || c.Succeeded != 50 || c.Streams != 50 || c.WarmupSent != 10 || report.Config.Dialect != loadgen.Anthropic || report.Config.MaxTokens != 8 {
		t.Fatalf("%+v %+v", report.Config, c)
	}
	// The x-mock headers took effect: the first frame waited for them.
	if report.Latency.TTFT.P50Ms < 20 || report.Latency.TTFT.Count != 50 {
		t.Fatalf("%+v", report.Latency.TTFT)
	}
	if got := report.Config.PromptTokens; len(got) != 2 || got[0] != 0 || got[1] != 2000 {
		t.Fatalf("prompt sizes %v", got)
	}
	if !strings.Contains(stderr.String(), "p99") {
		t.Fatalf("no summary on standard error:\n%s", stderr.String())
	}
}

func TestQuietAndStandardOutput(t *testing.T) {
	origin := target(t)
	var stdout, stderr bytes.Buffer
	if err := run(context.Background(), []string{"-url", origin, "-model", "m", "-rate", "20", "-duration", "500ms", "-warmup", "0", "-quiet", "-json", "-", "-late-after", "1s"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("-quiet printed %q", stderr.String())
	}
	var report loadgen.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Requests.Sent != 10 {
		t.Fatalf("standard output is not the report: %v\n%s", err, stdout.String())
	}
}

func TestStrictMakesAnInvalidRunAnError(t *testing.T) {
	origin := target(t)
	args := []string{"-url", origin, "-model", "m", "-rate", "200", "-duration", "500ms", "-warmup", "0", "-quiet",
		"-header", "x-mock-ttft-ms: 200", "-max-in-flight", "1", "-max-backlog", "-1"}
	// Without -strict the run is reported, not failed.
	if err := run(context.Background(), args, nil, nil); err != nil {
		t.Fatalf("%v", err)
	}
	err := run(context.Background(), append(args, "-strict"), nil, nil)
	var invalid *invalidRun
	if !errors.As(err, &invalid) || !strings.Contains(err.Error(), "dropped") {
		t.Fatalf("%v", err)
	}
}

func TestAPIKeyComesFromTheEnvironment(t *testing.T) {
	t.Setenv("LOADGEN_API_KEY", "from-env")
	keys := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case keys <- r.Header.Get("Authorization"):
		default:
		}
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	args := []string{"-url", server.URL, "-model", "m", "-rate", "1", "-duration", "1s", "-warmup", "0", "-quiet"}
	if err := run(context.Background(), args, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-keys; got != "Bearer from-env" {
		t.Fatalf("Authorization %q", got)
	}
	// The flag wins over the environment.
	if err := run(context.Background(), append(args, "-api-key", "from-flag"), nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-keys; got != "Bearer from-flag" {
		t.Fatalf("Authorization %q", got)
	}
}

func TestParseSizes(t *testing.T) {
	for in, want := range map[string][]int{"0": {0}, "50000,75000,100000": {50000, 75000, 100000}, "50k, 75k,100k": {50000, 75000, 100000}, "1k,0": {1000, 0}} {
		got, err := parseSizes(in)
		if err != nil || len(got) != len(want) {
			t.Fatalf("%q: %v %v", in, got, err)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%q: %v", in, got)
			}
		}
	}
	for _, bad := range []string{"", "abc", "-5", "1.5k", "10,,20", "k"} {
		if _, err := parseSizes(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-h"}, io.Discard, &out); err != nil || !strings.Contains(out.String(), "-addr") && !strings.Contains(out.String(), "-rate") {
		t.Fatalf("%v\n%s", err, out.String())
	}
}

func TestBadInvocations(t *testing.T) {
	for _, args := range [][]string{
		{"-nonsense"},
		{"-model", "m", "stray"},
		{},
		{"-model", "m", "-dialect", "cohere"},
		{"-model", "m", "-prompt-tokens", "lots"},
		{"-model", "m", "-header", "no colon"},
		{"-model", "m", "-header", ": value"},
		{"-model", "m", "-rate", "0"},
		{"-model", "m", "-stream-share", "2"},
		{"-model", "m", "-url", "not a url"},
		{"-model", "m", "-json", filepath.Join(t.TempDir(), "missing", "dir", "r.json"), "-rate", "10", "-duration", "100ms", "-warmup", "0", "-url", "http://127.0.0.1:1", "-timeout", "100ms"},
	} {
		if err := run(context.Background(), args, nil, nil); err == nil {
			t.Errorf("%v was accepted", args)
		}
	}
}
