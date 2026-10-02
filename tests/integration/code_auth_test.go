//go:build integration

package integration_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/testutil"
)

// A trusted fixture proxy supplies the TLS peer for the production plugin's
// fixed issuer. No test build overrides or production authentication shortcuts.
type codexAuthority struct {
	proxy          *httptest.Server
	roots          string
	approved       atomic.Bool
	changedAccount atomic.Bool
	mu             sync.Mutex
	calls          []string
	refreshes      int
	accessToken    string
}

func newCodexAuthority(t *testing.T) *codexAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Codex fixture authority"},
		DNSNames: []string{"auth.openai.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	f := &codexAuthority{roots: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.Method != "POST" || r.Host != "auth.openai.com" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected auth authority, method or bearer credential")
			w.WriteHeader(400)
			return
		}
		switch r.URL.Path {
		case "/api/accounts/deviceauth/usercode":
			var request map[string]string
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["client_id"] != "app_EMoamEEZ73f0CkXaXp7hrann" || len(request) != 1 {
				t.Error("wrong device client ID")
			}
			io.WriteString(w, `{"device_auth_id":"device-fixture","user_code":"ABCD-EFGH","interval":"5"}`)
		case "/api/accounts/deviceauth/token":
			var request map[string]string
			if json.NewDecoder(r.Body).Decode(&request) != nil || request["device_auth_id"] != "device-fixture" || request["user_code"] != "ABCD-EFGH" || len(request) != 2 {
				t.Error("wrong device polling request")
			}
			if !f.approved.Load() {
				w.WriteHeader(403)
				return
			}
			io.WriteString(w, `{"authorization_code":"device-authorized","code_verifier":"device-verifier","code_challenge":"device-challenge"}`)
		case "/oauth/token":
			if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" {
				if r.ParseForm() != nil || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "device-authorized" || r.Form.Get("code_verifier") != "device-verifier" || r.Form.Get("redirect_uri") != "https://auth.openai.com/deviceauth/callback" || r.Form.Get("client_id") != "app_EMoamEEZ73f0CkXaXp7hrann" {
					t.Error("wrong device exchange")
				}
			} else {
				var request map[string]string
				if json.NewDecoder(r.Body).Decode(&request) != nil || r.Header.Get("Content-Type") != "application/json" || request["grant_type"] != "refresh_token" || request["client_id"] != "app_EMoamEEZ73f0CkXaXp7hrann" || request["refresh_token"] != fmt.Sprintf("fixture-refresh-%d", f.refreshes) {
					t.Error("worker reused an old token or changed the refresh protocol")
					w.WriteHeader(401)
					return
				}
				f.refreshes++
			}
			account := "fixture-account"
			if f.changedAccount.Load() {
				account = "another-account"
			}
			payload := fmt.Sprintf(`{"exp":%d,"jti":"generation-%d","https://api.openai.com/auth":{"chatgpt_account_id":%q,"chatgpt_user_id":"fixture-user"}}`, time.Now().Add(time.Hour).Unix(), f.refreshes, account)
			f.accessToken = "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".fixture-signature"
			json.NewEncoder(w).Encode(map[string]string{"access_token": f.accessToken, "id_token": f.accessToken, "refresh_token": fmt.Sprintf("fixture-refresh-%d", f.refreshes)})
		default:
			t.Errorf("setup or maintenance attempted inference: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	f.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			w.WriteHeader(405)
			return
		}
		target, err := net.DialTimeout("tcp", strings.TrimPrefix(server.URL, "https://"), 5*time.Second)
		if err != nil {
			w.WriteHeader(502)
			return
		}
		defer target.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffered.Flush()
		go io.Copy(target, buffered)
		io.Copy(client, target)
	}))
	t.Cleanup(f.proxy.Close)
	return f
}

func TestCodeAuthDeviceEnrollmentRefreshRotationAndPrincipalLapse(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := newCodexAuthority(t)
	digest := installPlugin(t, h, owner, testutil.BuildPlugin(t, "./plugins/codex"))
	project := h.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Codex accounts"}, idem(uuid.NewString()), 201)["id"].(string)
	configuration := map[string]any{
		"kind": "plugin", "auth_mode": "grant", "profile_id": "codex-subscription", "profile_revision": digest,
		"options": map[string]any{"network": map[string]any{"proxy_url": authority.proxy.URL, "trust_roots_pem": authority.roots}},
	}
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Codex", "project_id": project, "model": "gpt-5.3-codex", "configuration": configuration}, idem(uuid.NewString()), 201)
	providerID := created["id"].(string)
	path := "/api/v1/providers/" + providerID
	enrollment := startGrantEnrollment(t, h, owner, path)
	device := enrollment["device"].(map[string]any)
	if device["verification_url"] != "https://auth.openai.com/codex/device" || device["user_code"] != "ABCD-EFGH" {
		t.Fatal("wrong official device enrollment instructions")
	}
	pollDue(t, h, enrollment)
	wantStatus(t, pollGrantEnrollment(h, owner, path, enrollment, 200), "pending")
	authority.approved.Store(true)
	pollDue(t, h, enrollment)
	status := pollGrantEnrollment(h, owner, path, enrollment, 200)
	wantStatus(t, status, "completed")
	completion := status["completion"].(map[string]any)
	credentialID := completion["credential_id"].(string)
	principal := completion["principal"].(string)
	encoded, _ := json.Marshal(status)
	if strings.Contains(string(encoded), "fixture-refresh") || strings.Contains(string(encoded), "fixture-signature") {
		t.Fatal("upstream tokens escaped through management API")
	}
	accountResponse := h.want(owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": project, "provider_id": providerID, "credential_id": credentialID, "name": "Codex account", "enabled": true, "models": []string{"gpt-5.3-codex"}}, idem(uuid.NewString()), 201)
	if accountResponse["health"] != "unknown" || accountResponse["principal"] != principal || accountResponse["grant_state"] != "current" {
		t.Fatal("enrollment was mislabeled as inference health or lost identity")
	}
	var account codemode.Account
	encoded, _ = json.Marshal(accountResponse)
	if err := json.Unmarshal(encoded, &account); err != nil {
		t.Fatal(err)
	}
	var cfg runtime.Configuration
	encoded, _ = json.Marshal(configuration)
	if err := json.Unmarshal(encoded, &cfg); err != nil {
		t.Fatal(err)
	}
	worker := grantRefresher(t, h)
	authorizer := &providers.CodeAuthorizer{Pool: h.Pool, Credentials: h.Runtime, Plugins: worker.Plugins}
	h.refresh()
	auth, err := authorizer.AuthorizeCode(t.Context(), cfg, account)
	if err != nil || auth.Principal != principal || auth.Headers.Get("ChatGPT-Account-ID") != "fixture-account" || len(auth.Headers) != 2 {
		t.Fatalf("enrolled authorization: %v", err)
	}
	before := auth.Headers.Get("Authorization")
	first := readGrant(t, h, credentialID)
	if first.generation != 1 || first.refreshToken == nil || first.expires == nil {
		t.Fatal("grant did not enter encrypted refresh lifecycle")
	}
	var exposed bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.secrets WHERE position($1::bytea in ciphertext)>0)`, []byte("fixture-refresh")).Scan(&exposed); err != nil || exposed {
		t.Fatalf("unencrypted refresh material: %v", err)
	}
	if pass(t, worker) {
		t.Fatal("worker refreshed before the observed token expiry required it")
	}
	dueNow(t, h, credentialID)
	if !pass(t, worker) {
		t.Fatal("due refresh did not run")
	}
	second := readGrant(t, h, credentialID)
	if second.generation != 2 || second.refreshToken == nil || second.refreshTokens != 1 || second.lapsed != nil {
		t.Fatalf("refresh rotation: generation=%d lapsed=%v failures=%d reason=%v", second.generation, second.lapsed, second.failures, second.failure)
	}
	refresh, err := h.Server.Keys.Read(t.Context(), h.Pool, h.Server.Installation, *second.refreshToken, secrets.ProviderGrantRefresh)
	if err != nil || string(refresh) != "fixture-refresh-1" {
		t.Fatalf("new refresh token not encrypted under refresh purpose: %v", err)
	}
	h.refresh()
	auth, err = authorizer.AuthorizeCode(t.Context(), cfg, account)
	if err != nil || auth.Principal != principal || auth.Headers.Get("Authorization") == before {
		t.Fatalf("refreshed authorization: %v", err)
	}

	detail := h.want(owner, "GET", path, nil, nil, 200)
	probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 200)
	if probe["succeeded"] == true {
		t.Fatal("ordinary synthetic probe bypassed the code-mode fence")
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem(uuid.NewString())), 422)

	authority.changedAccount.Store(true)
	dueNow(t, h, credentialID)
	if !pass(t, worker) {
		t.Fatal("changed-account refresh did not run")
	}
	lapsed := readGrant(t, h, credentialID)
	if lapsed.lapsed == nil || lapsed.generation != 2 {
		t.Fatal("changed principal replaced authorization instead of lapsing")
	}
	if _, err := authorizer.AuthorizeCode(t.Context(), cfg, account); err == nil {
		t.Fatal("lapsed account remained authorized")
	}
	accounts := h.want(owner, "GET", "/api/v1/code/accounts?project_id="+project, nil, nil, 200)["items"].([]any)
	if len(accounts) != 1 || accounts[0].(map[string]any)["principal"] != principal || accounts[0].(map[string]any)["grant_state"] != "lapsed" || accounts[0].(map[string]any)["eligible"] != false {
		t.Fatal("management lost stable identity or live grant eligibility")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if len(authority.calls) != 6 || authority.refreshes != 2 {
		t.Fatalf("unexpected setup/maintenance dispatch count: %v", authority.calls)
	}
}
