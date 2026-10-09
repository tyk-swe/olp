package connectors

import (
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
)

func TestVertexCredentialsRequireTheirNativeDestination(t *testing.T) {
	for _, auth := range []string{"adc", "service_account"} {
		for _, region := range []string{"global", "us-central1", "asia-northeast3", "us", "eu"} {
			for _, profile := range []string{"", "vertex-gemini", "vertex-anthropic", "vertex-openai"} {
				cfg := Config{Kind: "vertex_ai", AuthMode: auth, CloudRegion: region, CloudProject: "project", ProfileID: profile}
				if profile != "" {
					cfg.ProfileRevision = ProfileRevision
				}
				cfg.Endpoint = DefaultProfileEndpoint(cfg.Kind, profile, region, cfg.CloudProject)
				if err := cfg.Validate(&egress.Policy{}); err != nil {
					t.Fatalf("%s/%s/%s: %v", auth, region, profile, err)
				}
			}
		}
	}
	for name, endpoint := range map[string]string{
		"another host": "https://attacker.example/v1",
		"host suffix":  "https://aiplatform.googleapis.com.attacker.example/v1",
		"wrong region": "https://us-central1-aiplatform.googleapis.com/v1",
		"cleartext":    "http://aiplatform.googleapis.com/v1",
		"port":         "https://aiplatform.googleapis.com:8443/v1",
		"userinfo":     "https://attacker@aiplatform.googleapis.com/v1",
	} {
		for _, auth := range []string{"adc", "service_account"} {
			t.Run(name+"/"+auth, func(t *testing.T) {
				cfg := Config{Kind: "vertex_ai", AuthMode: auth, CloudRegion: "global", CloudProject: "project", Endpoint: endpoint}
				if err := cfg.Validate(localPolicy()); err == nil {
					t.Fatal("accepted a non-native Vertex endpoint")
				}
				request, err := http.NewRequest(http.MethodPost, endpoint, nil)
				if err != nil {
					t.Fatal(err)
				}
				a := NewAuth(localPolicy())
				if _, err = a.Apply(t.Context(), request, cfg, []byte("unread-credential"), nil); err != ErrAuthentication {
					t.Fatalf("authentication reached credential parsing: %v", err)
				}
				if a.adc != nil || len(a.tokens) != 0 || request.Header.Get("Authorization") != "" {
					t.Fatal("acquired or attached authentication before validating its destination")
				}
			})
		}
	}
}

func TestVertexCredentialsRejectCustomTrustBeforeAuthentication(t *testing.T) {
	for _, auth := range []string{"adc", "service_account"} {
		cfg := Config{Kind: "vertex_ai", AuthMode: auth, CloudRegion: "global", CloudProject: "project",
			Endpoint: DefaultEndpoint("vertex_ai", "global", "project"), Network: &egress.ConnectionOptions{TrustRootsPEM: "operator trust root"}}
		request, _ := http.NewRequest(http.MethodPost, cfg.Endpoint+"/models/model:generateContent", nil)
		a := NewAuth(localPolicy())
		if _, err := a.Apply(t.Context(), request, cfg, []byte("unread-credential"), nil); err != ErrAuthentication {
			t.Fatalf("%s: %v", auth, err)
		}
		if a.adc != nil || len(a.tokens) != 0 || request.Header.Get("Authorization") != "" {
			t.Fatal("custom trust reached authentication")
		}
	}
}
