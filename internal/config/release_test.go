package config_test

import (
	"github.com/tyk-swe/olp/internal/config"
	"io"
	"testing"
)

func TestReleaseConfigurationEnforcesAdvertisedBounds(t *testing.T) {
	base := map[string]string{"OLP_DATABASE_URL": "postgres://localhost/test", "OLP_LOCAL_LOGIN_ENABLED": "false", "OLP_GATEWAY_CORS_ALLOWED_ORIGINS": "https://console.example,https://sdk.example"}
	get := func(name string) string { return base[name] }
	c, err := config.Parse([]string{"all"}, get, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if c.LocalLoginEnabled || len(c.GatewayCORSAllowedOrigins) != 2 {
		t.Fatal("deployment auth/CORS settings ignored")
	}
	for _, flag := range []string{"--http-max-connections=0", "--http-connection-max-age-seconds=0", "--http-connection-drain-timeout-seconds=0", "--http-max-inline-media-items=0", "--http-max-inline-media-item-bytes=0", "--gateway-cors-allowed-origins=*", "--gateway-cors-allowed-origins=https://console.example/path"} {
		if _, err := config.Parse([]string{"all", flag}, get, io.Discard); err == nil {
			t.Errorf("accepted %s", flag)
		}
	}
}
