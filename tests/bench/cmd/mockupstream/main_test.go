//go:build bench

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
