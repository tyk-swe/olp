//go:build bench

package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// The x-mock headers took effect: the first frame waited for them.

// Without -strict the run is reported, not failed.

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
