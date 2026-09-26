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
)

type profileNetworkCall struct {
	method, path, remote, authorization, beta string
	body                                      []byte
}

type profileNetworkFixture struct {
	server     *httptest.Server
	roots      string
	credential string
	mu         sync.Mutex
	calls      []profileNetworkCall
}

// This fixture owns its TLS identity and scripted provider contract. It does
// not invoke the production profile registry or a protocol encoder as an oracle.
func newProfileNetworkFixture(t *testing.T, mutualTLS bool) *profileNetworkFixture {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "profile fixture authority"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	roots := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	issue := func(serial int64, client bool) (tls.Certificate, string, string) {
		t.Helper()
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "profile fixture peer"},
			NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		}
		if client {
			leaf.Subject.CommonName = "profile fixture client"
			leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
		if err != nil {
			t.Fatal(err)
		}
		certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
		pair, err := tls.X509KeyPair(certificatePEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return pair, string(certificatePEM), string(keyPEM)
	}
	serverPair, _, _ := issue(2, false)
	_, clientCertificate, clientKey := issue(3, true)
	secret, err := json.Marshal(map[string]string{"client_certificate_pem": clientCertificate, "client_key_pem": clientKey})
	if err != nil {
		t.Fatal(err)
	}
	f := &profileNetworkFixture{roots: string(roots), credential: string(secret)}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || mutualTLS && (len(r.TLS.VerifiedChains) != 1 || r.TLS.PeerCertificates[0].Subject.CommonName != "profile fixture client") {
			t.Error("provider call did not authenticate the configured TLS identity")
			http.Error(w, "TLS identity required", http.StatusUnauthorized)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		f.mu.Lock()
		f.calls = append(f.calls, profileNetworkCall{r.Method, r.URL.RequestURI(), r.RemoteAddr, r.Header.Get("Authorization"), r.Header.Get("OpenAI-Beta"), body})
		f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+vendorSecret && r.Header.Get("Authorization") != "Bearer profile-rotated-api-secret" {
			t.Error("provider request did not use an API credential independently of mTLS")
			http.Error(w, "API credential required", http.StatusUnauthorized)
			return
		}
		if r.Header.Get("OpenAI-Beta") != "fixture-profile-v1" {
			t.Error("configured semantic header was lost")
			http.Error(w, "semantic header required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.RequestURI() {
		case "GET /v1/models":
			io.WriteString(w, `{"data":[{"id":"fixture-model","object":"model"}]}`)
		case "POST /v1/chat/completions":
			io.WriteString(w, `{"id":"profile-result","object":"chat.completion","created":1,"model":"fixture-model","choices":[{"index":0,"message":{"role":"assistant","content":"profile fixture answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
		default:
			t.Errorf("unexpected profile endpoint %s %s", r.Method, r.URL.RequestURI())
			http.Error(w, "unexpected endpoint", http.StatusNotFound)
		}
	}))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(roots)
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverPair}, ClientCAs: pool}
	if mutualTLS {
		f.server.TLS.ClientAuth = tls.RequireAndVerifyClientCert
	}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}

func (f *profileNetworkFixture) captured() []profileNetworkCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]profileNetworkCall(nil), f.calls...)
}

func profileNetworkConfiguration(f *profileNetworkFixture) map[string]any {
	return map[string]any{
		"kind": "openai_compatible", "auth_mode": "api_key", "endpoint": f.server.URL + "/v1",
		"profile_id": "compatible-chat", "profile_revision": "1",
		"options": map[string]any{
			"semantic_headers": map[string]any{"OpenAI-Beta": "fixture-profile-v1"},
			"network":          map[string]any{"trust_roots_pem": f.roots, "connect_timeout_ms": 1500, "tls_handshake_timeout_ms": 1500, "max_idle_conns_per_host": 2},
			"operation_defaults": map[string]any{"generation": map[string]any{
				"dialect": "openai-chat", "values": map[string]any{"temperature": 0.25, "top_p": 0.8, "stop": []string{"provider-stop"}, "parallel_tool_calls": true},
				"native_options": map[string]any{"fixture_toggle": false, "fixture_null": nil, "fixture_empty": ""},
			}},
			"bindings": map[string]any{vendorModel: map[string]any{"defaults": map[string]any{"generation": map[string]any{
				"dialect": "openai-chat", "values": map[string]any{"temperature": 0.5, "stop": []string{"binding-stop"}},
				"native_options": map[string]any{"fixture_binding": "chosen"},
			}}}},
		},
	}
}

func createProfileNetworkProvider(t *testing.T, h *accessHarness, owner *browser, name string, config map[string]any) map[string]any {
	t.Helper()
	return h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": name, "configuration": config, "model": vendorModel, "credential": vendorSecret}, idem("create-"+name), http.StatusCreated)
}

func certifyProfileNetworkProvider(t *testing.T, h *accessHarness, owner *browser, providerID string) {
	t.Helper()
	path := "/api/v1/providers/" + providerID
	detail := h.want(owner, "GET", path, nil, nil, 200)
	probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 200)
	if probe["succeeded"] != true {
		t.Fatalf("configured profile probe failed: %v", probe)
	}
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)["items"].([]any)
	if len(models) != 1 {
		t.Fatalf("configured discovery returned %d models", len(models))
	}
	modelID := models[0].(map[string]any)["id"].(string)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	detail = h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"}}}, etagHeader(detail), 200)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" || certified["certified_count"] != float64(1) {
		t.Fatalf("configured profile certification failed: %v", certified)
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem("activate-"+detail["etag"].(string))), 200)
}

func publishProfileNetworkRoute(t *testing.T, h *accessHarness, owner *browser, providerID string) (string, string) {
	t.Helper()
	const slug = "profile-network"
	draft := h.want(owner, "POST", "/api/v1/route-drafts", map[string]any{
		"slug": slug, "operations": []string{"generation"}, "overall_timeout_ms": 10000, "max_attempts": 1,
		"targets": []any{map[string]any{"provider_id": providerID, "provider_model": vendorModel, "priority": 0, "weight": 1, "timeout_ms": 5000}},
	}, idem("profile-route"), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem("profile-route-activate")), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "profile request", "scopes": []string{"inference"}, "allowed_routes": []string{slug}}, idem("profile-key"), 201)
	h.refresh()
	return slug, key["secret"].(string)
}

func requireProfileNetworkJSON(t *testing.T, expected string, actual []byte) {
	t.Helper()
	decode := func(raw []byte) any {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		var value any
		if err := d.Decode(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	if !reflect.DeepEqual(decode([]byte(expected)), decode(actual)) {
		t.Fatalf("provider body differs from the independent fixture:\n got %s\nwant %s", actual, expected)
	}
}

func TestProviderProfileTLSDefaultsAndNetworkCredentialLifecycle(t *testing.T) {
	for _, mutualTLS := range []bool{false, true} {
		t.Run(map[bool]string{false: "custom trust roots", true: "mutual TLS credential"}[mutualTLS], func(t *testing.T) {
			f := newProfileNetworkFixture(t, mutualTLS)
			h := newAccessHarness(t)
			owner := h.owner()
			config := profileNetworkConfiguration(f)
			created := createProfileNetworkProvider(t, h, owner, "Profile network", config)
			providerID := created["id"].(string)
			path := "/api/v1/providers/" + providerID
			var networkID string
			if mutualTLS {
				stored := h.want(owner, "POST", path+"/network-credentials", map[string]any{"credential": f.credential}, withMatch(created, idem("network-create")), 201)
				networkID = stored["credential_id"].(string)
				config["options"].(map[string]any)["network"].(map[string]any)["credential_id"] = networkID
				created = h.want(owner, "PATCH", path, map[string]any{"name": "Profile network", "configuration": config}, etagHeader(stored), 200)
				listed := h.want(owner, "GET", path+"/network-credentials", nil, nil, 200)
				if len(listed["items"].([]any)) != 1 || listed["items"].([]any)[0].(map[string]any)["id"] != networkID {
					t.Fatal("network credential was not listed independently")
				}
				for _, item := range h.want(owner, "GET", path+"/credentials", nil, nil, 200)["items"].([]any) {
					if item.(map[string]any)["id"] == networkID {
						t.Fatal("network credential became an API credential choice")
					}
				}
				var ciphertext []byte
				if err := h.Pool.QueryRow(t.Context(), "SELECT ciphertext FROM olp_go.secrets WHERE id=$1", networkID).Scan(&ciphertext); err != nil {
					t.Fatal(err)
				}
				if len(ciphertext) == 0 || bytes.Contains(ciphertext, []byte("PRIVATE KEY")) || bytes.Contains(ciphertext, []byte(f.credential)) {
					t.Fatal("network credential was not encrypted at rest")
				}
			}
			if len(f.captured()) != 0 {
				t.Fatal("storing draft profile/network configuration dispatched upstream")
			}
			certifyProfileNetworkProvider(t, h, owner, providerID)
			slug, key := publishProfileNetworkRoute(t, h, owner, providerID)
			request := map[string]any{"model": slug, "messages": []any{map[string]any{"role": "user", "content": "fixture input"}}}
			invoke := func(auth, expected string) profileNetworkCall {
				t.Helper()
				before := len(f.captured())
				status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, request)
				calls := f.captured()
				if status != 200 || len(calls) != before+1 {
					t.Fatalf("profile inference status=%d calls=%d: %v", status, len(calls)-before, reply)
				}
				if reply["model"] != slug || reply["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["content"] != "profile fixture answer" {
					t.Fatal("profile response did not reach the client unchanged")
				}
				last := calls[len(calls)-1]
				if last.method != "POST" || last.path != "/v1/chat/completions" || last.authorization != "Bearer "+auth || last.beta != "fixture-profile-v1" {
					t.Fatal("final provider path, authentication or semantic header differs")
				}
				requireProfileNetworkJSON(t, expected, last.body)
				return last
			}
			const omitted = `{"model":"fixture-model","messages":[{"role":"user","content":"fixture input"}],"temperature":0.5,"top_p":0.8,"stop":["binding-stop"],"parallel_tool_calls":true,"fixture_toggle":false,"fixture_null":null,"fixture_empty":"","fixture_binding":"chosen"}`
			first := invoke(vendorSecret, omitted)
			second := invoke(vendorSecret, omitted)
			if first.remote != second.remote {
				t.Fatal("successive requests did not exercise the existing pooled connection")
			}
			request["temperature"], request["top_p"], request["stop"], request["parallel_tool_calls"] = 0, nil, []string{}, false
			request["fixture_toggle"] = true
			const present = `{"model":"fixture-model","messages":[{"role":"user","content":"fixture input"}],"temperature":0,"top_p":null,"stop":[],"parallel_tool_calls":false,"fixture_toggle":true,"fixture_null":null,"fixture_empty":"","fixture_binding":"chosen"}`
			invoke(vendorSecret, present)
			if !mutualTLS {
				return
			}

			// API credential rotation preserves the semantic profile and mTLS
			// reference, including after the new provider revision is published.
			beforeRotation := h.want(owner, "GET", path, nil, nil, 200)["configuration"]
			detail := h.want(owner, "GET", path, nil, nil, 200)
			h.want(owner, "POST", path+"/credentials", map[string]any{"credential": "profile-rotated-api-secret"}, withMatch(detail, idem("rotate-api")), 201)
			if after := h.want(owner, "GET", path, nil, nil, 200)["configuration"]; !reflect.DeepEqual(beforeRotation, after) {
				t.Fatal("API credential rotation changed semantic or network configuration")
			}
			certifyProfileNetworkProvider(t, h, owner, providerID)
			h.refresh()
			invoke("profile-rotated-api-secret", present)
			t.Run("portable configuration", func(t *testing.T) {
				verifyProfileNetworkPromotion(t, h, owner, f, providerID, networkID)
			})

			detail = h.want(owner, "GET", path, nil, nil, 200)
			revoked := h.want(owner, "POST", path+"/network-credentials/"+networkID+"/revoke", nil, withMatch(detail, idem("revoke-network")), 200)
			if revoked["runtime_generation"] == nil {
				t.Fatal("network revocation did not advance authority")
			}
			h.refresh()
			before := len(f.captured())
			status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, request)
			if status != http.StatusServiceUnavailable || h.gatewayCode(status, reply) != "upstream_unavailable" || len(f.captured()) != before {
				t.Fatalf("revoked network identity dispatched through its cached connection: status=%d calls=%d", status, len(f.captured())-before)
			}
			detail = h.want(owner, "GET", path, nil, nil, 200)
			h.want(owner, "PATCH", path, map[string]any{"name": "Profile network", "configuration": config}, etagHeader(detail), 422)
			if len(f.captured()) != before {
				t.Fatal("validating a revoked reference made an upstream call")
			}
		})
	}
}

func verifyProfileNetworkPromotion(t *testing.T, source *accessHarness, owner *browser, f *profileNetworkFixture, providerID, networkID string) {
	t.Helper()
	before := len(f.captured())
	exported := source.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{providerID, networkID, "credential_id", "PRIVATE KEY", "client_key_pem", "profile-rotated-api-secret", vendorSecret} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatal("configuration export leaked a secret or installation-specific reference")
		}
	}
	entry := document["providers"].([]any)[0].(map[string]any)
	networkRef, ok := entry["network_credential_ref"].(string)
	if !ok || networkRef != "Profile network/network" {
		t.Fatal("configuration export lacks a portable network credential reference")
	}
	destination := newAccessHarness(t)
	destinationOwner := destination.owner()
	for _, member := range []string{`"profile_id":"compatible-responses"`, `"Profile_ID":"compatible-responses"`} {
		ambiguous := strings.Replace(string(encoded), `"profile_id":"compatible-chat"`, `"profile_id":"compatible-chat",`+member, 1)
		if ambiguous == string(encoded) {
			t.Fatal("profile fixture was missing its explicit profile")
		}
		body := json.RawMessage(`{"document":` + ambiguous + `}`)
		destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", body, nil, 400)
		destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", body, idem("ambiguous-"+member), 400)
	}
	var colliding map[string]any
	if err := json.Unmarshal(encoded, &colliding); err != nil {
		t.Fatal(err)
	}
	collidingProvider := colliding["providers"].([]any)[0].(map[string]any)
	collidingSlot := collidingProvider["slots"].([]any)[0].(map[string]any)
	collidingSlot["name"], collidingSlot["credential_ref"] = "network", networkRef
	collidingBody := map[string]any{"document": colliding, "secret_bindings": map[string]any{networkRef: f.credential}}
	destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", collidingBody, nil, 422)
	destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", collidingBody, idem("network-api-ref-collision"), 422)
	planned := destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", map[string]any{"document": document}, nil, 200)
	bindings := map[string]any{}
	for _, raw := range planned["blockers"].([]any) {
		blocker := raw.(map[string]any)
		if blocker["detail"] != "secret_binding_required" {
			t.Fatalf("unexpected fresh configuration blocker: %v", blocker)
		}
		ref := blocker["key"].(string)
		if ref == networkRef {
			bindings[ref] = f.credential
		} else {
			bindings[ref] = "profile-rotated-api-secret"
		}
	}
	if len(bindings) != 2 || bindings[networkRef] == nil {
		t.Fatal("plan did not separate API and network secret bindings")
	}
	input := map[string]any{"document": document, "secret_bindings": bindings}
	ready := destination.want(destinationOwner, "POST", "/api/v1/configuration/plan", input, nil, 200)
	if len(ready["blockers"].([]any)) != 0 || len(ready["conflicts"].([]any)) != 0 {
		t.Fatalf("bound configuration is not applicable: %v", ready)
	}
	applied := destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", input, idem("apply-network-profile"), 200)
	appliedJSON, err := json.Marshal(applied)
	if err != nil || bytes.Contains(appliedJSON, []byte("PRIVATE KEY")) || bytes.Contains(appliedJSON, []byte("profile-rotated-api-secret")) {
		t.Fatal("configuration apply returned a secret binding")
	}
	providers := destination.want(destinationOwner, "GET", "/api/v1/providers", nil, nil, 200)["items"].([]any)
	if len(providers) != 1 || providers[0].(map[string]any)["state"] != "draft" {
		t.Fatal("import must stage exactly one draft provider")
	}
	path := "/api/v1/providers/" + providers[0].(map[string]any)["id"].(string)
	detail := destination.want(destinationOwner, "GET", path, nil, nil, 200)
	config := detail["configuration"].(map[string]any)
	importedID := config["options"].(map[string]any)["network"].(map[string]any)["credential_id"].(string)
	if importedID == networkID || importedID == "" {
		t.Fatal("import did not bind a new destination-owned network credential")
	}
	models := destination.want(destinationOwner, "GET", path+"/models", nil, nil, 200)["items"].([]any)
	for _, raw := range models {
		for _, capability := range raw.(map[string]any)["capabilities"].([]any) {
			if capability.(map[string]any)["source"] != "declared" || capability.(map[string]any)["certified_at"] != nil {
				t.Fatal("import carried source certification to the destination")
			}
		}
	}
	if routes := destination.want(destinationOwner, "GET", "/api/v1/routes", nil, nil, 200)["items"].([]any); len(routes) != 0 {
		t.Fatal("configuration apply published an inference route")
	}
	drafts := destination.want(destinationOwner, "GET", "/api/v1/route-drafts", nil, nil, 200)["items"].([]any)
	if len(drafts) != 1 || drafts[0].(map[string]any)["slug"] != "profile-network" {
		t.Fatal("configuration apply did not retain the route as a draft")
	}
	reexported := destination.want(destinationOwner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	if !reflect.DeepEqual(document["providers"], reexported["document"].(map[string]any)["providers"]) {
		t.Fatal("profile, semantic defaults or portable network reference changed in roundtrip")
	}
	destination.want(destinationOwner, "POST", "/api/v1/configuration/apply", input, idem("reapply-network-profile"), 200)
	if current := destination.want(destinationOwner, "GET", path, nil, nil, 200); current["etag"] != detail["etag"] {
		t.Fatal("applying identical bindings rotated the provider or network credential")
	}
	if len(f.captured()) != before {
		t.Fatal("export, plan or apply made an upstream call")
	}
}

func TestProviderNetworkCredentialsRejectForeignReferences(t *testing.T) {
	f := newProfileNetworkFixture(t, true)
	h := newAccessHarness(t)
	owner := h.owner()
	first := createProfileNetworkProvider(t, h, owner, "First owner", profileNetworkConfiguration(f))
	firstPath := "/api/v1/providers/" + first["id"].(string)
	stored := h.want(owner, "POST", firstPath+"/network-credentials", map[string]any{"credential": f.credential}, withMatch(first, idem("foreign-network")), 201)
	secondConfig := profileNetworkConfiguration(f)
	second := createProfileNetworkProvider(t, h, owner, "Second owner", secondConfig)
	secondConfig["options"].(map[string]any)["network"].(map[string]any)["credential_id"] = stored["credential_id"]
	h.want(owner, "PATCH", "/api/v1/providers/"+second["id"].(string), map[string]any{"name": "Second owner", "configuration": secondConfig}, etagHeader(second), 422)
	third := map[string]any{"name": "Third owner", "configuration": secondConfig, "model": vendorModel, "credential": vendorSecret}
	h.want(owner, "POST", "/api/v1/providers", third, idem("foreign-network-create"), 422)
	apiCredentials := h.want(owner, "GET", firstPath+"/credentials", nil, nil, 200)["items"].([]any)
	apiAsNetwork := profileNetworkConfiguration(f)
	apiAsNetwork["options"].(map[string]any)["network"].(map[string]any)["credential_id"] = apiCredentials[0].(map[string]any)["id"]
	h.want(owner, "PATCH", firstPath, map[string]any{"name": "First owner", "configuration": apiAsNetwork}, etagHeader(stored), 422)
	for _, provider := range []map[string]any{first, second} {
		detail := h.want(owner, "GET", "/api/v1/providers/"+provider["id"].(string), nil, nil, 200)
		if reference := detail["configuration"].(map[string]any)["options"].(map[string]any)["network"].(map[string]any)["credential_id"]; reference != nil {
			t.Fatal("rejected credential reference changed the provider configuration")
		}
	}
	if len(f.captured()) != 0 {
		t.Fatal("foreign network reference validation dispatched upstream")
	}
}

func TestProviderProfilesRejectAmbiguousAndCollidingConfiguration(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	const base = `"kind":"openai_compatible","auth_mode":"api_key","endpoint":"http://127.0.0.1:1/v1","profile_id":"compatible-chat","profile_revision":"1"`
	for _, test := range []struct {
		name, config string
		status       int
	}{
		{"duplicate profile", base + `,"profile_id":"compatible-responses"`, 400},
		{"case alias", base + `,"Profile_ID":"compatible-responses"`, 400},
		{"duplicate control", base + `,"options":{"operation_defaults":{"generation":{"dialect":"openai-chat","values":{"temperature":0,"temperature":1}}}}`, 400},
		{"semantic header alias collision", base + `,"options":{"semantic_headers":{"OpenAI-Beta":"one","openai-beta":"two"}}`, 422},
		{"semantic header control byte", base + `,"options":{"semantic_headers":{"OpenAI-Beta":"bad\u0001value"}}`, 422},
		{"semantic header DEL", base + `,"options":{"semantic_headers":{"OpenAI-Beta":"bad\u007fvalue"}}`, 422},
		{"semantic header null", base + `,"options":{"semantic_headers":{"OpenAI-Beta":null}}`, 400},
		{"network private key", base + `,"options":{"network":{"client_key_pem":"private-fixture-marker"}}`, 400},
		{"network field alias", base + `,"options":{"network":{"Connect_Timeout_MS":10}}`, 400},
		{"control native collision", base + `,"options":{"operation_defaults":{"generation":{"dialect":"openai-chat","values":{"temperature":0},"native_options":{"temperature":1}}}}`, 422},
		{"cross scope collision", base + `,"options":{"operation_defaults":{"generation":{"dialect":"openai-chat","values":{"temperature":0}}},"bindings":{"fixture-model":{"defaults":{"generation":{"dialect":"openai-chat","native_options":{"temperature":1}}}}}}`, 422},
		{"semantic credential collision", strings.Replace(base, `"api_key"`, `"headers"`, 1) + `,"options":{"credential_headers":["OpenAI-Beta"],"semantic_headers":{"OpenAI-Beta":"fixture"}}`, 422},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := json.RawMessage(`{"name":"ambiguous profile","credential":"fixture-secret","configuration":{` + test.config + `}}`)
			h.want(owner, "POST", "/api/v1/providers", body, idem(test.name), test.status)
		})
	}
	if providers := h.want(owner, "GET", "/api/v1/providers", nil, nil, 200)["items"].([]any); len(providers) != 0 {
		t.Fatal("invalid configuration persisted a provider")
	}
}
