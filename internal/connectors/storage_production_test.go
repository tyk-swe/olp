//go:build !oidctest

package connectors

import (
	"net/http"
	"testing"
)

func TestProductionStorageAuthenticationRejectsLoopbackBeforeCredentials(t *testing.T) {
	a := NewAuth(localPolicy())
	for _, endpoint := range []string{"http://127.0.0.1/token", "https://127.0.0.1/token", "https://[::1]/token"} {
		for _, apply := range []struct {
			kind, mode string
		}{
			{"gcs", "adc"}, {"gcs", "service_account"},
			{"azure_blob", "azure_default"}, {"azure_blob", "azure_client_secret"},
		} {
			req, _ := http.NewRequest(http.MethodPut, endpoint, nil)
			fn := a.ApplyGoogleStorage
			if apply.kind == "azure_blob" {
				fn = a.ApplyAzureStorage
			}
			if _, err := fn(t.Context(), req, apply.mode, nil); err != ErrAuthentication || req.Header.Get("Authorization") != "" {
				t.Fatalf("production %s accepted %s: %v", apply.kind, endpoint, err)
			}
		}
	}
}
