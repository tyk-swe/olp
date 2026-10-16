//go:build integration

package integration_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

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

// Signing changes only authorization, so the profile serves strict routes.

// Were a call to load a plugin now, it would find none installed.
