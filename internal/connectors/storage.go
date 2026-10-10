package connectors

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/internal/egress"
)

// Storage tokens authorize only the cloud service's HTTPS endpoints. Check
// before acquiring credentials, even when the general egress policy admits
// other hosts. Loopback identity fixtures exist only in oidctest builds.
func storageDestination(endpoint *url.URL, kind string) bool {
	if endpoint == nil {
		return false
	}
	if cloudTestDestination(endpoint) {
		return true
	}
	if endpoint.Scheme != "https" || endpoint.User != nil || endpoint.Opaque != "" ||
		(endpoint.Port() != "" && endpoint.Port() != "443") {
		return false
	}
	host := strings.ToLower(endpoint.Hostname())
	switch kind {
	case "gcs":
		return host == "storage.googleapis.com" || strings.HasSuffix(host, ".storage.googleapis.com") &&
			strings.TrimSuffix(host, ".storage.googleapis.com") != ""
	case "azure_blob":
		account, found := strings.CutSuffix(host, ".blob.core.windows.net")
		if !found || len(account) < 3 || len(account) > 24 {
			return false
		}
		for _, c := range account {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
				return false
			}
		}
		return true
	}
	return false
}

// ApplyGoogleStorage uses Google's storage endpoint contract, independently
// of the regional Vertex AI provider endpoint contract.
func (a *Auth) ApplyGoogleStorage(ctx context.Context, req *http.Request, mode string, secret []byte) (egress.Sensitive, error) {
	if !storageDestination(req.URL, "gcs") {
		return egress.Sensitive{}, ErrAuthentication
	}
	if mode != "adc" && mode != "service_account" {
		return egress.Sensitive{}, ErrCredentialRejected
	}
	token, err := a.googleToken(ctx, Config{Kind: "gcs", AuthMode: mode}, secret)
	if err != nil {
		return egress.Sensitive{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token.Value)
	var sensitive egress.Sensitive
	sensitive.Add(string(secret), token.Value)
	return sensitive, nil
}
