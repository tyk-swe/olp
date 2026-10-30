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
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/resources"
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
	f := &codingPlanFixture{h: h, owner: owner, profile: profile, keys: keys, peer: newFixedHostPeer(t,
		"api.z.ai", "open.bigmodel.cn", "opencode.ai",
		"api.minimax.io", "api.minimax.cn", "api.kimi.ai", "api.kimi.com",
		"token-plan.ap-southeast-1.maas.aliyuncs.com", "token-plan.cn-beijing.maas.aliyuncs.com")}
	f.project = createProject(h, owner, "Coding plan qualification")
	f.digest = installPlugin(t, h, owner, testutil.BuildPlugin(t, "./plugins/"+plugin))
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Code key", "project_id": f.project, "scopes": []string{"inference"}, "allowed_routes": []string{"qualification"}}, idem("code-key"), 201)
	f.keyID, f.key = key["id"].(string), key["secret"].(string)
	// OpenCode Go serves GPT models only on Responses.
	models := []string{"glm-5.3", "minimax-m3"}
	if profile == codeplans.OpenCodeGoProfile {
		models = append(models, "gpt-5.5")
	}
	for i, vendorKey := range keys {
		provider := f.provider(t, fmt.Sprintf("Coding plan %d", i), profile)
		f.providers = append(f.providers, provider)
		credential := f.enroll(t, provider, vendorKey)
		f.accounts = append(f.accounts, h.want(owner, "POST", "/api/v1/code/accounts", map[string]any{
			"project_id": f.project, "provider_id": provider, "credential_id": credential, "name": "Controlled fixture only", "enabled": true, "models": models,
		}, idem(fmt.Sprintf("code-account-%d", i)), 201))
	}
	f.pool = h.want(owner, "POST", "/api/v1/code/pools", f.poolInput(f.accounts...), idem("code-pool"), 201)
	draft := h.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": models, "enabled": true}, idem("code-route"), 201)
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
	other = h.want(owner, "POST", "/api/v1/code/accounts", map[string]any{"project_id": f.project, "provider_id": provider, "credential_id": credential, "name": "Other plan", "enabled": true, "models": []string{"minimax-m3", "kimi-k3", "gpt-5.5"}}, idem("other-account"), 201)
	if other["adapter"] != "opencode_go" {
		t.Fatalf("account adapter: %v", other)
	}
	f.providers, f.accounts = append(f.providers, provider), []map[string]any{glm, other}
	f.pool = h.want(owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), f.poolInput(glm, other), etagHeader(f.pool), 200)
	id := f.route["id"].(string)
	draft := h.want(owner, "PUT", "/api/v1/code/routes/"+id, map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": []string{"glm-5.3", "minimax-m3", "kimi-k3", "gpt-5.5"}, "enabled": true}, etagHeader(f.route), 200)
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
	pages := map[string]string{
		codeplans.ZAIProfile: codeplans.ZAIKeyPage, codeplans.BigModelProfile: codeplans.BigModelKeyPage, codeplans.OpenCodeGoProfile: codeplans.OpenCodeGoKeyPage,
		codeplans.MiniMaxProfile: codeplans.MiniMaxKeyPage, codeplans.MiniMaxCNProfile: codeplans.MiniMaxCNKeyPage,
		codeplans.KimiProfile: codeplans.KimiKeyPage, codeplans.KimiCNProfile: codeplans.KimiCNKeyPage,
		codeplans.QwenTokenPlanProfile: codeplans.QwenTokenPlanKeyPage, codeplans.QwenTokenPlanCNProfile: codeplans.QwenTokenPlanCNKeyPage,
	}
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

func TestCodeModelDiscoveryTracksLiveAccountEligibility(t *testing.T) {
	f := newCodingPlanFixture(t, "zai-coding", codeplans.ZAIProfile, "controlled-key-a-0123456789abcdef", "controlled-key-b-0123456789abcdef", "controlled-key-c-0123456789abcdef")
	models := []string{"private-code-a", "private-code-b", "private-code-c"}
	accounts := make([]map[string]any, len(models))
	for i, account := range f.accounts {
		accounts[i] = f.h.want(f.owner, "PUT", "/api/v1/code/accounts/"+account["id"].(string), map[string]any{
			"project_id": f.project, "provider_id": account["provider_id"], "credential_id": account["credential_id"], "name": account["name"], "enabled": true, "models": models[i : i+1],
		}, etagHeader(account), 200)
	}
	routeID := f.route["id"].(string)
	draft := f.h.want(f.owner, "PUT", "/api/v1/code/routes/"+routeID, map[string]any{
		"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": models, "enabled": true,
	}, etagHeader(f.route), 200)
	f.h.want(f.owner, "POST", "/api/v1/code/routes/"+routeID+"/publish", nil, withMatch(draft, idem("discovery-publish")), 200)
	f.h.refresh()
	catalog := func(status int) []codemode.Model {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.gateway.URL+"/code/qualification/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer "+f.key)
		response, err := f.gateway.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("discovery status %d body %s", response.StatusCode, body)
		}
		if status != 200 {
			if strings.Contains(string(body), "private-code") || strings.Contains(string(body), "controlled-key") {
				t.Fatalf("refusal leaked account details %s", body)
			}
			return nil
		}
		var result struct {
			Data []codemode.Model `json:"data"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatal(err)
		}
		return result.Data
	}
	assertModels := func(want []string) {
		t.Helper()
		data := catalog(200)
		ids := make([]string, len(data))
		for i, model := range data {
			ids[i] = model.ID
		}
		if !reflect.DeepEqual(ids, want) {
			t.Fatalf("models %v want %v", ids, want)
		}
		config := f.h.want(f.owner, "GET", "/api/v1/code/routes/"+routeID+"/client-config?gateway_url=https%3A%2F%2Fgateway.example&client=opencode", nil, nil, 200)
		encoded, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		var setup codemode.ClientConfiguration
		if err := json.Unmarshal(encoded, &setup); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(setup.Models, data) {
			t.Fatalf("setup models differ from discovery %+v %+v", setup.Models, data)
		}
	}
	assertModels(models)
	f.pool = f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), f.poolInput(accounts[:2]...), etagHeader(f.pool), 200)
	assertModels(models[:2])
	b := accounts[1]
	b = f.h.want(f.owner, "PUT", "/api/v1/code/accounts/"+b["id"].(string), map[string]any{
		"project_id": f.project, "provider_id": b["provider_id"], "credential_id": b["credential_id"], "name": b["name"], "enabled": false, "models": models[1:2],
	}, etagHeader(b), 200)
	assertModels(models[:1])
	f.h.want(f.owner, "PUT", "/api/v1/code/accounts/"+b["id"].(string), map[string]any{
		"project_id": f.project, "provider_id": b["provider_id"], "credential_id": b["credential_id"], "name": b["name"], "enabled": true, "models": models[1:2],
	}, etagHeader(b), 200)
	assertModels(models[:2])
	providerPath := "/api/v1/providers/" + b["provider_id"].(string)
	provider := f.h.want(f.owner, "GET", providerPath, nil, nil, 200)
	f.h.want(f.owner, "POST", providerPath+"/credentials/"+b["credential_id"].(string)+"/revoke", nil, withMatch(provider, idem("discovery-credential-revoke")), 200)
	assertModels(models[:1])
	f.peer.mu.Lock()
	f.peer.respond = func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }
	f.peer.mu.Unlock()
	response, body := f.post(t, "v1/chat/completions", http.Header{"Authorization": {"Bearer " + f.key}, "X-Opencode-Session-Id": {"catalog-quota"}}, `{"model":"private-code-a","stream":true,"messages":[{"role":"user","content":"controlled"}]}`)
	if response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("quota response %d %s", response.StatusCode, body)
	}
	f.settled(t, 1)
	if data := catalog(200); len(data) != 0 {
		t.Fatalf("unavailable accounts remain discoverable %+v", data)
	}
	f.h.want(f.owner, "GET", "/api/v1/code/routes/"+routeID+"/client-config?gateway_url=https%3A%2F%2Fgateway.example&client=opencode", nil, nil, 409)
	pool := f.poolInput(accounts[:2]...)
	pool["api_key_ids"] = []string{}
	f.pool = f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), pool, etagHeader(f.pool), 200)
	catalog(403)
	f.pool = f.h.want(f.owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), f.poolInput(accounts[:2]...), etagHeader(f.pool), 200)
	path := "/api/v1/api-keys/" + f.keyID
	key := f.h.want(f.owner, "GET", path, nil, nil, 200)
	f.h.want(f.owner, "POST", path+"/revoke", nil, withMatch(key, idem("discovery-revoke")), 200)
	catalog(403)
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

// A different key is a different principal, so it can't rotate the account.

// The real authorizer places the key in the header each protocol's native client uses.

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
	responses := `{"model":"gpt-5.5","stream":true,"store":false,"input":"CONTROLLED_PRIVATE_PROMPT"}`
	for _, test := range []struct {
		plugin, profile, host, keyPrefix string
		calls                            []call
	}{
		{"zai-coding", codeplans.ZAIProfile, "api.z.ai", "", []call{
			{"v1/messages?beta=true", "/api/anthropic/v1/messages", "Authorization", "messages", claude, messages, codingPlanMessagesSSE},
			{"v1/chat/completions", "/api/coding/paas/v4/chat/completions", "Authorization", "chat", opencode, chat, codingPlanChatSSE},
		}},
		{"zai-coding", codeplans.BigModelProfile, "open.bigmodel.cn", "", []call{
			{"v1/messages?beta=true", "/api/anthropic/v1/messages", "Authorization", "messages", claude, messages, codingPlanMessagesSSE},
		}},
		{"opencode-go", codeplans.OpenCodeGoProfile, "opencode.ai", "", []call{
			{"v1/messages", "/zen/go/v1/messages", "X-Api-Key", "messages", opencode, messages, codingPlanMessagesSSE},
			{"v1/chat/completions", "/zen/go/v1/chat/completions", "Authorization", "chat", opencode, chat, codingPlanChatSSE},
			{"v1/responses", "/zen/go/v1/responses", "Authorization", "responses", opencode, responses, codingPlanResponsesSSE},
		}},
		{"minimax-plan", codeplans.MiniMaxProfile, "api.minimax.io", "sk-cp-", []call{
			{"v1/messages?beta=true", "/anthropic/v1/messages", "Authorization", "messages", claude, messages, codingPlanMessagesSSE},
			{"v1/chat/completions", "/v1/chat/completions", "Authorization", "chat", opencode, chat, codingPlanChatSSE},
		}},
		{"minimax-plan", codeplans.MiniMaxCNProfile, "api.minimax.cn", "sk-cp-", []call{
			{"v1/messages?beta=true", "/anthropic/v1/messages", "Authorization", "messages", claude, messages, codingPlanMessagesSSE},
		}},
		{"kimi-code", codeplans.KimiProfile, "api.kimi.ai", "", []call{
			{"v1/messages?beta=true", "/coding/v1/messages", "X-Api-Key", "messages", claude, messages, codingPlanMessagesSSE},
			{"v1/chat/completions", "/coding/v1/chat/completions", "Authorization", "chat", opencode, chat, codingPlanChatSSE},
		}},
		{"kimi-code", codeplans.KimiCNProfile, "api.kimi.com", "", []call{
			{"v1/messages?beta=true", "/coding/v1/messages", "X-Api-Key", "messages", claude, messages, codingPlanMessagesSSE},
		}},
		{"qwen-token-plan", codeplans.QwenTokenPlanProfile, "token-plan.ap-southeast-1.maas.aliyuncs.com", "sk-sp-", []call{
			{"v1/messages?beta=true", "/apps/anthropic/v1/messages", "Authorization", "messages", claude, messages, codingPlanMessagesSSE},
			{"v1/chat/completions", "/compatible-mode/v1/chat/completions", "Authorization", "chat", opencode, chat, codingPlanChatSSE},
		}},
		{"qwen-token-plan", codeplans.QwenTokenPlanCNProfile, "token-plan.cn-beijing.maas.aliyuncs.com", "sk-sp-", []call{
			{"v1/messages?beta=true", "/apps/anthropic/v1/messages", "Authorization", "messages", claude, messages, codingPlanMessagesSSE},
		}},
	} {
		t.Run(test.profile, func(t *testing.T) {
			key := test.keyPrefix + "0123456789abcdef0123456789abcdef." + test.profile
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

// An OpenCode child session follows its parent session's account too.

// Without the client's identity, or with two clients' identities, nothing is dispatched.

// Codex paths, the WebSocket and unlisted operations refuse before dispatch.
