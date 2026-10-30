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
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/routing"
	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/claudeauth"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/providers"
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
	omitRotation   atomic.Bool
	mu             sync.Mutex
	calls          []string
	refreshes      int
	refreshToken   int
	accessToken    string
}

func newCodexAuthority(t *testing.T) *codexAuthority {
	return newCodexAuthorityWithPeer(t, "fixture-account", nil)
}

func newCodexAuthorityWithPeer(t *testing.T, accountID string, peer http.Handler) *codexAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Codex fixture authority"},
		DNSNames: []string{"auth.openai.com", "chatgpt.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	f := &codexAuthority{roots: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "chatgpt.com" && peer != nil {
			peer.ServeHTTP(w, r)
			return
		}
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
				if json.NewDecoder(r.Body).Decode(&request) != nil || r.Header.Get("Content-Type") != "application/json" || request["grant_type"] != "refresh_token" || request["client_id"] != "app_EMoamEEZ73f0CkXaXp7hrann" || request["refresh_token"] != fmt.Sprintf("fixture-refresh-%d", f.refreshToken) {
					t.Error("worker reused an old token or changed the refresh protocol")
					w.WriteHeader(401)
					return
				}
				f.refreshes++
				if !f.omitRotation.Load() {
					f.refreshToken++
				}
			}
			account := accountID
			if f.changedAccount.Load() {
				account = "another-account"
			}
			payload := fmt.Sprintf(`{"exp":%d,"jti":"generation-%d","https://api.openai.com/auth":{"chatgpt_account_id":%q,"chatgpt_user_id":"fixture-user"}}`, time.Now().Add(time.Hour).Unix(), f.refreshes, account)
			f.accessToken = "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".fixture-signature"
			if f.omitRotation.Load() {
				json.NewEncoder(w).Encode(map[string]string{"access_token": f.accessToken, "id_token": f.accessToken})
				return
			}
			json.NewEncoder(w).Encode(map[string]string{"access_token": f.accessToken, "id_token": f.accessToken, "refresh_token": fmt.Sprintf("fixture-refresh-%d", f.refreshToken)})
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

var codexDispatch = codemode.Dispatch{ClientPath: "responses", Adapter: codemode.AdapterCodex, Protocol: codemode.ProtocolResponses}

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
	var cfg routing.Configuration
	encoded, _ = json.Marshal(configuration)
	if err := json.Unmarshal(encoded, &cfg); err != nil {
		t.Fatal(err)
	}
	worker := grantRefresher(t, h)
	authorizer := &providers.CodeAuthorizer{Pool: h.Pool, Credentials: h.Runtime, Plugins: worker.Plugins}
	h.refresh()
	auth, err := authorizer.AuthorizeCode(t.Context(), cfg, account, codexDispatch)
	if err != nil || auth.Principal != principal || auth.CredentialID != credentialID || auth.GrantGeneration != 1 || auth.Headers.Get("ChatGPT-Account-ID") != "fixture-account" || len(auth.Headers) != 2 {
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
	if h.Runtime.GrantGeneration(credentialID) != 0 {
		t.Fatal("code-only credential unexpectedly belongs to the ordinary grant poll")
	}
	h.Runtime.CredentialRefused(auth.CredentialID, auth.GrantGeneration)
	deadline := time.Now().Add(5 * time.Second)
	for {
		grant := readGrant(t, h, credentialID)
		if grant.refresh != nil && !grant.refresh.After(time.Now()) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("authentication refusal did not request early code-only grant refresh")
		}
		time.Sleep(10 * time.Millisecond)
	}
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
	auth, err = authorizer.AuthorizeCode(t.Context(), cfg, account, codexDispatch)
	if err != nil || auth.Principal != principal || auth.GrantGeneration != 2 || auth.Headers.Get("Authorization") == before {
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
	if _, err := authorizer.AuthorizeCode(t.Context(), cfg, account, codexDispatch); err == nil {
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

func codexImportDocument(t *testing.T, account, user, refresh string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"exp": time.Now().Add(-time.Hour).Unix(), "https://api.openai.com/auth": map[string]string{"chatgpt_account_id": account, "chatgpt_user_id": user}})
	if err != nil {
		t.Fatal(err)
	}
	token := "header." + base64.RawURLEncoding.EncodeToString(payload) + ".fixture-signature"
	document, err := json.Marshal(map[string]any{"auth_mode": "chatgpt", "OPENAI_API_KEY": nil, "tokens": map[string]string{"access_token": token, "id_token": token, "refresh_token": refresh, "account_id": account}})
	if err != nil {
		t.Fatal(err)
	}
	return string(document)
}

func TestCodeAuthImportPreservesPinnedAccountAndOwnsRefresh(t *testing.T) {
	codeAuthImportContinuity(t, true)
}

func TestCodeAuthImportRetainsTokenWhenIssuerDoesNotRotate(t *testing.T) {
	codeAuthImportContinuity(t, false)
}

func codeAuthImportContinuity(t *testing.T, rotate bool) {
	t.Helper()
	f := newCodePublicFixtureWithPeer(t, codexfixture.New())
	f.codexAuthorities[0].omitRotation.Store(!rotate)
	h, owner := f.h, f.owner
	account := f.accounts[0]
	providerPath := "/api/v1/providers/" + account["provider_id"].(string)
	poolInput := map[string]any{"project_id": f.project, "name": "Controlled import pool", "kind": "shared", "owner_user_id": nil, "account_ids": []string{account["id"].(string)}, "api_key_ids": []string{f.keyID}}
	h.want(owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), poolInput, etagHeader(f.pool), 200)
	worker := grantRefresher(t, h)
	h.Gateway.CodeLedger = &resources.CodeStore{Pool: h.Pool}
	h.Gateway.CodeAuthorizer = &providers.CodeAuthorizer{Pool: h.Pool, Credentials: h.Runtime, Plugins: worker.Plugins}
	h.refresh()
	mux := http.NewServeMux()
	h.Gateway.RegisterCode(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	request := func() codexfixture.Request {
		t.Helper()
		r, err := http.NewRequestWithContext(t.Context(), "POST", server.URL+"/code/qualification/responses", strings.NewReader(`{"model":"gpt-5.4","stream":true,"input":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+f.key)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Thread-Id", "import-pinned-account")
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil || response.StatusCode != 200 || string(body) != codexfixture.SSE {
			t.Fatalf("imported account request failed with status %d", response.StatusCode)
		}
		captured := f.peer.Requests()
		return captured[len(captured)-1]
	}
	before := request()
	provider := h.want(owner, "GET", providerPath, nil, nil, 200)
	document := codexImportDocument(t, "controlled-0", "fixture-user", "fixture-refresh-0")
	slot := h.want(owner, "GET", providerPath+"/credential-slots", nil, nil, 200)["items"].([]any)[0].(map[string]any)
	imported := h.want(owner, "POST", providerPath+"/codex-import", map[string]any{"credentials": document, "slot_id": strings.ToUpper(slot["id"].(string))}, etagHeader(provider), 201)
	encoded, err := json.Marshal(imported)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "fixture-refresh") || strings.Contains(string(encoded), "fixture-signature") || imported["principal"] != account["principal"] {
		t.Fatal("import exposed credentials or changed the verified principal")
	}
	if len(f.peer.Requests()) != 1 {
		t.Fatal("import generated inference")
	}
	rotated := h.want(owner, "PUT", "/api/v1/code/accounts/"+account["id"].(string), map[string]any{"project_id": f.project, "provider_id": account["provider_id"], "credential_id": imported["credential_id"], "name": account["name"], "enabled": true, "models": account["models"]}, etagHeader(account), 200)
	if rotated["principal"] != account["principal"] || rotated["health"] != "unknown" {
		t.Fatal("import changed the account identity or certified inference")
	}
	h.refresh()
	after := request()
	if after.Header.Get("ChatGPT-Account-ID") != before.Header.Get("ChatGPT-Account-ID") || after.Header.Get("Authorization") == before.Header.Get("Authorization") {
		t.Fatal("import did not retain the pinned account with a verified replacement grant")
	}
	dueNow(t, h, imported["credential_id"].(string))
	if !pass(t, worker) {
		t.Fatal("imported grant did not enter the refresh lifecycle")
	}
	h.refresh()
	refreshed := request()
	if refreshed.Header.Get("Authorization") == after.Header.Get("Authorization") || refreshed.Header.Get("ChatGPT-Account-ID") != before.Header.Get("ChatGPT-Account-ID") {
		t.Fatal("OLP did not rotate the imported grant for the pinned account")
	}
	f.codexAuthorities[0].changedAccount.Store(true)
	latest := h.want(owner, "GET", providerPath, nil, nil, 200)
	f.codexAuthorities[0].mu.Lock()
	refreshToken := f.codexAuthorities[0].refreshToken
	f.codexAuthorities[0].mu.Unlock()
	changed := codexImportDocument(t, "controlled-0", "fixture-user", fmt.Sprintf("fixture-refresh-%d", refreshToken))
	rejected := h.want(owner, "POST", providerPath+"/codex-import", map[string]any{"credentials": changed}, etagHeader(latest), 422)
	rejection, err := json.Marshal(rejected)
	if err != nil || strings.Contains(string(rejection), "fixture-refresh") || strings.Contains(string(rejection), "fixture-signature") {
		t.Fatal("failed identity verification exposed credentials")
	}
	unchanged := h.want(owner, "GET", providerPath, nil, nil, 200)
	if unchanged["etag"] != latest["etag"] {
		t.Fatal("changed identity modified the provider")
	}
	request()
	bindings := f.list("bindings")
	if len(bindings) != 1 || bindings[0].(map[string]any)["account_id"] != account["id"] {
		t.Fatal("import discarded or changed the conversation pin")
	}
}

func TestCodeAuthImportRejectsMalformedConflictingAndChangedAccount(t *testing.T) {
	f := newCodePublicFixtureWithPeer(t, codexfixture.New())
	h, owner := f.h, f.owner
	account := f.accounts[0]
	path := "/api/v1/providers/" + account["provider_id"].(string)
	provider := h.want(owner, "GET", path, nil, nil, 200)
	valid := codexImportDocument(t, "controlled-0", "fixture-user", "fixture-refresh-0")
	for name, document := range map[string]string{
		"malformed":           "{",
		"missing refresh":     strings.Replace(valid, `"refresh_token":"fixture-refresh-0"`, `"refresh_token":""`, 1),
		"conflicting account": strings.Replace(valid, `"account_id":"controlled-0"`, `"account_id":"different-account"`, 1),
		"API key conflict":    strings.Replace(valid, `"OPENAI_API_KEY":null`, `"OPENAI_API_KEY":"fixture-api-key"`, 1),
		"duplicate fields":    strings.Replace(valid, `"auth_mode":"chatgpt"`, `"auth_mode":"chatgpt","auth_mode":"chatgpt"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			h.want(owner, "POST", path+"/codex-import", map[string]any{"credentials": document}, etagHeader(provider), 422)
		})
	}
	unknown := h.want(owner, "POST", path+"/codex-import", map[string]any{"credentials": valid, "slot_id": uuid.NewString()}, etagHeader(provider), 422)
	requireFieldError(t, unknown, "slot_id")
	f.noSyntheticInference(t)
	f.codexAuthorities[0].mu.Lock()
	refreshes := f.codexAuthorities[0].refreshes
	f.codexAuthorities[0].mu.Unlock()
	if refreshes != 0 {
		t.Fatal("malformed import reached the token issuer")
	}
	f.codexAuthorities[0].changedAccount.Store(true)
	other := codexImportDocument(t, "another-account", "fixture-user", "fixture-refresh-0")
	h.want(owner, "POST", path+"/codex-import", map[string]any{"credentials": other}, etagHeader(provider), 422)
	unchanged := h.want(owner, "GET", path, nil, nil, 200)
	if unchanged["etag"] != provider["etag"] {
		t.Fatal("rejected import modified the connection")
	}
	accounts := f.list("accounts")
	for _, value := range accounts {
		current := value.(map[string]any)
		if current["id"] == account["id"] && (current["principal"] != account["principal"] || current["credential_id"] != account["credential_id"]) {
			t.Fatal("rejected import changed the existing account")
		}
	}
	f.noSyntheticInference(t)
}

func TestCodeAuthImportRejectsSameNamedForeignPluginBeforeRefresh(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	var received atomic.Int64
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	digest := installPlugin(t, h, owner, testutil.BuildPlugin(t, "./internal/plugins/testdata/fixture", "-X=main.behaviour=codex-import", "-X=main.upstream="+receiver.URL+"/v1"))
	project := createProject(h, owner, "Import trust boundary")
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Foreign Codex profile", "project_id": project, "model": "fixture-model",
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "grant", "profile_id": "codex-subscription", "profile_revision": digest}}, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + provider["id"].(string)
	document := codexImportDocument(t, "fixture-account", "fixture-user", "fixture-refresh-0")
	h.want(owner, "POST", path+"/codex-import", map[string]any{"credentials": document}, etagHeader(provider), 422)
	if received.Load() != 0 {
		t.Fatal("a same-named foreign plugin received imported refresh credentials")
	}
	unchanged := h.want(owner, "GET", path, nil, nil, 200)
	if unchanged["etag"] != provider["etag"] {
		t.Fatal("a rejected foreign plugin import modified the provider")
	}
	credentials := h.want(owner, "GET", path+"/credentials", nil, nil, 200)["items"].([]any)
	if len(credentials) != 0 {
		t.Fatal("a rejected foreign plugin import staged a credential")
	}
}

func TestCodeAuthImportRejectsInvalidPluginTokenMaterial(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	digest := installPlugin(t, h, owner, testutil.BuildPlugin(t, "./internal/plugins/testdata/fixture", "-X=main.behaviour=codex-import-invalid"))
	project := createProject(h, owner, "Import grant validation")
	provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Invalid import material", "project_id": project, "model": "fixture-model",
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "grant", "profile_id": "codex-subscription", "profile_revision": digest}}, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + provider["id"].(string)
	for _, material := range []string{"empty-access", "unsafe-access", "oversized-refresh", "excessive-expiry"} {
		t.Run(material, func(t *testing.T) {
			document := codexImportDocument(t, "fixture-account", "fixture-user", material)
			rejected := h.want(owner, "POST", path+"/codex-import", map[string]any{"credentials": document}, etagHeader(provider), 422)
			requireFieldError(t, rejected, "credentials")
		})
	}
	unchanged := h.want(owner, "GET", path, nil, nil, 200)
	if unchanged["etag"] != provider["etag"] {
		t.Fatal("invalid plugin token material modified the provider")
	}
	credentials := h.want(owner, "GET", path+"/credentials", nil, nil, 200)["items"].([]any)
	if len(credentials) != 0 {
		t.Fatal("invalid plugin token material was staged as a credential")
	}
}

type claudeAuthority struct {
	proxy *httptest.Server
	roots string
}

func newClaudeAuthority(t *testing.T, handler http.Handler) claudeAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Claude fixture authority"},
		DNSNames: []string{"platform.claude.com", "api.anthropic.com"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(proxy.Close)
	return claudeAuthority{proxy: proxy, roots: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func TestClaudeHeadlessEnrollmentRefreshAndCredentialRefusal(t *testing.T) {
	for _, refusal := range []string{"changed identity", "revocation"} {
		t.Run(refusal, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			var refreshed atomic.Bool
			var changed atomic.Bool
			authority := newClaudeAuthority(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == "POST" && r.Host == "platform.claude.com" && r.URL.Path == "/v1/oauth/token":
					var request struct {
						GrantType    string `json:"grant_type"`
						Code         string `json:"code"`
						Verifier     string `json:"code_verifier"`
						State        string `json:"state"`
						RefreshToken string `json:"refresh_token"`
					}
					if json.NewDecoder(r.Body).Decode(&request) != nil {
						t.Error("invalid OAuth request")
						w.WriteHeader(400)
						return
					}
					if request.GrantType == "refresh_token" {
						if request.RefreshToken != "fixture-claude-refresh" {
							t.Error("encrypted refresh identity was lost")
						}
						refreshed.Store(true)
						io.WriteString(w, `{"access_token":"fixture-claude-rotated","refresh_token":"fixture-claude-refresh","token_type":"bearer","expires_in":3600}`)
						return
					}
					if request.GrantType != "authorization_code" || request.Code != "fixture-approved" || request.Verifier == "" || request.State == "" {
						t.Error("authorization session was not bound to the code")
					}
					io.WriteString(w, `{"access_token":"fixture-claude-access","refresh_token":"fixture-claude-refresh","token_type":"bearer","expires_in":3600}`)
				case r.Method == "GET" && r.Host == "api.anthropic.com" && r.URL.Path == "/api/oauth/profile":
					expected := "Bearer fixture-claude-access"
					if refreshed.Load() {
						expected = "Bearer fixture-claude-rotated"
					}
					if r.Header.Get("Authorization") != expected {
						t.Error("identity was not verified with the issued access token")
					}
					account := "11111111-1111-4111-8111-111111111111"
					if changed.Load() {
						account = "33333333-3333-4333-8333-333333333333"
					}
					fmt.Fprintf(w, `{"account":{"uuid":%q},"organization":{"uuid":"22222222-2222-4222-8222-222222222222"}}`, account)
				default:
					t.Errorf("enrollment or maintenance attempted an unrelated operation %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			digest := installPlugin(t, h, owner, testutil.BuildPlugin(t, "./plugins/claude"))
			project := createProject(h, owner, "Claude fixture accounts")
			configuration := map[string]any{"kind": "plugin", "auth_mode": "grant", "profile_id": claudeauth.ProfileID, "profile_revision": digest,
				"options": map[string]any{"network": map[string]any{"proxy_url": authority.proxy.URL, "trust_roots_pem": authority.roots}}}
			provider := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Claude fixture", "project_id": project, "model": "claude-fixture", "configuration": configuration}, idem(uuid.NewString()), 201)
			path := "/api/v1/providers/" + provider["id"].(string)
			enrollment := startGrantEnrollment(t, h, owner, path)
			authorization, err := url.Parse(enrollment["authorization_url"].(string))
			if err != nil {
				t.Fatal(err)
			}
			completion := continueGrantEnrollment(h, owner, path, enrollment, "fixture-approved#"+authorization.Query().Get("state"), 201)
			principal := completion["principal"].(string)
			credential := completion["credential_id"].(string)
			accountResponse := h.want(owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": project, "provider_id": provider["id"], "credential_id": credential, "name": "Fixture only", "enabled": true, "models": []string{"claude-fixture"}}, idem(uuid.NewString()), 201)
			if accountResponse["health"] != "unknown" || accountResponse["principal"] != principal || accountResponse["adapter"] != "claude" {
				t.Fatal("enrollment fabricated health or lost verified metadata")
			}
			var account codemode.Account
			encoded, err := json.Marshal(accountResponse)
			if err != nil || json.Unmarshal(encoded, &account) != nil {
				t.Fatal("invalid account response")
			}
			var configurationValue routing.Configuration
			encodedConfiguration, err := json.Marshal(configuration)
			if err != nil || json.Unmarshal(encodedConfiguration, &configurationValue) != nil {
				t.Fatal("invalid configuration")
			}
			worker := grantRefresher(t, h)
			authorizer := providers.CodeAuthorizer{Pool: h.Pool, Credentials: h.Runtime, Plugins: worker.Plugins}
			dispatch := codemode.Dispatch{ClientPath: "v1/messages", Adapter: codemode.AdapterClaude, Protocol: codemode.ProtocolMessages}
			h.refresh()
			before, err := authorizer.AuthorizeCode(t.Context(), configurationValue, account, dispatch)
			if err != nil || before.Principal != principal || before.Headers.Get("Authorization") != "Bearer fixture-claude-access" {
				t.Fatalf("enrolled grant refused %v", err)
			}
			first := readGrant(t, h, credential)
			if first.refreshToken == nil {
				t.Fatal("refresh material was not stored separately")
			}
			refresh, err := h.Server.Keys.Read(t.Context(), h.Pool, h.Server.Installation, *first.refreshToken, secrets.ProviderGrantRefresh)
			if err != nil || string(refresh) != "fixture-claude-refresh" {
				t.Fatal("refresh material did not use encrypted grant storage")
			}

			if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.provider_grants SET expires_at=now()-interval '1 minute' WHERE credential_id=$1`, credential); err != nil {
				t.Fatal(err)
			}
			if _, err := authorizer.AuthorizeCode(t.Context(), configurationValue, account, dispatch); err == nil {
				t.Fatal("expired access material remained authorized")
			}
			dueNow(t, h, credential)
			if !pass(t, worker) {
				t.Fatal("due refresh did not execute")
			}
			h.refresh()
			after, err := authorizer.AuthorizeCode(t.Context(), configurationValue, account, dispatch)
			if err != nil || after.Principal != before.Principal || after.GrantGeneration <= before.GrantGeneration || after.Headers.Get("Authorization") == before.Headers.Get("Authorization") {
				t.Fatal("refresh lost identity or kept expired access material")
			}
			detail := h.want(owner, "GET", path, nil, nil, 200)
			probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 200)
			if probe["succeeded"] == true {
				t.Fatal("ordinary probe bypassed the code-mode boundary")
			}

			if refusal == "revocation" {
				current := h.want(owner, "GET", path, nil, nil, 200)
				h.want(owner, "POST", path+"/credentials/"+credential+"/revoke", nil, withMatch(current, idem(uuid.NewString())), 200)
				h.refresh()
				if _, err := authorizer.AuthorizeCode(t.Context(), configurationValue, account, dispatch); err == nil {
					t.Fatal("revoked credential remained authorized")
				}
				return
			}

			changed.Store(true)
			dueNow(t, h, credential)
			if !pass(t, worker) {
				t.Fatal("changed identity refresh did not execute")
			}
			if _, err := authorizer.AuthorizeCode(t.Context(), configurationValue, account, dispatch); err == nil {
				t.Fatal("changed identity remained authorized")
			}
			accounts := h.want(owner, "GET", "/api/v1/code/accounts?project_id="+project, nil, nil, 200)["items"].([]any)
			if accounts[0].(map[string]any)["principal"] != principal || accounts[0].(map[string]any)["eligible"] != false {
				t.Fatal("management lost stable identity or lapse eligibility")
			}

		})
	}
}
