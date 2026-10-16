//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// envelopedRequest is what the enveloped upstream accepts: the model and the
// Gemini generateContent request inside the upstream's own envelope.
type envelopedRequest struct {
	Model   string `json:"model"`
	Request struct {
		Contents          []json.RawMessage          `json:"contents"`
		SystemInstruction json.RawMessage            `json:"systemInstruction"`
		GenerationConfig  map[string]json.RawMessage `json:"generationConfig"`
	} `json:"request"`
}

// envelopedUpstream is the fictional upstream the reference plugin's
// reference-gemini profile places requests at: Gemini generateContent under
// /enveloped/v1beta, with each request wrapped as {model, request} and each
// response and stream event as {response}. It authenticates the way the
// reference plugin declares and records every envelope it accepts.
type envelopedUpstream struct {
	*httptest.Server
	mu       sync.Mutex
	requests []envelopedRequest
}

func newEnvelopedUpstream(t *testing.T) *envelopedUpstream {
	t.Helper()
	u := &envelopedUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Token "+pluginCredential || r.Header.Get("X-Reference-Client") != "olp" {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]any{"error": map[string]any{"code": 401, "message": "Unknown token", "status": "UNAUTHENTICATED"}})
			return
		}
		model, operation, found := strings.Cut(strings.TrimPrefix(r.URL.Path, "/enveloped/v1beta/models/"), ":")
		stream := operation == "streamGenerateContent" && r.URL.Query().Get("alt") == "sse"
		var envelope envelopedRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if r.Method != http.MethodPost || !found || operation != "generateContent" && !stream || decoder.Decode(&envelope) != nil || envelope.Model != model || len(envelope.Request.Contents) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]any{"error": map[string]any{"code": 400, "message": "Not an enveloped generateContent request", "status": "INVALID_ARGUMENT"}})
			return
		}
		u.mu.Lock()
		u.requests = append(u.requests, envelope)
		u.mu.Unlock()
		candidate := func(text, finish string) map[string]any {
			c := map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": text}}}}
			if finish != "" {
				c["finishReason"] = finish
			}
			return map[string]any{"responseId": "enveloped-1", "modelVersion": model, "candidates": []any{c}}
		}
		usage := map[string]any{"promptTokenCount": 4, "candidatesTokenCount": 6, "totalTokenCount": 10}
		if !stream {
			response := candidate("Hello from the enveloped upstream", "STOP")
			response["usageMetadata"] = usage
			writeJSON(w, map[string]any{"response": response, "traceId": "trace-unary"})
			return
		}
		last := candidate("the enveloped upstream", "STOP")
		last["usageMetadata"] = usage
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{candidate("Hello from ", ""), last} {
			data, _ := json.Marshal(map[string]any{"response": event, "traceId": "trace-stream"})
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
	}))
	t.Cleanup(u.Close)
	return u
}

func (u *envelopedUpstream) received() []envelopedRequest {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.requests)
}

// Certification probes unary and streaming chat through the envelope.

// Each request reached the upstream in its envelope, with only the
// declared rewrites made: one candidate, no seed, and the default system
// instruction unless the caller gave one.
