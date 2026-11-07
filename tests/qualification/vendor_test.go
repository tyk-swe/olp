//go:build qualification

package qualification_test

import (
	"bufio"
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
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/grokauth"
)

const (
	vendorModel        = "fixture-model"
	vendorAnswer       = "hello from the fixture vendor"
	vendorInputTokens  = 4
	vendorOutputTokens = 6
	grokToken          = "opaque-grok-fixture-access"
	grokRefreshToken   = "opaque-grok-fixture-refresh"
	grokRotatedToken   = "opaque-grok-fixture-rotated-access"
	grokRotatedRefresh = "opaque-grok-fixture-rotated-refresh"
	codexAccountID     = "acct-qualify"
	codexUserID        = "user-qualify"
	zaiKey             = "qualify-zai-coding-plan-key-1"
	openCodeGoKey      = "qualify-opencode-go-key-1"
)

type vendorRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
	Tools  []struct {
		Name string `json:"name"`
	} `json:"tools"`
	Input json.RawMessage `json:"input"`
}

func refuseVendor(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func decodeVendor(w http.ResponseWriter, r *http.Request, errBody string) (vendorRequest, bool) {
	var request vendorRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Model != vendorModel {
		refuseVendor(w, http.StatusNotFound, errBody)
		return vendorRequest{}, false
	}
	return request, true
}

func bearer(r *http.Request) string {
	return r.Header.Get("Authorization")
}

func newVendorMux(codexToken string, toolProof chan<- struct{}) *http.ServeMux {
	mux := http.NewServeMux()
	openAIError := `{"error":{"message":"unknown model","type":"invalid_request_error","code":"model_not_found"}}`
	badKey := `{"error":{"message":"bad key","type":"invalid_request_error","code":"invalid_api_key"}}`
	mux.HandleFunc("POST /zen/go/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != "Bearer "+openCodeGoKey {
			refuseVendor(w, http.StatusUnauthorized, badKey)
			return
		}
		request, ok := decodeVendor(w, r, openAIError)
		if !ok {
			return
		}
		usage := fmt.Sprintf(`{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}`, vendorInputTokens, vendorOutputTokens, vendorInputTokens+vendorOutputTokens)
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":%s}`, vendorModel, vendorAnswer, usage)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", vendorModel, vendorAnswer)
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":%s}\n\n", vendorModel, usage)
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	mux.HandleFunc("POST /backend-api/codex/responses", func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != "Bearer "+codexToken || r.Header.Get("Chatgpt-Account-Id") != codexAccountID {
			refuseVendor(w, http.StatusUnauthorized, badKey)
			return
		}
		fixtureResponses(w, r, toolProof, "Bash")
	})
	mux.HandleFunc("POST /oauth2/device/code", func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.Form.Get("client_id") != grokauth.ClientID || !strings.Contains(r.Form.Get("scope"), "offline_access") {
			refuseVendor(w, 400, `{"error":"invalid_request"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"device_code":"fixture-device-session","user_code":"FIXTURE","verification_uri":"https://auth.x.ai/device","expires_in":600,"interval":1}`)
	})
	mux.HandleFunc("POST /oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		if r.ParseForm() != nil || r.Form.Get("client_id") != grokauth.ClientID {
			refuseVendor(w, 400, `{"error":"invalid_request"}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code" && r.Form.Get("device_code") == "fixture-device-session":
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"token_type":"Bearer","expires_in":3600}`, grokToken, grokRefreshToken)
		case r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == grokRefreshToken:
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"token_type":"Bearer","expires_in":3600}`, grokRotatedToken, grokRotatedRefresh)
		default:
			refuseVendor(w, 400, `{"error":"invalid_grant"}`)
		}
	})
	mux.HandleFunc("GET /oauth2/userinfo", func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != "Bearer "+grokToken && bearer(r) != "Bearer "+grokRotatedToken {
			refuseVendor(w, 401, badKey)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sub":"fixture-subject"}`)
	})
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != "Bearer "+grokRotatedToken || r.Header.Get("X-Grok-Client-Identifier") != "grok-shell" || r.Header.Get("X-Xai-Token-Auth") != "xai-grok-cli" || r.Header.Get("X-Authenticateresponse") != "authenticate-response" {
			refuseVendor(w, http.StatusUnauthorized, badKey)
			return
		}
		fixtureResponses(w, r, toolProof, "exec_command")
	})
	mux.HandleFunc("POST /api/anthropic/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if bearer(r) != "Bearer "+zaiKey {
			refuseVendor(w, http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`)
			return
		}
		request, ok := decodeVendor(w, r, `{"type":"error","error":{"type":"not_found_error","message":"unknown model"}}`)
		if !ok {
			return
		}
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"msg_fixture","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":%d}}`, vendorModel, vendorAnswer, vendorInputTokens, vendorOutputTokens)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fixture\",\"type\":\"message\",\"role\":\"assistant\",\"model\":%q,\"content\":[],\"stop_reason\":null,\"stop_sequence\":null,\"usage\":{\"input_tokens\":%d,\"output_tokens\":0}}}\n\n", vendorModel, vendorInputTokens)
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", vendorAnswer)
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\",\"stop_sequence\":null},\"usage\":{\"output_tokens\":%d}}\n\n", vendorOutputTokens)
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	})
	return mux
}

func fixtureCodexTool(w http.ResponseWriter, request vendorRequest, proof chan<- struct{}, toolName string) bool {
	hasBash := slices.ContainsFunc(request.Tools, func(tool struct {
		Name string `json:"name"`
	}) bool {
		return tool.Name == toolName
	})
	if !hasBash {
		return false
	}
	var input []struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(request.Input, &input); err != nil {
		refuseVendor(w, http.StatusBadRequest, `{"error":{"message":"tool input must be an array"}}`)
		return true
	}
	for _, item := range input {
		if item.Type != "function_call_output" {
			continue
		}
		if item.CallID != "call_fixture_bash" || !strings.Contains(item.Output, "olp-tool-round-trip") {
			refuseVendor(w, http.StatusBadRequest, `{"error":{"message":"fixture tool output missing"}}`)
			return true
		}
		select {
		case proof <- struct{}{}:
		default:
		}
		return false
	}
	arguments := func() string {
		if toolName == "exec_command" {
			return `{"cmd":"printf olp-tool-round-trip","max_output_tokens":100}`
		}
		return `{"command":"printf olp-tool-round-trip","description":"Print the fixture marker"}`
	}()
	item := map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": "call_fixture_bash", "name": toolName, "arguments": arguments, "status": "completed"}
	response := map[string]any{"id": "resp_fixture_tool", "object": "response", "model": vendorModel, "status": "completed", "output": []any{item}, "usage": map[string]int{"input_tokens": 4, "output_tokens": 6, "total_tokens": 10}}
	if !request.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
		return true
	}

	events := []struct {
		SequenceNumber int            `json:"sequence_number"`
		Type           string         `json:"type"`
		Response       map[string]any `json:"response,omitempty"`
		OutputIndex    *int           `json:"output_index,omitempty"`
		Item           map[string]any `json:"item,omitempty"`
		ItemID         string         `json:"item_id,omitempty"`
		Delta          string         `json:"delta,omitempty"`
		Arguments      string         `json:"arguments,omitempty"`
	}{
		{SequenceNumber: 0, Type: "response.created", Response: map[string]any{"id": "resp_fixture_tool", "object": "response", "model": vendorModel, "status": "in_progress", "output": []any{}}},
		{SequenceNumber: 1, Type: "response.output_item.added", OutputIndex: new(0), Item: map[string]any{"id": "fc_fixture", "type": "function_call", "call_id": "call_fixture_bash", "name": toolName, "arguments": "", "status": "in_progress"}},
		{SequenceNumber: 2, Type: "response.function_call_arguments.delta", ItemID: "fc_fixture", OutputIndex: new(0), Delta: arguments},
		{SequenceNumber: 3, Type: "response.function_call_arguments.done", ItemID: "fc_fixture", OutputIndex: new(0), Arguments: arguments},
		{SequenceNumber: 4, Type: "response.output_item.done", OutputIndex: new(0), Item: item},
		{SequenceNumber: 5, Type: "response.completed", Response: response},
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		raw, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, raw)
	}

	return true
}

type vendorProxy struct {
	url   string
	roots string
}

type notifyConn struct {
	net.Conn
	done chan struct{}
	once sync.Once
}

func (c *notifyConn) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

type singleConnListener struct {
	conn *notifyConn
	used bool
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if !l.used {
		l.used = true
		return l.conn, nil
	}
	<-l.conn.done
	return nil, net.ErrClosed
}

func (l *singleConnListener) Close() error   { return nil }
func (l *singleConnListener) Addr() net.Addr { return l.conn.LocalAddr() }

func newVendorProxy(t *testing.T, handler http.Handler) vendorProxy {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "qualification fixture ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	certificates := map[string]tls.Certificate{}
	for i, host := range []string{"chatgpt.com", "api.z.ai", "opencode.ai", "cli-chat-proxy.grok.com", "auth.x.ai"} {
		leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		certificates[host] = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: leafKey}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	tlsConfig := &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			certificate, ok := certificates[hello.ServerName]
			if !ok {
				return nil, fmt.Errorf("no fixture certificate for %q", hello.ServerName)
			}
			return &certificate, nil
		},
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				reader := bufio.NewReader(conn)
				request, err := http.ReadRequest(reader)
				if err != nil || request.Method != http.MethodConnect {
					return
				}
				if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
					return
				}
				secured := tls.Server(conn, tlsConfig)
				if err := secured.Handshake(); err != nil {
					return
				}
				_ = http.Serve(&singleConnListener{conn: &notifyConn{Conn: secured, done: make(chan struct{})}}, handler)
			}(conn)
		}
	}()
	return vendorProxy{url: "http://" + listener.Addr().String(), roots: roots}
}

func fixtureResponses(w http.ResponseWriter, r *http.Request, toolProof chan<- struct{}, toolName string) {
	request, ok := decodeVendor(w, r, `{"error":{"message":"unknown model"}}`)
	if !ok {
		return
	}
	if fixtureCodexTool(w, request, toolProof, toolName) {
		return
	}
	message := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": vendorAnswer, "annotations": []any{}}}}
	response := map[string]any{
		"id": "resp_fixture", "object": "response", "created_at": 1, "model": vendorModel, "status": "completed", "error": nil, "incomplete_details": nil,
		"output": []any{message},
		"usage":  map[string]int{"input_tokens": vendorInputTokens, "output_tokens": vendorOutputTokens, "total_tokens": vendorInputTokens + vendorOutputTokens},
	}
	if !request.Stream {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	created, _ := json.Marshal(map[string]any{"type": "response.created", "sequence_number": 0, "response": map[string]any{"id": "resp_fixture", "object": "response", "created_at": 1, "model": vendorModel, "status": "in_progress", "error": nil, "incomplete_details": nil, "output": []any{}, "usage": nil}})
	added, _ := json.Marshal(map[string]any{"type": "response.output_item.added", "sequence_number": 1, "output_index": 0, "item": map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}})
	part, _ := json.Marshal(map[string]any{"type": "response.content_part.added", "sequence_number": 2, "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
	delta, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "sequence_number": 3, "item_id": "msg_fixture", "output_index": 0, "content_index": 0, "delta": vendorAnswer, "logprobs": []any{}})
	item, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "sequence_number": 4, "output_index": 0, "item": message})
	terminal, _ := json.Marshal(map[string]any{"type": "response.completed", "sequence_number": 5, "response": response})
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "event: response.created\ndata: %s\n\nevent: response.output_item.added\ndata: %s\n\nevent: response.content_part.added\ndata: %s\n\nevent: response.output_text.delta\ndata: %s\n\nevent: response.output_item.done\ndata: %s\n\nevent: response.completed\ndata: %s\n\n", created, added, part, delta, item, terminal)
}
