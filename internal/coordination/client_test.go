package coordination_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/coordination"
)

func TestConfigurationPreservesConnectionSettings(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, url, host, tls, username, password string
		port, database                           uint32
	}{
		{name: "defaults", url: "redis://cache.example", host: "cache.example", port: 6379, tls: "NoTls"},
		{name: "root path", url: "redis://cache.example/", host: "cache.example", port: 6379, tls: "NoTls"},
		{name: "port and database", url: "redis://cache.example:6380/4", host: "cache.example", port: 6380, database: 4, tls: "NoTls"},
		{name: "verified TLS", url: "rediss://cache.example/2", host: "cache.example", port: 6379, database: 2, tls: "SecureTls"},
		{name: "IPv6", url: "rediss://[::1]:6381/3", host: "::1", port: 6381, database: 3, tls: "SecureTls"},
		{name: "password only", url: "redis://:pa%3Ass%40word@cache.example/5", host: "cache.example", port: 6379, database: 5, tls: "NoTls", username: "default", password: "pa:ss@word"},
		{name: "ACL credentials", url: "rediss://team%2Dworker:s%2Fe%3Fcret@cache.example", host: "cache.example", port: 6379, tls: "SecureTls", username: "team-worker", password: "s/e?cret"},
		{name: "lowest port", url: "redis://cache.example:1/0", host: "cache.example", port: 1, tls: "NoTls"},
		{name: "highest port", url: "redis://cache.example:65535", host: "cache.example", port: 65535, tls: "NoTls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := coordination.Configuration(tc.url, "", 1500*time.Millisecond)
			if err != nil {
				t.Fatal(err)
			}
			request, err := cfg.ToProtobuf()
			if err != nil {
				t.Fatal(err)
			}
			addresses := request.GetAddresses()
			if len(addresses) != 1 || addresses[0].GetHost() != tc.host || addresses[0].GetPort() != tc.port {
				t.Fatalf("addresses = %v; want %s:%d", addresses, tc.host, tc.port)
			}
			if request.GetDatabaseId() != tc.database || request.GetTlsMode().String() != tc.tls {
				t.Fatalf("database=%d TLS=%s; want database=%d TLS=%s", request.GetDatabaseId(), request.GetTlsMode(), tc.database, tc.tls)
			}
			if request.GetRequestTimeout() != 1500 || request.GetConnectionTimeout() != 1500 {
				t.Fatalf("request/connection timeouts = %d/%d; want 1500ms", request.GetRequestTimeout(), request.GetConnectionTimeout())
			}
			if request.GetClientName() != "olp" || !request.GetLazyConnect() {
				t.Fatal("missing application identity or lazy connection setting")
			}
			credentials := request.GetAuthenticationInfo()
			if tc.username == "" {
				if credentials != nil {
					t.Fatal("URL without credentials configured authentication")
				}
			} else if credentials.GetUsername() != tc.username || credentials.GetPassword() != tc.password {
				t.Fatal("URL credentials were not decoded correctly")
			}
			if len(request.GetRootCerts()) != 0 {
				t.Fatal("configuration without a CA file replaced the system trust roots")
			}
		})
	}
}

func TestConfigurationRejectsInvalidURLsWithoutExposingCredentials(t *testing.T) {
	t.Parallel()
	const invalidURL = "OLP_VALKEY_URL must be a redis:// or rediss:// URL without query or fragment"
	for _, tc := range []struct{ name, url, message string }{
		{"empty", "", invalidURL},
		{"scheme", "https://worker:private-password@cache.example", invalidURL},
		{"missing host", "redis://worker:private-password@/0", invalidURL},
		{"invalid escape", "redis://worker:private-password%ZZ@cache.example", invalidURL},
		{"query", "redis://worker:private-password@cache.example?secret=private-password", invalidURL},
		{"fragment", "redis://worker:private-password@cache.example#private-password", invalidURL},
		{"nonnumeric port", "redis://worker:private-password@cache.example:invalid", invalidURL},
		{"zero port", "redis://worker:private-password@cache.example:0", "invalid OLP_VALKEY_URL port"},
		{"large port", "redis://worker:private-password@cache.example:65536", "invalid OLP_VALKEY_URL port"},
		{"negative database", "redis://worker:private-password@cache.example/-1", "invalid OLP_VALKEY_URL database"},
		{"nonnumeric database", "redis://worker:private-password@cache.example/private-password", "invalid OLP_VALKEY_URL database"},
		{"nested path", "redis://worker:private-password@cache.example/0/1", "invalid OLP_VALKEY_URL database"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := coordination.Configuration(tc.url, "", time.Second)
			if cfg != nil || err == nil || err.Error() != tc.message {
				t.Fatalf("configuration returned %v; want sanitized error %q and no client configuration", err, tc.message)
			}
		})
	}
}

func TestConfigurationValidatesCustomTrustRoots(t *testing.T) {
	t.Parallel()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, public, private)
	if err != nil {
		t.Fatal(err)
	}
	trust := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	dir := t.TempDir()
	for name, data := range map[string][]byte{"ca.pem": trust, "empty.pem": {}, "invalid.pem": []byte("private-password is not a certificate")} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, url, file, message string }{
		{"custom roots", "rediss://cache.example", "ca.pem", ""},
		{"plaintext", "redis://cache.example", "ca.pem", "OLP_VALKEY_TLS_CA_FILE requires rediss://"},
		{"missing file", "rediss://cache.example", "missing.pem", "OLP_VALKEY_TLS_CA_FILE must contain PEM trust roots"},
		{"empty file", "rediss://cache.example", "empty.pem", "OLP_VALKEY_TLS_CA_FILE must contain PEM trust roots"},
		{"invalid PEM", "rediss://cache.example", "invalid.pem", "OLP_VALKEY_TLS_CA_FILE must contain PEM trust roots"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := coordination.Configuration(tc.url, filepath.Join(dir, tc.file), time.Second)
			if tc.message != "" {
				if cfg != nil || err == nil || err.Error() != tc.message {
					t.Fatalf("configuration returned %v; want sanitized error %q and no client configuration", err, tc.message)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			request, err := cfg.ToProtobuf()
			if err != nil {
				t.Fatal(err)
			}
			roots := request.GetRootCerts()
			if request.GetTlsMode().String() != "SecureTls" || len(roots) != 1 || !bytes.Equal(roots[0], trust) {
				t.Fatal("custom trust roots were not passed to the verified TLS connection")
			}
		})
	}
}
