package connectors

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
)

func localPolicy() *egress.Policy {
	return &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
}

func TestGoogleServiceAccountRefreshADCBoundsAndPublicTokenEgress(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	der, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if e := r.ParseForm(); e != nil || r.Form.Get("assertion") == "" {
			t.Error("missing signed service-account assertion")
		}
		w.Header().Set("Content-Type", "application/json")
		expiry := 3600
		if n == 1 {
			expiry = 1
		}
		fmt.Fprintf(w, `{"access_token":"token-%d","token_type":"Bearer","expires_in":%d}`, n, expiry)
	}))
	defer tokenServer.Close()
	secret, _ := json.Marshal(map[string]string{"type": "service_account", "client_email": "fixture@project.iam.gserviceaccount.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), "token_uri": tokenServer.URL})
	cfg := Config{Kind: "vertex_ai", AuthMode: "service_account", CloudRegion: "global"}
	a := NewAuth(localPolicy())
	req, _ := http.NewRequest("POST", "https://aiplatform.googleapis.com/v1", nil)
	if _, e = a.Apply(context.Background(), req, cfg, secret, nil); e != ErrAuthentication || calls.Load() != 0 {
		t.Fatalf("provider exception relaxed token egress: %v", e)
	}
	// Test-only token endpoint: production always keeps Google's public-only client.
	a.publicClient = a.client
	for _, want := range []string{"Bearer token-1", "Bearer token-2", "Bearer token-2"} {
		req, _ = http.NewRequest("POST", "https://aiplatform.googleapis.com/v1", nil)
		if _, e = a.Apply(context.Background(), req, cfg, secret, nil); e != nil || req.Header.Get("Authorization") != want {
			t.Fatalf("token refresh: %s %v", req.Header.Get("Authorization"), e)
		}
	}
	path := filepath.Join(t.TempDir(), "adc.json")
	if e = os.WriteFile(path, secret, 0600); e != nil {
		t.Fatal(e)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", path)
	cfg.AuthMode = "adc"
	if _, e = a.Apply(context.Background(), req, cfg, nil, nil); e != nil {
		t.Fatal("ADC", e)
	}
	done := make(chan struct{})
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-done:
		}
	}))
	defer blocked.Close()
	defer close(done)
	var document map[string]string
	_ = json.Unmarshal(secret, &document)
	document["token_uri"] = blocked.URL
	secret, _ = json.Marshal(document)
	cfg.AuthMode = "service_account"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, e = a.Apply(ctx, req, cfg, secret, nil); e != ErrAuthentication || time.Since(started) > time.Second {
		t.Fatalf("unbounded acquisition: %v", e)
	}
	body := &boundedAuthBody{ReadCloser: io.NopCloser(strings.NewReader(strings.Repeat("x", authBodyLimit+1))), remaining: authBodyLimit}
	if _, e = io.ReadAll(body); e == nil {
		t.Fatal("unbounded auth response")
	}
}

// A provider diagnostic can echo only the signature, not the whole header.
