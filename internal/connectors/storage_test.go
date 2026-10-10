package connectors

import (
	"net/http"
	"testing"
	"time"

	cloudauth "cloud.google.com/go/auth"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
)

func TestStorageAuthenticationUsesOnlyTrustedServiceEndpoints(t *testing.T) {
	for _, kind := range []string{"gcs", "azure_blob"} {
		t.Run(kind, func(t *testing.T) {
			a := NewAuth(localPolicy())
			mode := "adc"
			allowed := []string{"https://storage.googleapis.com/bucket/object", "https://bucket.storage.googleapis.com/object", "https://storage.googleapis.com:443/bucket/object"}
			if kind == "azure_blob" {
				mode = "azure_default"
				allowed = []string{"https://account.blob.core.windows.net/container/object", "https://account.blob.core.windows.net:443/container/object"}
			}
			key := cacheKey(Config{Kind: kind, AuthMode: mode}, nil)
			a.tokens[key] = &cloudauth.Token{Value: "storage-token", Expiry: time.Now().Add(time.Hour)}
			a.azureTokens[key] = azcore.AccessToken{Token: "storage-token", ExpiresOn: time.Now().Add(time.Hour)}
			apply := a.ApplyGoogleStorage
			if kind == "azure_blob" {
				apply = a.ApplyAzureStorage
			}
			for _, endpoint := range allowed {
				req, _ := http.NewRequest(http.MethodPut, endpoint, nil)
				if _, err := apply(t.Context(), req, mode, nil); err != nil || req.Header.Get("Authorization") != "Bearer storage-token" {
					t.Fatalf("storage authentication failed for %s: %v", endpoint, err)
				}
			}
			for _, endpoint := range []string{
				"https://attacker.example/collect", "https://storage.googleapis.com.attacker.example/object",
				"https://account.blob.core.windows.net.attacker.example/object", "https://aiplatform.googleapis.com/object",
				"https://storage.googleapis.com:8443/object", "https://account.blob.core.windows.net:8443/object",
				"http://storage.googleapis.com/object", "http://account.blob.core.windows.net/object",
				"https://user@storage.googleapis.com/object", "https://user@account.blob.core.windows.net/object",
			} {
				req, _ := http.NewRequest(http.MethodPut, endpoint, nil)
				if _, err := apply(t.Context(), req, mode, nil); err != ErrAuthentication || req.Header.Get("Authorization") != "" {
					t.Fatalf("storage credential accepted an unrelated destination %s: %v", endpoint, err)
				}
			}
		})
	}
}
