package main

import (
	"context"
	"strings"
	"testing"
)

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
