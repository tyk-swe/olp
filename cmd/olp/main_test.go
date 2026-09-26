package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/process"
)

func TestVersionPrintsBuildVersion(t *testing.T) {
	previous := process.Version
	process.Version = "0.1.0"
	t.Cleanup(func() { process.Version = previous })
	for _, arg := range []string{"version", "--version"} {
		t.Run(arg, func(t *testing.T) {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout := os.Stdout
			os.Stdout = write
			err = run(context.Background(), []string{arg})
			os.Stdout = stdout
			write.Close()
			output, readErr := io.ReadAll(read)
			if err != nil || readErr != nil {
				t.Fatal(err, readErr)
			}
			if string(output) != "olp 0.1.0\n" {
				t.Fatalf("version output = %q", output)
			}
		})
	}
}

func TestAccountResetPasswordArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"missing password file": {"account", "reset-password", "owner@example.com"},
		"missing arguments":     {"account", "reset-password"},
		"unknown account verb":  {"account", "unlock", "owner@example.com", "/tmp/password"},
		"flags before operands": {"account", "reset-password", "--log-level=debug", "owner@example.com"},
		"file flag operand":     {"account", "reset-password", "owner@example.com", "--password-file"},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(context.Background(), args)
			if err == nil || !strings.Contains(err.Error(), "reset-password") {
				t.Fatal("accepted invalid account arguments", err)
			}
		})
	}
	err := run(context.Background(), []string{"account", "reset-password", "owner@example.com", "/tmp/password", "stray"})
	if err == nil || !strings.Contains(err.Error(), "unexpected positional") {
		t.Fatal("accepted a positional argument after the password file", err)
	}
}
