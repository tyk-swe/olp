//go:build integration

package integration_test

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/testutil"
)

// fixedHostPeer is a controlled TLS peer for an upstream's fixed hosts,
// reached through the CONNECT proxy a provider's network options name, with a
// certificate only the provider's trust roots accept. No test build overrides
// or production authentication shortcuts.
type fixedHostPeer struct {
	proxy    *httptest.Server
	roots    string
	mu       sync.Mutex
	requests []peerRequest
	respond  func(w http.ResponseWriter, r *http.Request)
}

type peerRequest struct {
	Host, Path, RawQuery string
	Header               http.Header
	Body                 []byte
}

func newFixedHostPeer(t *testing.T, hosts ...string) *fixedHostPeer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Coding plan fixture peer"},
		DNSNames: hosts, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := &fixedHostPeer{roots: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), respond: codingPlanResponse}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.requests = append(p.requests, peerRequest{Host: r.Host, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone(), Body: body})
		respond := p.respond
		p.mu.Unlock()
		r.Body = io.NopCloser(bytes.NewReader(body))
		respond(w, r)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	server.StartTLS()
	t.Cleanup(server.Close)
	p.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	t.Cleanup(p.proxy.Close)
	return p
}

func (p *fixedHostPeer) received() []peerRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]peerRequest(nil), p.requests...)
}

const (
	codingPlanMessagesSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":8,\"output_tokens\":1}}}\n\n" +
		"event: ping\ndata: {\"type\": \"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"CONTROLLED_ANSWER\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	codingPlanChatSSE = "data: {\"id\":\"chat_fixture\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"CONTROLLED_ANSWER\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"chat_fixture\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}\n\n" +
		"data: [DONE]\n\n"
	codingPlanResponsesSSE = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_fixture\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n"
)

func codingPlanResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	switch {
	case strings.HasSuffix(r.URL.Path, "/messages"):
		io.WriteString(w, codingPlanMessagesSSE)
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		io.WriteString(w, codingPlanChatSSE)
	case strings.HasSuffix(r.URL.Path, "/responses"):
		io.WriteString(w, codingPlanResponsesSSE)
	default:
		w.WriteHeader(404)
	}
}

// codingPlanFixture enrolls pasted keys through a coding-plan plugin's public
// enrollment, and serves the published route "qualification" from an
// in-process gateway with the real ledger and authorizer.
type codingPlanFixture struct {
	h                   *accessHarness
	owner               *browser
	peer                *fixedHostPeer
	digest, profile     string
	project, keyID, key string
	providers, keys     []string
	accounts            []map[string]any
	pool, route         map[string]any
	gateway             *httptest.Server
}

func newCodingPlanFixture(t *testing.T, plugin, profile string, keys ...string) *codingPlanFixture {
	t.Helper()
	h := newAccessHarness(t)
	owner := h.owner()
	f := &codingPlanFixture{h: h, owner: owner, profile: profile, keys: keys, peer: newFixedHostPeer(t, "api.z.ai", "open.bigmodel.cn", "opencode.ai")}
	f.project = createProject(h, owner, "Coding plan qualification")
	f.digest = installPlugin(t, h, owner, testutil.BuildPlugin(t, "./plugins/"+plugin))
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Code key", "project_id": f.project, "scopes": []string{"inference"}, "allowed_routes": []string{"qualification"}}, idem("code-key"), 201)
	f.keyID, f.key = key["id"].(string), key["secret"].(string)
	for i, vendorKey := range keys {
		provider := f.provider(t, fmt.Sprintf("Coding plan %d", i), profile)
		f.providers = append(f.providers, provider)
		credential := f.enroll(t, provider, vendorKey)
		f.accounts = append(f.accounts, h.want(owner, "POST", "/api/v1/code/accounts", map[string]any{
			"project_id": f.project, "provider_id": provider, "credential_id": credential, "name": "Controlled fixture only", "enabled": true, "models": []string{"glm-5.3", "minimax-m3"},
		}, idem(fmt.Sprintf("code-account-%d", i)), 201))
	}
	f.pool = h.want(owner, "POST", "/api/v1/code/pools", f.poolInput(f.accounts...), idem("code-pool"), 201)
	draft := h.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": []string{"glm-5.3", "minimax-m3"}, "enabled": true}, idem("code-route"), 201)
	f.route = h.want(owner, "POST", "/api/v1/code/routes/"+draft["id"].(string)+"/publish", nil, withMatch(draft, idem("code-publish")), 200)
	if n := len(f.peer.received()); n != 0 {
		t.Fatalf("enrollment and publication sent %d upstream requests", n)
	}
	h.refresh()
	h.Gateway.CodeLedger = &resources.CodeStore{Pool: h.Pool}
	h.Gateway.CodeAuthorizer = &providers.CodeAuthorizer{Pool: h.Pool, Credentials: h.Runtime, Plugins: grantRefresher(t, h).Plugins}
	mux := http.NewServeMux()
	h.Gateway.RegisterCode(mux)
	f.gateway = httptest.NewServer(mux)
	t.Cleanup(f.gateway.Close)
	return f
}

// mix adds an OpenCode Go account serving minimax-m3 and kimi-k3 to the GLM
// fixture's pool, narrows the GLM account to glm-5.3, and republishes the route
// with all three models, so each model has exactly one subscription.
func (f *codingPlanFixture) mix(t *testing.T, key string) (glm, other map[string]any) {
	t.Helper()
	h, owner := f.h, f.owner
	glm = f.accounts[0]
	glm = h.want(owner, "PUT", "/api/v1/code/accounts/"+glm["id"].(string), map[string]any{"project_id": f.project, "provider_id": glm["provider_id"], "credential_id": glm["credential_id"], "name": glm["name"], "enabled": true, "models": []string{"glm-5.3"}}, etagHeader(glm), 200)
	f.digest = installPlugin(t, h, owner, testutil.BuildPlugin(t, "./plugins/opencode-go"))
	provider := f.provider(t, "OpenCode Go", codeplans.OpenCodeGoProfile)
	credential := f.enroll(t, provider, key)
	other = h.want(owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": f.project, "provider_id": provider, "credential_id": credential, "name": "Other plan", "enabled": true, "models": []string{"minimax-m3", "kimi-k3"}}, idem("other-account"), 201)
	if other["adapter"] != "opencode_go" {
		t.Fatalf("account adapter: %v", other)
	}
	f.providers, f.accounts = append(f.providers, provider), []map[string]any{glm, other}
	f.pool = h.want(owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), f.poolInput(glm, other), etagHeader(f.pool), 200)
	id := f.route["id"].(string)
	draft := h.want(owner, "PUT", "/api/v1/code/routes/"+id, map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": []string{"glm-5.3", "minimax-m3", "kimi-k3"}, "enabled": true}, etagHeader(f.route), 200)
	f.route = h.want(owner, "POST", "/api/v1/code/routes/"+id+"/publish", nil, withMatch(draft, idem("mixed-publish")), 200)
	h.refresh()
	return glm, other
}

func (f *codingPlanFixture) provider(t *testing.T, name, profile string) string {
	t.Helper()
	provider := f.h.want(f.owner, "POST", "/api/v1/providers", map[string]any{"name": name, "project_id": f.project, "model": vendorModel, "configuration": map[string]any{
		"kind": "plugin", "auth_mode": "grant", "profile_id": profile, "profile_revision": f.digest,
		"options": map[string]any{"network": map[string]any{"proxy_url": f.peer.proxy.URL, "trust_roots_pem": f.peer.roots}},
	}}, idem("provider-"+name), 201)
	return provider["id"].(string)
}

// enroll pastes key into a provider's pasted-key enrollment and returns the
// credential version it becomes.
func (f *codingPlanFixture) enroll(t *testing.T, provider, key string) string {
	t.Helper()
	path := "/api/v1/providers/" + provider
	enrollment := startGrantEnrollment(t, f.h, f.owner, path)
	pages := map[string]string{codeplans.ZAIProfile: codeplans.ZAIKeyPage, codeplans.BigModelProfile: codeplans.BigModelKeyPage, codeplans.OpenCodeGoProfile: codeplans.OpenCodeGoKeyPage}
	if enrollment["input"] != "secret" || enrollment["authorization_url"] != pages[f.profileOf(t, provider)] || enrollment["device"] != nil {
		t.Fatalf("pasted-key enrollment: %v", enrollment)
	}
	completed := continueGrantEnrollment(f.h, f.owner, path, enrollment, key, 201)
	if completed["principal"] != codeplans.Principal(f.profileOf(t, provider), key) {
		t.Fatalf("principal %v is not the key's fingerprint", completed["principal"])
	}
	return completed["credential_id"].(string)
}

func (f *codingPlanFixture) profileOf(t *testing.T, provider string) string {
	t.Helper()
	detail := f.h.want(f.owner, "GET", "/api/v1/providers/"+provider, nil, nil, 200)
	return detail["configuration"].(map[string]any)["profile_id"].(string)
}

func (f *codingPlanFixture) poolInput(accounts ...map[string]any) map[string]any {
	ids := []string{}
	for _, account := range accounts {
		ids = append(ids, account["id"].(string))
	}
	return map[string]any{"project_id": f.project, "name": "Controlled shared pool", "kind": "shared", "owner_user_id": nil, "account_ids": ids, "api_key_ids": []string{f.keyID}}
}

func (f *codingPlanFixture) post(t *testing.T, path string, header http.Header, body string) (*http.Response, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), "POST", f.gateway.URL+"/code/qualification/"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header = header.Clone()
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	out, _ := io.ReadAll(response.Body)
	return response, out
}

// settled waits until n attempts are no longer prepared or in flight.
func (f *codingPlanFixture) settled(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		items := f.h.want(f.owner, "GET", "/api/v1/code/attempts?project_id="+f.project, nil, nil, 200)["items"].([]any)
		done := []map[string]any{}
		for _, item := range items {
			if attempt := item.(map[string]any); attempt["finished_at"] != nil {
				done = append(done, attempt)
			}
		}
		if len(done) >= n {
			return done
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d attempts settled", len(done), n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestCodeCodingPlanKeyEnrollmentStoresOnlyAFingerprintedKey(t *testing.T) {
	key := "0123456789abcdef0123456789abcdef.CONTROLLEDzaiKEY"
	f := newCodingPlanFixture(t, "zai-coding", codeplans.ZAIProfile, key)
	h, owner := f.h, f.owner
	account := f.accounts[0]
	if account["adapter"] != "zai_coding" || account["grant_state"] != "current" || account["eligible"] != true || account["health"] != "unknown" || account["principal"] != codeplans.Principal(codeplans.ZAIProfile, key) {
		t.Fatalf("account: %v", account)
	}
	grant := readGrant(t, h, account["credential_id"].(string))
	if grant.expires != nil || grant.refreshToken != nil || grant.generation != 1 {
		t.Fatalf("pasted key entered the refresh lifecycle: %+v", grant)
	}
	var exposed bool
	if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp.secrets WHERE position($1::bytea in ciphertext)>0)`, []byte(key)).Scan(&exposed); err != nil || exposed {
		t.Fatalf("unencrypted key: %v", err)
	}
	path := "/api/v1/providers/" + f.providers[0]
	for _, read := range []string{"/api/v1/code/accounts?project_id=" + f.project, path, path + "/credentials"} {
		if _, body := h.do(owner, "GET", read, nil, nil); bytes.Contains(body, []byte(key)) {
			t.Fatalf("%s returns the key", read)
		}
	}
	if pass(t, grantRefresher(t, h)) {
		t.Fatal("a pasted key was due a refresh")
	}
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 200); probe["succeeded"] == true {
		t.Fatal("ordinary synthetic probe bypassed the code-mode fence")
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem("activate")), 422)

	// A different key is a different principal, so it can't rotate the account.
	replacement := f.enroll(t, f.providers[0], "fedcba9876543210fedcba9876543210.CONTROLLEDother")
	input := map[string]any{"project_id": f.project, "provider_id": f.providers[0], "credential_id": replacement, "name": "Controlled fixture only", "enabled": true, "models": account["models"]}
	h.want(owner, "PUT", "/api/v1/code/accounts/"+account["id"].(string), input, etagHeader(account), 422)
	if len(f.peer.received()) != 0 {
		t.Fatal("enrollment or management reached the upstream")
	}

	// The real authorizer places the key in the header each protocol's native client uses.
	cfg := runtime.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codeplans.ZAIProfile, ProfileRevision: f.digest, Endpoint: codeplans.ZAIUpstream}
	var a codemode.Account
	encoded, _ := json.Marshal(account)
	if err := json.Unmarshal(encoded, &a); err != nil {
		t.Fatal(err)
	}
	authorizer := f.h.Gateway.CodeAuthorizer
	for _, protocol := range []codemode.Protocol{codemode.ProtocolMessages, codemode.ProtocolChat} {
		auth, err := authorizer.AuthorizeCode(t.Context(), cfg, a, codemode.Dispatch{Adapter: codemode.AdapterZAICoding, Protocol: protocol})
		if err != nil || len(auth.Headers) != 1 || auth.Headers.Get("Authorization") != "Bearer "+key || auth.Principal != a.Principal || auth.GrantGeneration != 1 {
			t.Fatalf("%s authorization: %v", protocol, err)
		}
	}
	for _, dispatch := range []codemode.Dispatch{{Adapter: codemode.AdapterZAICoding, Protocol: codemode.ProtocolResponses}, {Adapter: codemode.AdapterCodex, Protocol: codemode.ProtocolResponses}, {Adapter: codemode.AdapterOpenCodeGo, Protocol: codemode.ProtocolMessages}} {
		if _, err := authorizer.AuthorizeCode(t.Context(), cfg, a, dispatch); err == nil {
			t.Fatalf("%+v authorized", dispatch)
		}
	}
}

func TestCodeCodingPlanForwardsClientTrafficWithTheAccountsKey(t *testing.T) {
	type call struct {
		path, upstreamPath, credential, operation string
		header                                    http.Header
		body, wire                                string
	}
	claude := http.Header{"X-Api-Key": {""}, "Anthropic-Version": {"2023-06-01"}, "Anthropic-Beta": {"fixture-2026-01-01"}, "X-Claude-Code-Session-Id": {"session-1"}, "User-Agent": {"claude-cli/2.1.286 (external, cli)"}}
	opencode := http.Header{"Authorization": {""}, "X-Opencode-Session-Id": {"ses_root"}, "User-Agent": {"opencode/1.18.34"}}
	messages := `{"model":"minimax-m3","max_tokens":256,"stream":true,"messages":[{"role":"user","content":"CONTROLLED_PRIVATE_PROMPT"}]}`
	chat := `{"model":"glm-5.3","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"CONTROLLED_PRIVATE_PROMPT"}]}`
	responses := `{"model":"glm-5.3","stream":true,"store":false,"input":"CONTROLLED_PRIVATE_PROMPT"}`
	for _, test := range []struct {
		plugin, profile, host string
		calls                 []call
	}{
		{"zai-coding", codeplans.ZAIProfile, "api.z.ai", []call{
			{"v1/messages?beta=true", "/api/anthropic/v1/messages", "Authorization", "messages", claude, messages, codingPlanMessagesSSE},
			{"v1/chat/completions", "/api/coding/paas/v4/chat/completions", "Authorization", "chat", opencode, chat, codingPlanChatSSE},
		}},
		{"zai-coding", codeplans.BigModelProfile, "open.bigmodel.cn", []call{
			{"v1/messages?beta=true", "/api/anthropic/v1/messages", "Authorization", "messages", claude, messages, codingPlanMessagesSSE},
		}},
		{"opencode-go", codeplans.OpenCodeGoProfile, "opencode.ai", []call{
			{"v1/messages", "/zen/go/v1/messages", "X-Api-Key", "messages", opencode, messages, codingPlanMessagesSSE},
			{"v1/chat/completions", "/zen/go/v1/chat/completions", "Authorization", "chat", opencode, chat, codingPlanChatSSE},
			{"v1/responses", "/zen/go/v1/responses", "Authorization", "responses", opencode, responses, codingPlanResponsesSSE},
		}},
	} {
		t.Run(test.profile, func(t *testing.T) {
			key := "0123456789abcdef0123456789abcdef." + test.profile
			f := newCodingPlanFixture(t, test.plugin, test.profile, key)
			for i, c := range test.calls {
				header := c.header.Clone()
				for name := range header {
					if header.Get(name) == "" {
						header.Set(name, f.key)
						if name == "Authorization" {
							header.Set(name, "Bearer "+f.key)
						}
					}
				}
				header.Set("X-Olp-Private", "consumed")
				response, got := f.post(t, c.path, header, c.body)
				if response.StatusCode != 200 || string(got) != c.wire {
					t.Fatalf("%s: %d %s", c.path, response.StatusCode, got)
				}
				sent := f.peer.received()[i]
				want := key
				if c.credential == "Authorization" {
					want = "Bearer " + key
				}
				if sent.Host != test.host || sent.Path != c.upstreamPath || string(sent.Body) != c.body || sent.Header.Get(c.credential) != want ||
					len(sent.Header.Values("Authorization"))+len(sent.Header.Values("X-Api-Key")) != 1 || sent.Header.Get("X-Olp-Private") != "" || sent.Header.Get("User-Agent") != c.header.Get("User-Agent") {
					t.Fatalf("%s reached the upstream as %s%s %v", c.path, sent.Host, sent.Path, sent.Header)
				}
				if strings.Contains(fmt.Sprint(sent.Header), f.key) {
					t.Fatal("the OLP key reached the upstream")
				}
				if strings.Contains(c.path, "?") && sent.RawQuery != strings.SplitN(c.path, "?", 2)[1] {
					t.Fatalf("query changed: %s", sent.RawQuery)
				}
			}
			attempts := f.settled(t, len(test.calls))
			operations := map[string]bool{}
			for _, attempt := range attempts {
				operations[attempt["operation"].(string)] = true
				if attempt["reported_tokens"] == nil || attempt["state"] != "settled" {
					t.Fatalf("usage not settled: %v", attempt)
				}
			}
			for _, c := range test.calls {
				if !operations[c.operation] {
					t.Fatalf("operations %v lack %s", operations, c.operation)
				}
			}
		})
	}
}

func TestCodeCodingPlanConversationTreesKeepOneAccount(t *testing.T) {
	f := newCodingPlanFixture(t, "zai-coding", codeplans.ZAIProfile, "0123456789abcdef0123456789abcdef.first", "fedcba9876543210fedcba9876543210.second")
	body := `{"model":"glm-5.3","max_tokens":64,"messages":[]}`
	request := func(agent, parent string) int {
		header := http.Header{"Authorization": {"Bearer " + f.key}, "X-Claude-Code-Session-Id": {"session-tree"}}
		if agent != "" {
			header.Set("X-Claude-Code-Agent-Id", agent)
		}
		if parent != "" {
			header.Set("X-Claude-Code-Parent-Agent-Id", parent)
		}
		response, _ := f.post(t, "v1/messages", header, body)
		return response.StatusCode
	}
	if request("", "") != 200 || request("agent-1", "") != 200 || request("agent-2", "agent-1") != 200 {
		t.Fatal("conversation tree refused")
	}
	if status := request("agent-4", "agent-3"); status != 409 {
		t.Fatalf("unresolved parent agent: %d", status)
	}
	if len(f.peer.received()) != 3 {
		t.Fatal("the unresolved child was dispatched")
	}
	// An OpenCode child session follows its parent session's account too.
	chat := `{"model":"glm-5.3","messages":[]}`
	for _, header := range []http.Header{
		{"Authorization": {"Bearer " + f.key}, "X-Opencode-Session-Id": {"ses_parent"}},
		{"Authorization": {"Bearer " + f.key}, "X-Opencode-Session-Id": {"ses_child"}, "X-Opencode-Parent-Session-Id": {"ses_parent"}},
	} {
		if response, _ := f.post(t, "v1/chat/completions", header, chat); response.StatusCode != 200 {
			t.Fatalf("OpenCode session tree refused: %d", response.StatusCode)
		}
	}
	bindings := f.h.want(f.owner, "GET", "/api/v1/code/bindings?project_id="+f.project, nil, nil, 200)["items"].([]any)
	accounts, roots := map[any]bool{}, map[any]bool{}
	conversations := map[any]string{}
	for _, item := range bindings {
		binding := item.(map[string]any)
		accounts[binding["account_id"]], roots[binding["root_id"]] = true, true
		conversations[binding["conversation"]] = binding["root_id"].(string)
	}
	if len(bindings) != 5 || len(accounts) != 1 || len(roots) != 2 || conversations["session-tree/agent-2"] != conversations["session-tree"] || conversations["ses:5fchild"] != conversations["ses:5fparent"] {
		t.Fatalf("bindings: %v", bindings)
	}
	// Without the client's identity, or with two clients' identities, nothing is dispatched.
	for _, header := range []http.Header{
		{"Authorization": {"Bearer " + f.key}},
		{"Authorization": {"Bearer " + f.key}, "X-Claude-Code-Session-Id": {"s"}, "X-Opencode-Session-Id": {"s"}},
	} {
		if response, _ := f.post(t, "v1/messages", header, body); response.StatusCode != 400 {
			t.Fatalf("anonymous or ambiguous request: %d", response.StatusCode)
		}
	}
	// Codex paths, the WebSocket and unlisted operations refuse before dispatch.
	for _, path := range []string{"responses", "responses/compact", "v1/responses", "v1/models"} {
		if response, _ := f.post(t, path, http.Header{"Authorization": {"Bearer " + f.key}, "X-Claude-Code-Session-Id": {"s"}}, body); response.StatusCode != 400 {
			t.Fatalf("%s: %d", path, response.StatusCode)
		}
	}
	if len(f.peer.received()) != 5 {
		t.Fatal("a refused request reached the upstream")
	}
}

func TestCodeCodingPlanQuotaCoolsTheAccountWithoutFailover(t *testing.T) {
	f := newCodingPlanFixture(t, "zai-coding", codeplans.ZAIProfile, "0123456789abcdef0123456789abcdef.first", "fedcba9876543210fedcba9876543210.second")
	quota := `{"error":{"code":"1308","message":"Usage limit reached for 5 hour."}}`
	f.peer.mu.Lock()
	f.peer.respond = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		io.WriteString(w, quota)
	}
	f.peer.mu.Unlock()
	header := http.Header{"Authorization": {"Bearer " + f.key}, "X-Opencode-Session-Id": {"ses_quota"}}
	body := `{"model":"glm-5.3","messages":[]}`
	response, got := f.post(t, "v1/chat/completions", header, body)
	if response.StatusCode != 429 || string(got) != quota {
		t.Fatalf("quota refusal changed: %d %s", response.StatusCode, got)
	}
	response, got = f.post(t, "v1/chat/completions", header, body)
	if response.StatusCode != 503 || !bytes.Contains(got, []byte("code_account_unavailable")) || len(f.peer.received()) != 1 {
		t.Fatalf("cooling account was retried or failed over: %d %s", response.StatusCode, got)
	}
	account := f.h.want(f.owner, "GET", "/api/v1/code/accounts?project_id="+f.project, nil, nil, 200)["items"].([]any)
	limited := 0
	for _, item := range account {
		if item.(map[string]any)["health"] == "quota_limited" {
			limited++
		}
	}
	if limited != 1 {
		t.Fatalf("health: %v", account)
	}
}

func TestCodeCodingPlanRoutesServeEachModelFromItsSubscription(t *testing.T) {
	glmKey, goKey := "0123456789abcdef0123456789abcdef.first", "opencode0123456789abcdef0123456789"
	f := newCodingPlanFixture(t, "zai-coding", codeplans.ZAIProfile, glmKey)
	h, owner := f.h, f.owner
	if adapters := fmt.Sprint(f.route["adapters"]); adapters != "[zai_coding]" {
		t.Fatalf("route adapters: %s", adapters)
	}
	// An OpenCode Go account joins the GLM pool, and the republished route
	// serves each model from the subscription that lists it.
	glm, other := f.mix(t, goKey)
	id := f.route["id"].(string)
	routes := h.want(owner, "GET", "/api/v1/code/routes?project_id="+f.project, nil, nil, 200)["items"].([]any)
	revisions := h.want(owner, "GET", "/api/v1/code/routes/"+id+"/revisions", nil, nil, 200)["items"].([]any)
	for _, adapters := range []any{f.route["adapters"], routes[0].(map[string]any)["adapters"], revisions[0].(map[string]any)["route"].(map[string]any)["adapters"]} {
		if fmt.Sprint(adapters) != "[opencode_go zai_coding]" {
			t.Fatalf("mixed route adapters: %v", adapters)
		}
	}

	session := func(name, value string) http.Header {
		return http.Header{"Authorization": {"Bearer " + f.key}, name: {value}}
	}
	claude, child, opencode := session("X-Claude-Code-Session-Id", "mix"), session("X-Claude-Code-Session-Id", "mix"), session("X-Opencode-Session-Id", "ses_mix")
	child.Set("X-Claude-Code-Agent-Id", "agent-1")
	bodies := map[string]string{
		"v1/messages":         `{"model":%q,"max_tokens":64,"messages":[]}`,
		"v1/chat/completions": `{"model":%q,"messages":[]}`,
		"v1/responses":        `{"model":%q,"stream":true,"store":false,"input":"CONTROLLED_PRIVATE_PROMPT"}`,
	}
	for i, c := range []struct {
		path                                            string
		header                                          http.Header
		model, host, upstreamPath, credential, expected string
	}{
		{"v1/messages", claude, "glm-5.3", "api.z.ai", "/api/anthropic/v1/messages", "Authorization", "Bearer " + glmKey},
		{"v1/messages", claude, "minimax-m3", "opencode.ai", "/zen/go/v1/messages", "X-Api-Key", goKey},
		{"v1/messages", child, "minimax-m3", "opencode.ai", "/zen/go/v1/messages", "X-Api-Key", goKey},
		{"v1/chat/completions", opencode, "kimi-k3", "opencode.ai", "/zen/go/v1/chat/completions", "Authorization", "Bearer " + goKey},
		{"v1/responses", opencode, "kimi-k3", "opencode.ai", "/zen/go/v1/responses", "Authorization", "Bearer " + goKey},
	} {
		if response, got := f.post(t, c.path, c.header, fmt.Sprintf(bodies[c.path], c.model)); response.StatusCode != 200 {
			t.Fatalf("%s %s: %d %s", c.path, c.model, response.StatusCode, got)
		}
		sent := f.peer.received()[i]
		if sent.Host != c.host || sent.Path != c.upstreamPath || sent.Header.Get(c.credential) != c.expected || len(sent.Header.Values("Authorization"))+len(sent.Header.Values("X-Api-Key")) != 1 {
			t.Fatalf("%s %s reached the upstream as %s%s %v", c.path, c.model, sent.Host, sent.Path, sent.Header)
		}
	}
	// A path no account serving the model can take, and a path no adapter of
	// the route serves, refuse before dispatch.
	if response, got := f.post(t, "v1/responses", session("X-Opencode-Session-Id", "ses_glm"), fmt.Sprintf(bodies["v1/responses"], "glm-5.3")); response.StatusCode != 503 || !bytes.Contains(got, []byte("code_account_unavailable")) {
		t.Fatalf("GLM on Responses: %d %s", response.StatusCode, got)
	}
	if response, _ := f.post(t, "responses", claude, fmt.Sprintf(bodies["v1/responses"], "kimi-k3")); response.StatusCode != 400 {
		t.Fatalf("Codex path: %d", response.StatusCode)
	}

	// The tree pins one account per model and keeps its first account.
	var root map[string]any
	for _, item := range h.want(owner, "GET", "/api/v1/code/bindings?project_id="+f.project, nil, nil, 200)["items"].([]any) {
		if binding := item.(map[string]any); binding["conversation"] == "mix" {
			root = binding
		}
	}
	pins := []string{}
	for _, item := range root["pins"].([]any) {
		pin := item.(map[string]any)
		pins = append(pins, pin["model"].(string)+"="+pin["account_id"].(string))
	}
	if root["account_id"] != glm["id"] || strings.Join(pins, " ") != "glm-5.3="+glm["id"].(string)+" minimax-m3="+other["id"].(string) {
		t.Fatalf("tree pins: %v", root)
	}
	// A cooling pin refuses its model without failing over, while the tree's
	// other models stay served.
	if err := (&resources.CodeStore{Pool: h.Pool}).ObserveHealth(t.Context(), other["id"].(string), "quota_limited"); err != nil {
		t.Fatal(err)
	}
	if response, got := f.post(t, "v1/messages", child, fmt.Sprintf(bodies["v1/messages"], "minimax-m3")); response.StatusCode != 503 || !bytes.Contains(got, []byte("code_account_unavailable")) {
		t.Fatalf("cooling pin: %d %s", response.StatusCode, got)
	}
	if response, _ := f.post(t, "v1/messages", claude, fmt.Sprintf(bodies["v1/messages"], "glm-5.3")); response.StatusCode != 200 || len(f.peer.received()) != 6 {
		t.Fatalf("the tree's GLM pin: %d", response.StatusCode)
	}

	path := "/api/v1/code/routes/" + id + "/client-config?gateway_url=https%3A%2F%2Fgateway.example"
	config := h.want(owner, "GET", path+"&plan_model=minimax-m3", nil, nil, 200)
	if config["client"] != "claude-code" || fmt.Sprint(config["supported_clients"]) != "[claude-code opencode]" || fmt.Sprint(config["adapters"]) != "[opencode_go zai_coding]" ||
		config["model"] != "glm-5.3" || config["plan_model"] != "minimax-m3" || !strings.Contains(config["configuration"].(string), "export ANTHROPIC_MODEL='opusplan'") {
		t.Fatalf("Claude Code configuration: %v", config)
	}
	config = h.want(owner, "GET", path+"&client=opencode&plan_model=kimi-k3", nil, nil, 200)
	for _, want := range []string{`"model": "zai-coding-plan/glm-5.3"`, `"model": "opencode-go/kimi-k3"`, `"opencode-go",` + "\n" + `    "zai-coding-plan"`} {
		if !strings.Contains(config["configuration"].(string), want) {
			t.Fatalf("OpenCode configuration lacks %s:\n%s", want, config["configuration"])
		}
	}
	for _, refused := range []string{"&client=codex", "&plan_model=unpublished", "&model=claude-sonnet-4-6"} {
		h.want(owner, "GET", path+refused, nil, nil, 422)
	}
	if config["client_version"] != codeadapter.OpenCodeVersion {
		t.Fatal("client pins changed")
	}
}
