package config

import (
	"io"
	"testing"
)

func TestParseCarriesHostnameFromEnvironment(t *testing.T) {
	env := map[string]string{
		"OLP_DATABASE_URL": "postgres://olp@db.example/olp",
		"HOSTNAME":         "gateway-node-7",
	}
	parsed, err := Parse([]string{"gateway"}, func(name string) string { return env[name] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Hostname != "gateway-node-7" {
		t.Fatalf("Hostname = %q, want the HOSTNAME environment value", parsed.Hostname)
	}
}

func TestParseWithoutHostnameLeavesItEmpty(t *testing.T) {
	env := map[string]string{"OLP_DATABASE_URL": "postgres://olp@db.example/olp"}
	parsed, err := Parse([]string{"gateway"}, func(name string) string { return env[name] }, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Hostname != "" {
		t.Fatalf("Hostname = %q, want empty without HOSTNAME", parsed.Hostname)
	}
}
