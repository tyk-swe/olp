package database_test

import (
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/database"
)

func TestConfigurationPreservesConnectionAndEnforcesSessionSettings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, options string
		connections   int
		timeout       time.Duration
	}{
		{"defaults", "", 7, 1500 * time.Millisecond},
		{"URL overrides", "&application_name=other&TimeZone=America%2FNew_York&statement_timeout=1&lock_timeout=2&idle_in_transaction_session_timeout=3&pool_max_conns=99&connect_timeout=1", 2, 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := database.Configuration("postgres://worker:pa%3Ass%40word@db.example:5433/olp?sslmode=disable&search_path=public"+tc.options, tc.connections, tc.timeout)
			if err != nil {
				t.Fatal(err)
			}
			connection := cfg.ConnConfig
			if connection.Host != "db.example" || connection.Port != 5433 || connection.Database != "olp" || connection.User != "worker" || connection.Password != "pa:ss@word" {
				t.Fatal("database connection identity was not preserved")
			}
			if cfg.MaxConns != int32(tc.connections) || connection.ConnectTimeout != tc.timeout {
				t.Fatalf("pool size/connection timeout = %d/%s; want %d/%s", cfg.MaxConns, connection.ConnectTimeout, tc.connections, tc.timeout)
			}
			for key, want := range map[string]string{
				"application_name":                    "olp",
				"TimeZone":                            "UTC",
				"statement_timeout":                   "10000",
				"lock_timeout":                        "10000",
				"idle_in_transaction_session_timeout": "15000",
				"search_path":                         "public",
			} {
				if got := connection.RuntimeParams[key]; got != want {
					t.Errorf("session setting %s = %q; want %q", key, got, want)
				}
			}
		})
	}
}

func TestConfigurationRejectsInvalidURLsWithoutExposingCredentials(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, url string }{
		{"invalid escape", "postgres://worker:private-password%ZZ@db.example/olp"},
		{"invalid port", "postgres://worker:private-password@db.example:invalid/olp"},
		{"invalid TLS mode", "postgres://worker:private-password@db.example/olp?sslmode=private-password"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := database.Configuration(tc.url, 3, time.Second)
			if cfg != nil || err == nil || err.Error() != "invalid OLP_DATABASE_URL" {
				t.Fatalf("configuration returned %v; want a sanitized OLP_DATABASE_URL error and no pool configuration", err)
			}
		})
	}
}
