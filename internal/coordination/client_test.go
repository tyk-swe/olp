package coordination_test

import (
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
