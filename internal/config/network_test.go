package config_test

import (
	"io"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/config"
)

func TestManagementCIDRsValidateAndRespectFlagPrecedence(t *testing.T) {
	for _, value := range []string{"", "192.0.2.99/24, 2001:db8::/32", "0.0.0.0/0,::/0"} {
		env := map[string]string{"OLP_DATABASE_URL": "postgres://localhost/db", "OLP_MANAGEMENT_ALLOWED_CIDRS": value}
		c, err := config.Parse([]string{"all"}, func(k string) string { return env[k] }, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if value != "" && len(c.ManagementAllowedCIDRs) != 2 {
			t.Fatal("ranges were lost")
		}
		c, err = config.Parse([]string{"all", "--management-allowed-cidrs="}, func(k string) string { return env[k] }, io.Discard)
		if err != nil || len(c.ManagementAllowedCIDRs) != 0 {
			t.Fatal("flag did not clear environment ranges", err)
		}
	}
	for _, value := range []string{",", "host.example", "192.0.2.0/33", "2001:db8::/129", "::ffff:192.0.2.0/120", "::ffff:192.0.2.0/64", strings.Repeat("192.0.2.0/24,", 65)} {
		env := map[string]string{"OLP_DATABASE_URL": "postgres://localhost/db", "OLP_MANAGEMENT_ALLOWED_CIDRS": value}
		if _, err := config.Parse([]string{"all"}, func(k string) string { return env[k] }, io.Discard); err == nil {
			t.Fatalf("accepted invalid CIDRs %q", value)
		}
	}
}
