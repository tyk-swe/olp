//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"math/big"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/net/dns/dnsmessage"
)

// The managed inference-file contract is owned by the direct OpenAI Responses
// profile, whose retained-resource qualification names the official
// api.openai.com endpoint. These fixtures let an otherwise unchanged provider
// reach a deterministic local stand-in: the test resolver answers
// api.openai.com as 127.0.0.1, the configured CONNECT proxy tunnels every :443
// target to the fixture, and the fixture presents an api.openai.com
// certificate under the provider's private trust roots.
func pinFixtureDNS(t *testing.T) {
	t.Helper()
	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(_ context.Context, network, upstream string) (net.Conn, error) {
		client, server := net.Pipe()
		go serveFixtureDNS(server, network, upstream)
		return client, nil
	}}
	t.Cleanup(func() { net.DefaultResolver = previous })
}

// serveFixtureDNS speaks enough DNS wire protocol for the Go resolver.
// net.Pipe is not a PacketConn, so the resolver always uses length-prefixed
// stream framing here regardless of the requested network. api.openai.com
// answers locally; every other query is relayed to the configured upstream
// resolver so unrelated lookups keep working.
func serveFixtureDNS(conn net.Conn, network, upstream string) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	for {
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return
		}
		query := make([]byte, binary.BigEndian.Uint16(length[:]))
		if _, err := io.ReadFull(conn, query); err != nil {
			return
		}
		response := answerFixtureQuery(query)
		if response == nil {
			response = forwardFixtureQuery(query, network, upstream)
		}
		if response == nil {
			return
		}
		frame := make([]byte, 2+len(response))
		binary.BigEndian.PutUint16(frame[:2], uint16(len(response)))
		copy(frame[2:], response)
		if _, err := conn.Write(frame); err != nil {
			return
		}
	}
}

func answerFixtureQuery(query []byte) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(query)
	if err != nil {
		return nil
	}
	question, err := parser.Question()
	if err != nil || question.Name.String() != "api.openai.com." {
		return nil
	}
	reply := dnsmessage.Message{Header: dnsmessage.Header{ID: header.ID, Response: true, Authoritative: true, RCode: dnsmessage.RCodeSuccess}, Questions: []dnsmessage.Question{question}}
	if question.Type == dnsmessage.TypeA {
		reply.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
			Body:   &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}},
		}}
	}
	packed, err := reply.Pack()
	if err != nil {
		return nil
	}
	return packed
}

func forwardFixtureQuery(query []byte, network, upstream string) []byte {
	tcp := strings.HasPrefix(network, "tcp")
	if !tcp {
		network = "udp"
	}
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(context.Background(), network, upstream)
	if err != nil {
		return nil
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	if tcp {
		frame := make([]byte, 2+len(query))
		binary.BigEndian.PutUint16(frame[:2], uint16(len(query)))
		copy(frame[2:], query)
		query = frame
	}
	if _, err := conn.Write(query); err != nil {
		return nil
	}
	if tcp {
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return nil
		}
		response := make([]byte, binary.BigEndian.Uint16(length[:]))
		if _, err := io.ReadFull(conn, response); err != nil {
			return nil
		}
		return response
	}
	buffer := make([]byte, 4096)
	n, err := conn.Read(buffer)
	if err != nil {
		return nil
	}
	return buffer[:n]
}

// connectProxy tunnels any :443 CONNECT target to the fixture, matching the
// operator-configured egress proxy shape the gateway exercises in production.
// The dialer resolves and validates destinations locally, so asserting the
// loopback CONNECT target also proves the pinned answer took effect.
func connectProxy(t *testing.T, upstream string) *httptest.Server {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, port, err := net.SplitHostPort(r.Host)
		if r.Method != http.MethodConnect || err != nil || host != "127.0.0.1" || port != "443" {
			http.Error(w, "CONNECT target refused", http.StatusBadRequest)
			return
		}
		tunnel, err := net.DialTimeout("tcp", upstream, 5*time.Second)
		if err != nil {
			http.Error(w, "fixture upstream unreachable", http.StatusBadGateway)
			return
		}
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			tunnel.Close()
			return
		}
		defer client.Close()
		defer tunnel.Close()
		if _, err = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil || buffered.Flush() != nil {
			return
		}
		go func() { _, _ = io.Copy(tunnel, buffered) }()
		_, _ = io.Copy(client, tunnel)
	}))
	t.Cleanup(proxy.Close)
	return proxy
}

type managedFileUpload struct {
	fields        map[string]string
	filename      string
	contentType   string
	data          []byte
	authorization string
}

type managedFilesFixture struct {
	*httptest.Server
	roots     string
	mu        sync.Mutex
	files     map[string]map[string]any
	contents  map[string][]byte
	uploads   []managedFileUpload
	responses [][]byte
	next      int
	respSeq   int
}

func newManagedFilesFixture(t *testing.T) *managedFilesFixture {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "managed files fixture authority"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "api.openai.com"}, DNSNames: []string{"api.openai.com"},
		NotBefore: ca.NotBefore, NotAfter: ca.NotAfter,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	f := &managedFilesFixture{roots: string(roots), files: map[string]map[string]any{}, contents: map[string][]byte{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"object": "list", "data": []any{map[string]any{"id": vendorModel, "object": "model", "created": 1, "owned_by": "fixture"}}})
	})
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.responses = append(f.responses, raw)
		f.mu.Unlock()
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			http.Error(w, "json", http.StatusBadRequest)
			return
		}
		// Every response carries a distinct upstream identity; a strict
		// response contract commits under the serving it was produced in.
		f.mu.Lock()
		f.respSeq++
		id := fmt.Sprintf("resp-fixture-%d", f.respSeq)
		f.mu.Unlock()
		writeManagedResponseFixture(w, id, vendorModel, vendorAnswer, string(body["stream"]) == "true")
	})
	mux.HandleFunc("POST /v1/files", func(w http.ResponseWriter, r *http.Request) {
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			http.Error(w, "multipart upload required", http.StatusBadRequest)
			return
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		upload := managedFileUpload{fields: map[string]string{}, authorization: r.Header.Get("Authorization")}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				http.Error(w, "malformed multipart", http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(part)
			if err != nil {
				http.Error(w, "part", http.StatusBadRequest)
				return
			}
			if part.FormName() == "file" {
				upload.filename, upload.contentType, upload.data = part.FileName(), part.Header.Get("Content-Type"), data
			} else {
				upload.fields[part.FormName()] = string(data)
			}
		}
		if upload.data == nil || upload.fields["purpose"] == "" {
			http.Error(w, "upload requires purpose and file", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.next++
		id := fmt.Sprintf("file-up-%d", f.next)
		f.uploads = append(f.uploads, upload)
		f.files[id] = map[string]any{"id": id, "object": "file", "bytes": len(upload.data), "created_at": 1, "filename": upload.filename, "purpose": upload.fields["purpose"], "status": "processed"}
		f.contents[id] = upload.data
		f.mu.Unlock()
		writeJSON(w, f.files[id])
	})
	mux.HandleFunc("GET /v1/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		file, ok := f.files[r.PathValue("id")]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":{"message":"no such file"}}`, http.StatusNotFound)
			return
		}
		writeJSON(w, file)
	})
	mux.HandleFunc("GET /v1/files/{id}/content", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		data, ok := f.contents[r.PathValue("id")]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":{"message":"no such file"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(data)
	})
	mux.HandleFunc("DELETE /v1/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		_, ok := f.files[r.PathValue("id")]
		if ok {
			delete(f.files, r.PathValue("id"))
		}
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"error":{"message":"no such file"}}`, http.StatusNotFound)
			return
		}
		writeJSON(w, map[string]any{"id": r.PathValue("id"), "object": "file", "deleted": true})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Logf("managed fixture 404: %s %s", r.Method, r.URL.String())
		http.Error(w, `{"error":{"message":"fixture no such route"}}`, http.StatusNotFound)
	})
	f.Server = httptest.NewUnstartedServer(mux)
	f.Server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	f.Server.StartTLS()
	t.Cleanup(f.Server.Close)
	return f
}

func (f *managedFilesFixture) responseBodies() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.responses...)
}

func (f *managedFilesFixture) recordedUploads() []managedFileUpload {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]managedFileUpload(nil), f.uploads...)
}

// publishManagedFileProvider registers the OpenAI Responses profile provider
// against the official endpoint and proves reachability through the
// configured proxy and trust roots before activating a strict route.
func publishManagedFileProvider(t *testing.T, h *accessHarness, owner *browser, configuration map[string]any) (map[string]any, string) {
	t.Helper()
	provider := h.want(owner, "POST", "/api/v3/providers", map[string]any{"name": "Managed files " + uuid.NewString(), "configuration": configuration, "model": vendorModel, "credential": vendorSecret}, idem(uuid.NewString()), 201)
	providerID := provider["id"].(string)
	path := "/api/v3/providers/" + providerID
	if probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(provider), 200); probe["succeeded"] != true {
		t.Fatalf("managed-file provider probe failed: %v", probe)
	}
	models := h.want(owner, "GET", path+"/models", nil, nil, 200)["items"].([]any)
	if len(models) != 1 {
		t.Fatalf("managed-file discovery returned %d models", len(models))
	}
	modelID := models[0].(map[string]any)["id"].(string)
	detail := h.want(owner, "PATCH", path+"/models/"+modelID, map[string]any{"enabled": true, "capabilities": []any{
		map[string]any{"operation": "generation", "surface": "openai", "mode": "unary"},
		map[string]any{"operation": "generation", "surface": "openai", "mode": "streaming"},
	}}, etagHeader(provider), 200)
	certified := h.want(owner, "POST", path+"/models/"+modelID+"/certify", nil, etagHeader(detail), 200)
	if certified["status"] != "certified" {
		t.Fatalf("managed-file capability certification failed: %v", certified)
	}
	detail = h.want(owner, "GET", path, nil, nil, 200)
	h.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem(uuid.NewString())), 200)
	slug := "managed-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	input := fidelityDraft(slug, providerID)
	input["fidelity"] = map[string]any{"mode": "strict"}
	draft := h.want(owner, "POST", "/api/v3/route-drafts", input, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+draft["id"].(string)+"/activate", nil, withMatch(draft, idem(uuid.NewString())), 200)
	return detail, slug
}

func (h *accessHarness) uploadManagedFile(slug, key, purpose string, extra map[string]string, filename, contentType, content string) (int, []byte) {
	h.t.Helper()
	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	if err := form.WriteField("purpose", purpose); err != nil {
		h.t.Fatal(err)
	}
	for _, name := range slices.Sorted(maps.Keys(extra)) {
		if err := form.WriteField(name, extra[name]); err != nil {
			h.t.Fatal(err)
		}
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, filename))
	header.Set("Content-Type", contentType)
	part, err := form.CreatePart(header)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		h.t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		h.t.Fatal(err)
	}
	status, raw, _ := h.gatewayRaw("POST", "/v1/files", key, &buf, map[string]string{"Content-Type": form.FormDataContentType(), "X-OLP-Route": slug})
	return status, raw
}

func fileReferenceRequest(slug, fileID string) map[string]any {
	return map[string]any{"model": slug, "input": []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "input_text", "text": "Summarize the uploaded note."},
		map[string]any{"type": "input_file", "file_id": fileID},
	}}}}
}

// managedUpstreamID reads the provider-native identity the durable authority
// bound to one gateway-local file reference.
func managedUpstreamID(t *testing.T, h *accessHarness, localID string) string {
	t.Helper()
	var upstream string
	if err := h.Pool.QueryRow(t.Context(), `SELECT upstream_id FROM olp_go.provider_resources WHERE kind='strict_file' AND 'strict_file_'||replace(id::text,'-','')=$1`, localID).Scan(&upstream); err != nil {
		t.Fatalf("managed file %s has no committed native identity: %v", localID, err)
	}
	return upstream
}

func TestManagedInferenceFileLifecycle(t *testing.T) {
	fixture := newManagedFilesFixture(t)
	proxy := connectProxy(t, strings.TrimPrefix(fixture.URL, "https://"))
	pinFixtureDNS(t)
	h := newAccessHarness(t)
	owner := h.owner()
	configuration := map[string]any{
		"kind": "openai", "profile_id": "openai-responses", "profile_revision": "1",
		"endpoint": "https://api.openai.com/v1", "auth_mode": "api_key",
		"options": map[string]any{"network": map[string]any{"proxy_url": proxy.URL, "trust_roots_pem": fixture.roots}},
	}
	detail, slug := publishManagedFileProvider(t, h, owner, configuration)
	providerPath := "/api/v3/providers/" + detail["id"].(string)
	secret := stateKey(t, h, owner, slug, true)
	other := stateKey(t, h, owner, slug, true)
	plain := stateKey(t, h, owner, slug, false)
	// A legacy route over the same provider proves managed references require
	// the strict contract, not just any generation surface.
	legacySlug := "managed-legacy-" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	legacy := h.want(owner, "POST", "/api/v3/route-drafts", fidelityDraft(legacySlug, detail["id"]), idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v3/route-drafts/"+legacy["id"].(string)+"/activate", nil, withMatch(legacy, idem(uuid.NewString())), 200)
	legacyKey := h.want(owner, "POST", "/api/v3/api-keys", map[string]any{"name": "legacy key", "scopes": []string{"inference"}, "allowed_routes": []string{legacySlug}}, idem(uuid.NewString()), 201)["secret"].(string)
	h.refresh()

	content := "managed inference file bytes marker-4\n"
	respond := func(key, fileID string) (int, map[string]any) {
		t.Helper()
		status, body, _ := h.gateway("POST", "/v1/responses", key, fileReferenceRequest(slug, fileID))
		return status, body
	}
	assertBound := func(key, localID string) {
		t.Helper()
		before := len(fixture.responseBodies())
		status, body := respond(key, localID)
		if status != http.StatusOK {
			t.Fatalf("managed file reference: status %d body %v", status, body)
		}
		bodies := fixture.responseBodies()
		if len(bodies) != before+1 {
			t.Fatalf("managed file reference dispatched %d times", len(bodies)-before)
		}
		native := `"file_id":"` + managedUpstreamID(t, h, localID) + `"`
		last := bodies[len(bodies)-1]
		if !bytes.Contains(last, []byte(native)) {
			t.Fatalf("provider request did not carry the native file identity: %s", last)
		}
		if bytes.Contains(last, []byte(localID)) {
			t.Fatalf("provider request leaked the gateway-local file identity: %s", last)
		}
	}
	assertRefused := func(key, fileID string, want int, code string) {
		t.Helper()
		before := len(fixture.responseBodies())
		status, body := respond(key, fileID)
		if status != want || h.gatewayCode(status, body) != code {
			t.Fatalf("file reference %s: status %d want %d/%s: %v", fileID, status, want, code, body)
		}
		if len(fixture.responseBodies()) != before {
			t.Fatalf("refused file reference reached the provider: %s", fileID)
		}
	}

	// Purpose and option admission is profile-owned; nothing else may commit.
	status, raw := h.uploadManagedFile(slug, secret, "assistants", nil, "notes.txt", "text/plain", content)
	if status != http.StatusBadRequest || h.gatewayCode(status, decodeJSON(t, raw)) != "target_capability" {
		t.Fatalf("undeclared purpose upload: status %d %s", status, raw)
	}
	status, raw = h.uploadManagedFile(slug, secret, "user_data", map[string]string{"ttl_seconds": "60"}, "notes.txt", "text/plain", content)
	if status != http.StatusBadRequest || h.gatewayCode(status, decodeJSON(t, raw)) != "target_capability" {
		t.Fatalf("undeclared option upload: status %d %s", status, raw)
	}
	status, raw = h.uploadManagedFile(slug, secret, "batch", nil, "input.jsonl", "application/jsonl", content)
	if status != http.StatusBadRequest || h.gatewayCode(status, decodeJSON(t, raw)) != "invalid_request" {
		t.Fatalf("batch purpose on a generation route: status %d %s", status, raw)
	}
	if len(fixture.recordedUploads()) != 0 {
		t.Fatal("refused uploads reached the provider")
	}

	// A key without provider-state authority can never commit managed state.
	status, raw = h.uploadManagedFile(slug, plain, "user_data", nil, "notes.txt", "text/plain", content)
	if status != http.StatusForbidden || h.gatewayCode(status, decodeJSON(t, raw)) != "provider_state_forbidden" {
		t.Fatalf("stateless key upload: status %d %s", status, raw)
	}

	// A qualified upload commits the verified binding and returns only the
	// gateway-local identity.
	status, raw = h.uploadManagedFile(slug, secret, "user_data",
		map[string]string{"expires_after[anchor]": "created_at", "expires_after[seconds]": "3600"},
		"notes.txt", "text/plain", content)
	if status != http.StatusOK {
		t.Fatalf("managed upload: status %d %s", status, raw)
	}
	uploaded := decodeJSON(t, raw)
	localID, _ := uploaded["id"].(string)
	if !strings.HasPrefix(localID, "strict_file_") || bytes.Contains(raw, []byte("file-up-")) {
		t.Fatalf("managed upload leaked or mislabeled identity: %s", raw)
	}
	uploads := fixture.recordedUploads()
	if len(uploads) != 1 {
		t.Fatalf("provider saw %d uploads", len(uploads))
	}
	upload := uploads[0]
	if upload.fields["purpose"] != "user_data" || upload.fields["expires_after[anchor]"] != "created_at" || upload.fields["expires_after[seconds]"] != "3600" ||
		upload.filename != "notes.txt" || upload.contentType != "text/plain" || string(upload.data) != content || upload.authorization != "Bearer "+vendorSecret {
		t.Fatalf("provider upload did not carry the exact purpose, options, name, type, bytes and credential: %+v", upload)
	}

	// The committed binding serves file lifecycle reads under this key.
	status, listed, _ := h.gateway("GET", "/v1/files", secret, nil)
	if status != http.StatusOK {
		t.Fatalf("file list: %d %v", status, listed)
	}
	found := false
	for _, item := range listed["data"].([]any) {
		if item.(map[string]any)["id"] == localID {
			found = true
		}
	}
	if !found {
		t.Fatalf("managed file missing from owner list: %v", listed)
	}
	status, fetched, _ := h.gateway("GET", "/v1/files/"+localID, secret, nil)
	if status != http.StatusOK || fetched["id"] != localID {
		t.Fatalf("managed file read: %d %v", status, fetched)
	}
	status, raw, header := h.gatewayRaw("GET", "/v1/files/"+localID+"/content", secret, nil, nil)
	if status != http.StatusOK || string(raw) != content || header.Get("X-OLP-Content-SHA256") == "" {
		t.Fatalf("managed file content: %d %q sha=%q", status, raw, header.Get("X-OLP-Content-SHA256"))
	}

	// The strict plan binds the local ID to the verified native identity and
	// the binding may be reused while authorization holds.
	assertBound(secret, localID)
	assertBound(secret, localID)

	// Foreign provider IDs never become file references, and unknown or
	// foreign-owned managed IDs are refused before any upstream contact.
	assertRefused(secret, "file-up-9", http.StatusBadRequest, "unsupported_stateful_reference")
	assertRefused(secret, "strict_file_0123456789abcdef0123456789abcdef", http.StatusBadRequest, "invalid_file_id")
	assertRefused(other, localID, http.StatusBadRequest, "invalid_file_id")

	// Managed references on a legacy route are refused before planning.
	legacyBodies := len(fixture.responseBodies())
	status, legacyBody, _ := h.gateway("POST", "/v1/responses", legacyKey, fileReferenceRequest(legacySlug, localID))
	if status != http.StatusBadRequest || h.gatewayCode(status, legacyBody) != "invalid_file_id" {
		t.Fatalf("managed file on legacy route: status %d %v", status, legacyBody)
	}
	if len(fixture.responseBodies()) != legacyBodies {
		t.Fatal("legacy-route file reference reached the provider")
	}

	// Deletion tombstones the binding and refuses further consumption.
	status, deleted, _ := h.gateway("DELETE", "/v1/files/"+localID, secret, nil)
	if status != http.StatusOK || deleted["deleted"] != true {
		t.Fatalf("managed file delete: %d %v", status, deleted)
	}
	assertRefused(secret, localID, http.StatusBadRequest, "invalid_file_id")

	// An expired mapping is refused at bind time.
	status, raw = h.uploadManagedFile(slug, secret, "user_data", nil, "expiring.txt", "text/plain", "expiring marker\n")
	if status != http.StatusOK {
		t.Fatalf("second upload: %d %s", status, raw)
	}
	expiringID, _ := decodeJSON(t, raw)["id"].(string)
	assertBound(secret, expiringID)
	if _, err := h.Pool.Exec(t.Context(), `UPDATE olp_go.provider_resources SET expires_at=now()-interval '1 second' WHERE 'strict_file_'||replace(id::text,'-','')=$1`, expiringID); err != nil {
		t.Fatal(err)
	}
	assertRefused(secret, expiringID, http.StatusBadRequest, "invalid_file_id")

	// Re-activating the provider changes the serving identity; a contract
	// committed under the prior revision is refused at bind time.
	status, raw = h.uploadManagedFile(slug, secret, "user_data", nil, "pinned.txt", "text/plain", "pinned marker\n")
	if status != http.StatusOK {
		t.Fatalf("third upload: %d %s", status, raw)
	}
	pinnedID, _ := decodeJSON(t, raw)["id"].(string)
	assertBound(secret, pinnedID)
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "PATCH", providerPath, map[string]any{"name": "Managed files renamed", "configuration": configuration}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/activate", nil, withMatch(detail, idem(uuid.NewString())), 200)
	h.refresh()
	assertRefused(secret, pinnedID, http.StatusBadRequest, "resource_affinity")

	// A file committed under the current revision binds again.
	status, raw = h.uploadManagedFile(slug, secret, "user_data", nil, "current.txt", "text/plain", "current marker\n")
	if status != http.StatusOK {
		t.Fatalf("post-revision upload: %d %s", status, raw)
	}
	currentID, _ := decodeJSON(t, raw)["id"].(string)
	assertBound(secret, currentID)

	// Credential revocation removes the resource authority's ability to stand
	// behind the binding.
	slots := h.want(owner, "GET", providerPath+"/credential-slots", nil, nil, 200)["items"].([]any)
	if len(slots) == 0 {
		t.Fatal("provider has no credential slot to revoke")
	}
	credentialVersion := slots[0].(map[string]any)["credential_version_id"].(string)
	detail = h.want(owner, "GET", providerPath, nil, nil, 200)
	h.want(owner, "POST", providerPath+"/credentials/"+credentialVersion+"/revoke", nil, withMatch(detail, idem(uuid.NewString())), 200)
	h.refresh()
	assertRefused(secret, currentID, http.StatusConflict, "provider_resource_credential_unavailable")
}

// writeManagedResponseFixture mirrors writeResponsesFixture with a caller
// identity so each strict response commits under a distinct upstream ID.
func writeManagedResponseFixture(w http.ResponseWriter, id, model, text string, stream bool) {
	response := map[string]any{"id": id, "object": "response", "created_at": 1, "model": model, "status": "completed", "error": nil, "incomplete_details": nil, "output": []any{map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}}, "usage": map[string]int{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10}}
	if !stream {
		writeJSON(w, response)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	delta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": text})
	terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
	_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: %s\n\nevent: response.completed\ndata: %s\n\n", delta, terminal)
	w.(http.Flusher).Flush()
}

func decodeJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("reply is not JSON: %s", raw)
	}
	return body
}
