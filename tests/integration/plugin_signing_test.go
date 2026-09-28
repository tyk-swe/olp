//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

func TestRejectedSigningHeadersDoNotExposeCredentialsInProviderDiagnostics(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	viewer := h.invite(owner, "viewer@example.com", "viewer")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a request with a rejected signing header reached the upstream")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)
	digest := installPlugin(t, h, owner, testutil.BuildPlugin(t, "./internal/plugins/testdata/fixture", "-X=main.upstream="+upstream.URL+"/v1"))
	const credential = "credential-header:probe-secret"
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{
		"name": "Rejected signing header", "credential": credential, "model": vendorModel,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "static_credential", "profile_id": "fixture-chat", "profile_revision": digest},
	}, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + created["id"].(string)
	probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(created), 200)
	if probe["succeeded"] != false {
		t.Fatalf("the rejected signing header did not fail the probe: %v", probe)
	}
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if detail["last_probe_status"] != "failed" || detail["last_probe_detail"] != probe["detail"] {
		t.Fatal("the failed probe diagnostic was not persisted")
	}
	health := h.want(viewer, "GET", "/api/v1/provider-health", nil, nil, 200)
	for surface, result := range map[string]any{"probe": probe, "provider": detail, "viewer health": health} {
		if strings.Contains(fmt.Sprint(result), credential) {
			t.Errorf("%s revealed the signing credential", surface)
		}
	}
}

// signedUpstream is the fictional upstream of the reference plugin's signed
// profile: an OpenAI Chat Completions server that authenticates each request
// by an HMAC-SHA256 of it, keyed with its API key, which never travels. It
// counts the requests it verified and refused.
type signedUpstream struct {
	*pluginUpstream
	verified, refused atomic.Int64
}

func newSignedUpstream(t *testing.T, credential string) *signedUpstream {
	t.Helper()
	u := &signedUpstream{pluginUpstream: &pluginUpstream{}}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.requests = append(u.requests, r.Header.Clone())
		u.mu.Unlock()
		timestamp := r.Header.Get("X-Reference-Timestamp")
		seconds, _ := strconv.ParseInt(timestamp, 10, 64)
		mac := hmac.New(sha256.New, []byte(credential))
		mac.Write([]byte(timestamp + "\n" + r.Method + "\n" + r.URL.RequestURI() + "\n"))
		mac.Write(body)
		if time.Since(time.Unix(seconds, 0)).Abs() > 5*time.Minute || !hmac.Equal([]byte(r.Header.Get("X-Reference-Signature")), []byte(hex.EncodeToString(mac.Sum(nil)))) ||
			r.Header.Get("X-Reference-Client") != "olp" || r.Header.Get("Authorization") != "" {
			u.refused.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"error": map[string]any{"message": "The request signature is invalid.", "type": "invalid_request_error", "code": "invalid_signature"}})
			return
		}
		u.verified.Add(1)
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &request)
		usage := map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}
		if !request.Stream {
			writeJSON(w, map[string]any{"id": "chatcmpl-signed", "object": "chat.completion", "created": 1, "model": request.Model,
				"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "Hello from the signed upstream"}, "finish_reason": "stop"}}, "usage": usage})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for i, content := range []string{"Hello", " from", " the", " signed", " upstream"} {
			chunk := map[string]any{"id": "chatcmpl-signed", "object": "chat.completion.chunk", "created": 1, "model": request.Model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": content}, "finish_reason": nil}}}
			if i == 0 {
				chunk["choices"].([]any)[0].(map[string]any)["delta"].(map[string]any)["role"] = "assistant"
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		data, _ := json.Marshal(map[string]any{"id": "chatcmpl-signed", "object": "chat.completion.chunk", "created": 1, "model": request.Model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": usage})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
	}))
	t.Cleanup(u.Close)
	return u
}

// The reference plugin's signing hook signs every upstream request of its
// signed profile, which never sends the credential: control's probe and
// certification, and a strict route's unary and streaming requests, each
// reach the upstream with a signature it verifies.
func TestPluginSigningHookSignsEveryUpstreamRequest(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	upstream := newSignedUpstream(t, pluginCredential)
	digest := installReferencePlugin(t, h, owner, upstream.pluginUpstream, "0.1.0")
	create := map[string]any{"name": "Reference signed", "credential": pluginCredential, "model": vendorModel,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "static_credential", "profile_id": "reference-signed-chat", "profile_revision": digest}}
	created := h.want(owner, "POST", "/api/v1/providers", create, idem(uuid.NewString()), 201)
	certifyPluginProvider(t, h, owner, "/api/v1/providers/"+created["id"].(string))
	certified := upstream.verified.Load()
	if certified < 3 || upstream.refused.Load() != 0 {
		t.Fatalf("control sent %d signed requests, and %d the upstream refused", certified, upstream.refused.Load())
	}

	// Signing changes only authorization, so the profile serves strict routes.
	draft := fidelityDraft("reference-signed", created["id"])
	draft["fidelity"] = map[string]any{"mode": "strict"}
	key := publishRoute(t, h, owner, draft, "Reference signed")
	h.refresh()

	status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, map[string]any{"model": "reference-signed", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if status != 200 || !strings.Contains(fmt.Sprint(reply), "Hello from the signed upstream") {
		t.Fatalf("strict unary through the signed profile: %d %v", status, reply)
	}
	streamed, _ := json.Marshal(map[string]any{"model": "reference-signed", "stream": true, "stream_options": map[string]any{"include_usage": true}, "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	code, body, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(streamed), map[string]string{"Content-Type": "application/json"})
	if code != 200 || bytes.Count(body, []byte("data: ")) < 6 || !bytes.Contains(body, []byte("data: [DONE]")) {
		t.Fatalf("strict stream through the signed profile: %d %s", code, body)
	}
	if served := upstream.verified.Load() - certified; served != 2 || upstream.refused.Load() != 0 {
		t.Fatalf("the gateway sent %d signed requests, and %d the upstream refused", served, upstream.refused.Load())
	}
	for _, headers := range upstream.received() {
		if strings.Contains(fmt.Sprint(headers), pluginCredential) {
			t.Fatalf("the credential travelled: %v", headers)
		}
	}
}

// A process prepares the plugins that providers pin as it starts, so their
// first calls find them compiled; it leaves other plugins alone.
func TestAProcessPreparesThePluginsProvidersPin(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	upstream := newSignedUpstream(t, pluginCredential)
	pinned := installReferencePlugin(t, h, owner, upstream.pluginUpstream, "0.1.0")
	unpinned := installReferencePlugin(t, h, owner, upstream.pluginUpstream, "0.2.0")
	h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Reference signed", "credential": pluginCredential, "model": vendorModel,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "static_credential", "profile_id": "reference-signed-chat", "profile_revision": pinned}}, idem(uuid.NewString()), 201)

	engine, err := plugins.NewRuntime(t.Context(), plugins.Interpreted, plugins.DefaultLimits, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	host := plugins.NewHost(engine, nil, h.Pool)
	defer host.Close(context.Background())
	if err = host.PreparePinned(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Were a call to load a plugin now, it would find none installed.
	if _, err = h.Pool.Exec(t.Context(), "DELETE FROM olp.plugins"); err != nil {
		t.Fatal(err)
	}
	request := abi.SignRequest{Profile: "reference-signed-chat", Method: "POST", URL: upstream.URL + "/v1/chat/completions", Credential: pluginCredential}
	provider := abi.Provider{Profile: "reference-signed-chat"}
	if signed, err := host.Sign(t.Context(), pinned, provider, request, nil); err != nil || signed.Headers["X-Reference-Signature"] == "" {
		t.Fatalf("the pinned plugin was not prepared: %+v %v", signed, err)
	}
	if _, err = host.Sign(t.Context(), unpinned, provider, request, nil); err == nil || !strings.Contains(err.Error(), plugins.CodeNotInstalled) {
		t.Fatalf("a plugin no provider pins was prepared: %v", err)
	}
}
