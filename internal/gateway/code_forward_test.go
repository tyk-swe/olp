package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/resources"
	"github.com/tyk-swe/olp/internal/runtime"
)

type codeTestLedger struct {
	mu         sync.Mutex
	inputs     []resources.CodeAdmission
	marks      []string
	aborts     []string
	usages     []codemode.Usage
	health     []string
	allowances []codemode.Allowance
	refusals   []string
	refuse     string
	done       chan codemode.Usage
	references map[string]bool
	outcomes   []codemode.Outcome
}

func (l *codeTestLedger) ObserveOutcome(_ context.Context, _ string, outcome codemode.Outcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.outcomes = append(l.outcomes, outcome)
	return nil
}

func (l *codeTestLedger) Admit(_ context.Context, in resources.CodeAdmission) (resources.CodePermit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inputs = append(l.inputs, in)
	if in.PreviousResponse != "" && !l.references[in.PreviousResponse] {
		return resources.CodePermit{}, codemode.Refuse(409, "code_parent_unresolved")
	}
	if l.refuse != "" {
		return resources.CodePermit{}, codemode.Refuse(409, l.refuse)
	}
	return resources.CodePermit{Authority: access.Authority{ID: in.APIKeyID, Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}}}, Account: codemode.Account{ID: "account", ProviderID: "provider", Principal: "principal"}, Binding: codemode.Binding{ID: "binding", AccountID: "account", Principal: "principal"}, Attempt: codemode.Attempt{ID: fmt.Sprint(len(l.inputs))}}, nil
}
func (l *codeTestLedger) MarkDispatched(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.marks = append(l.marks, id)
	return nil
}
func (l *codeTestLedger) ObserveReference(_ context.Context, _, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.references == nil {
		l.references = map[string]bool{}
	}
	l.references[id] = true
	return nil
}
func (l *codeTestLedger) BindConnection(_ context.Context, _ codemode.Route, key string, _ codemode.Identity, _ string) (resources.CodePermit, error) {
	return resources.CodePermit{Authority: access.Authority{ID: key, Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}}}, Account: codemode.Account{ID: "account", ProviderID: "provider", Principal: "principal"}, Binding: codemode.Binding{ID: "binding", AccountID: "account", Principal: "principal"}}, nil
}
func (l *codeTestLedger) Settle(_ context.Context, _ string, u codemode.Usage) error {
	l.mu.Lock()
	l.usages = append(l.usages, u)
	l.mu.Unlock()
	l.done <- u
	return nil
}
func (l *codeTestLedger) Abort(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.aborts = append(l.aborts, id)
	return nil
}
func (l *codeTestLedger) ObserveHealth(_ context.Context, _ string, status string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.health = append(l.health, status)
	return nil
}
func (l *codeTestLedger) ObserveAllowance(_ context.Context, _ string, a codemode.Allowance) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.allowances = append(l.allowances, a)
	return nil
}
func (l *codeTestLedger) RecordRefusal(_ context.Context, _ codemode.Route, _ string, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refusals = append(l.refusals, reason)
	return nil
}
func (l *codeTestLedger) wait(t *testing.T) codemode.Usage {
	t.Helper()
	select {
	case u := <-l.done:
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("attempt not settled")
		return codemode.Usage{}
	}
}

type codeTestAuthorizer struct{ principal string }

func (a codeTestAuthorizer) AuthorizeCode(context.Context, runtime.Configuration, codemode.Account) (codemode.Authorization, error) {
	return codemode.Authorization{Principal: a.principal, Headers: http.Header{"Authorization": {"Bearer upstream-only"}, "Chatgpt-Account-Id": {"upstream-account"}}}, nil
}

func newCodeForwardHarness(t *testing.T) (*harness, *codeTestLedger, *httptest.Server) {
	t.Helper()
	h := newHarness(t, Config{})
	ledger := &codeTestLedger{done: make(chan codemode.Usage, 100)}
	route := codemode.Route{ID: "route", Slug: "coding", ProjectID: "project", RevisionID: "revision", Enabled: true, Models: []string{"native-model"}}
	authority := h.rt.keys[fullKey]
	authority.ProjectID = &route.ProjectID
	h.rt.keys[fullKey] = authority
	h.rt.release.Snapshot.CodeRoutes = map[string]codemode.Route{"coding": route}
	h.rt.release.Snapshot.CodeConnections = map[string]runtime.Configuration{"revision:provider": {Endpoint: h.upstream.URL + "/a"}}
	h.gateway.CodeLedger = ledger
	h.gateway.CodeAuthorizer = codeTestAuthorizer{principal: "principal"}
	mux := http.NewServeMux()
	h.gateway.RegisterCode(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return h, ledger, server
}

func codeDo(t *testing.T, server *httptest.Server, body []byte, extra http.Header) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = extra.Clone()
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.Header.Set("Authorization", "Bearer "+fullKey)
	r.Header.Set("Session_id", "root")
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func TestCodeForwardHTTPExactBytesHeadersSSEAndUsage(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	body := []byte("{ \"model\" : \"native-model\", \"input\":[{\"type\":\"function_call_output\",\"call_id\":\"call_1\",\"output\":\"SECRET TOOL DATA\"}], \"unknown\":{\"n\":1e2}, \"stream\":true }")
	wire := ": heartbeat\r\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"SECRET ANSWER\"}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":18,\"input_tokens_details\":{\"cached_tokens\":4},\"output_tokens_details\":{\"reasoning_tokens\":2}}}}\n\n"
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if !bytes.Equal(got, body) || r.URL.Path != "/a/responses" {
			t.Errorf("request bytes/path changed")
		}
		if r.Header.Get("Authorization") != "Bearer upstream-only" || r.Header.Get("Chatgpt-Account-Id") != "upstream-account" || r.Header.Get("X-Olp-Private") != "" || r.Header.Get("X-Hop") != "" {
			t.Errorf("unsafe request headers: %v", r.Header)
		}
		if !reflect.DeepEqual(r.Header.Values("X-Native"), []string{"one", "two"}) || r.Header.Get("Session_id") != "root" || r.Header.Get("Accept-Encoding") != "" {
			t.Errorf("native headers changed: %v", r.Header)
		}
		ledger.mu.Lock()
		marked := len(ledger.marks)
		ledger.mu.Unlock()
		if marked != 1 {
			t.Error("dispatch before durable mark")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header()["X-Native-Response"] = []string{"first", "second"}
		w.Header().Set("X-Request-Id", "upstream-id")
		for i := 0; i < len(wire); i += 3 {
			_, _ = io.WriteString(w, wire[i:min(i+3, len(wire))])
			w.(http.Flusher).Flush()
		}
	})
	response := codeDo(t, server, body, http.Header{"X-Native": {"one", "two"}, "X-Olp-Private": {"secret"}, "Connection": {"X-Hop"}, "X-Hop": {"remove"}})
	got, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || string(got) != wire || !reflect.DeepEqual(response.Header.Values("X-Native-Response"), []string{"first", "second"}) || response.Header.Get("X-Request-Id") != "upstream-id" {
		t.Fatalf("response changed: %d %q %v", response.StatusCode, got, err)
	}
	u := ledger.wait(t)
	if u.Total == nil || *u.Total != 18 || *u.Cached != 4 || *u.Reasoning != 2 {
		t.Fatalf("usage=%+v", u)
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.inputs) != 1 || ledger.inputs[0].Bound != nil || ledger.inputs[0].Operation.Model != "native-model" || ledger.inputs[0].Operation.Identity.Conversation != "root" || len(ledger.marks) != 1 {
		t.Fatalf("admission=%+v marks=%v", ledger.inputs, ledger.marks)
	}
	if len(h.sink.envs) != 0 {
		t.Fatal("code payload entered transformed diagnostic envelope")
	}
}

func TestCodeForwardCompressedBodiesErrorsAndRedirectNeverReplay(t *testing.T) {
	for _, status := range []int{200, 307, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			var body, answer bytes.Buffer
			encoder := gzip.NewWriter(&body)
			_, _ = encoder.Write([]byte(`{"model":"native-model","input":"private"}`))
			_ = encoder.Close()
			encoder = gzip.NewWriter(&answer)
			_, _ = encoder.Write([]byte(`{ "native_error_or_result":"exact bytes" }`))
			_ = encoder.Close()
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				if !bytes.Equal(raw, body.Bytes()) || r.Header.Get("Content-Encoding") != "gzip" {
					t.Error("encoded request changed")
				}
				w.Header().Set("Content-Encoding", "gzip")
				w.Header().Set("Location", h.upstream.URL+"/b")
				w.Header().Set("X-Codex-Primary-Used-Percent", "75")
				w.WriteHeader(status)
				_, _ = w.Write(answer.Bytes())
			})
			response := codeDo(t, server, body.Bytes(), http.Header{"Content-Encoding": {"gzip"}})
			raw, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != status || !bytes.Equal(raw, answer.Bytes()) || response.Header.Get("Content-Encoding") != "gzip" {
				t.Fatalf("response changed status=%d err=%v", response.StatusCode, err)
			}
			if ledger.wait(t).Total != nil || h.mock.count("a") != 1 || h.mock.count("b") != 0 {
				t.Fatal("unknown usage fabricated or request replayed")
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.allowances) != 1 || *ledger.allowances[0].RemainingPercent != 25 {
				t.Fatal("allowance not observed")
			}
			if status == 429 && ledger.health[0] != "quota_limited" {
				t.Fatal("quota health not observed")
			}
		})
	}
}

func TestCodeForwardPreservesTrailersAndDoesNotInjectHeaders(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if !reflect.DeepEqual(r.Trailer.Values("X-Native-Trailer"), []string{"request-one", "request-two"}) || r.Trailer.Get("Authorization") != "" || r.Trailer.Get("X-Request-Hop") != "" || r.Header.Get("User-Agent") != "" || r.Header.Get("Accept-Encoding") != "" {
			t.Errorf("request trailers or headers changed: %v %v", r.Header, r.Trailer)
		}
		w.Header().Set("Connection", " X-Response-Hop, close ")
		w.Header().Set("Trailer", "X-Native-Trailer, X-Response-Hop")
		_, _ = io.WriteString(w, `{"status":"completed","usage":{"total_tokens":0,"input_tokens":0,"output_tokens":0}}`)
		w.Header()["X-Native-Trailer"] = []string{"response-one", "response-two"}
		w.Header().Set("X-Response-Hop", "remove")
	})
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses", strings.NewReader(`{"model":"native-model","input":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = http.Header{"Authorization": {"Bearer " + fullKey}, "Session_id": {"root"}, "User-Agent": {}, "Connection": {" x-request-hop "}}
	r.ContentLength = -1
	r.Trailer = http.Header{"X-Native-Trailer": {"request-one", "request-two"}, "Authorization": {"untrusted"}, "X-Request-Hop": {"remove"}}
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	defer client.CloseIdleConnections()
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(response.Trailer.Values("X-Native-Trailer"), []string{"response-one", "response-two"}) || response.Trailer.Get("X-Response-Hop") != "" {
		t.Fatalf("response trailers changed: %v", response.Trailer)
	}
	if usage := ledger.wait(t); usage.Total == nil || *usage.Total != 0 {
		t.Fatal("reported zero became uncertainty")
	}
}

func TestCodeForwardHTTPHealthRequiresSuccessfulGeneration(t *testing.T) {
	for _, test := range []struct {
		name, contentType, wire string
		healthy                 bool
	}{
		{"failed SSE", "text/event-stream", "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\"}}\n\n", false},
		{"incomplete SSE", "text/event-stream", "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n", false},
		{"interrupted SSE", "text/event-stream", "data: {\"type\":\"response.created\"}\n\n", false},
		{"failed unary", "application/json", `{"object":"response","status":"failed"}`, false},
		{"incomplete unary", "application/json", `{"object":"response","status":"incomplete"}`, false},
		{"completed SSE without usage", "text/event-stream", "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n", true},
		{"completed unary without usage", "application/json", `{"object":"response","status":"completed"}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, test.wire)
			})
			response := codeDo(t, server, []byte(`{"model":"native-model","input":[]}`), nil)
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || string(body) != test.wire {
				t.Fatalf("response changed: %d %q %v", response.StatusCode, body, err)
			}
			if ledger.wait(t).Total != nil {
				t.Fatal("missing usage became known")
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			var want []string
			if test.healthy {
				want = []string{"healthy"}
			}
			if !reflect.DeepEqual(ledger.health, want) {
				t.Fatalf("health=%v want %v", ledger.health, want)
			}
			if len(ledger.marks) != 1 || h.mock.count("a") != 1 {
				t.Fatal("generation was replayed")
			}
		})
	}
}

func TestCodeForwardRejectedWebSocketHandshakePreservesTrailersAndHealth(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			wire := `{"error":{"message":"native WebSocket refusal"}}`
			h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Connection", "X-Hop-Trailer")
				w.Header().Set("Trailer", "X-Native-Trailer, X-Hop-Trailer")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, wire)
				w.Header()["X-Native-Trailer"] = []string{"one", "two"}
				w.Header().Set("X-Hop-Trailer", "remove")
			})
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/code/coding/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header = http.Header{
				"Authorization":         {"Bearer " + fullKey},
				"Thread-Id":             {"handshake"},
				"Connection":            {"Upgrade"},
				"Upgrade":               {"websocket"},
				"Sec-Websocket-Version": {"13"},
				"Sec-Websocket-Key":     {"dGhlIHNhbXBsZSBub25jZQ=="},
			}
			response, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != status || string(body) != wire {
				t.Fatalf("refusal changed: %d %q %v", response.StatusCode, body, err)
			}
			if !reflect.DeepEqual(response.Trailer.Values("X-Native-Trailer"), []string{"one", "two"}) || response.Trailer.Get("X-Hop-Trailer") != "" {
				t.Fatalf("refusal trailers changed: %v", response.Trailer)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			var want []string
			if status == http.StatusServiceUnavailable {
				want = []string{"unavailable"}
			}
			if !reflect.DeepEqual(ledger.health, want) || len(ledger.inputs) != 0 || len(ledger.marks) != 0 {
				t.Fatalf("handshake health=%v inputs=%v dispatches=%v", ledger.health, ledger.inputs, ledger.marks)
			}
		})
	}
}

func TestCodeForwardWebSocketErrorBytesAndQuotaMetadata(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	wire := []byte(`{ "type":"error", "status":429, "error":{"message":"PRIVATE ERROR"}, "headers":{"x-codex-primary-used-percent":"100"} }`)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer c.CloseNow()
		if _, _, err = c.Read(r.Context()); err != nil {
			return
		}
		_ = c.Write(r.Context(), websocket.MessageText, wire)
		_, _, _ = c.Read(r.Context())
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Session_id": {"root"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	if err = c.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"native-model","input":"test"}`)); err != nil {
		t.Fatal(err)
	}
	_, body, err := c.Read(ctx)
	if err != nil || !bytes.Equal(body, wire) {
		t.Fatalf("error frame changed: %s %v", body, err)
	}
	if usage := ledger.wait(t); usage.Total != nil {
		t.Fatal("upstream error fabricated zero usage")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.marks) != 1 || len(ledger.health) != 1 || ledger.health[0] != "quota_limited" || len(ledger.allowances) != 1 || *ledger.allowances[0].RemainingPercent != 0 {
		t.Fatalf("quota metadata missing: %v %v", ledger.health, ledger.allowances)
	}
}

func TestCodeForwardRefusalsNeverDispatch(t *testing.T) {
	for _, kind := range []string{"principal", "unpublished", "previous-response", "ledger-budget", "rate-limit"} {
		t.Run(kind, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			body := `{"model":"native-model"}`
			switch kind {
			case "principal":
				h.gateway.CodeAuthorizer = codeTestAuthorizer{principal: "different"}
			case "unpublished":
				h.rt.release.Snapshot.CodeConnections = nil
			case "previous-response":
				body = `{"model":"native-model","previous_response_id":"unknown"}`
			case "ledger-budget":
				ledger.refuse = "code_token_bound_unavailable"
			case "rate-limit":
				authority := h.rt.keys[fullKey]
				n := int64(1)
				authority.Policy.MaxConcurrency = &n
				h.rt.keys[fullKey] = authority
			}
			response := codeDo(t, server, []byte(body), nil)
			_, _ = io.Copy(io.Discard, response.Body)
			if response.StatusCode < 400 || h.mock.count("a") != 0 {
				t.Fatalf("refusal dispatched: %d", response.StatusCode)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.marks) != 0 || len(ledger.usages) != 0 {
				t.Fatal("prepared refusal dispatched or settled")
			}
			if (kind == "principal" || kind == "unpublished") && len(ledger.aborts) != 1 {
				t.Fatal("prepared reservation not aborted")
			}
			if kind == "rate-limit" && len(ledger.inputs) != 0 {
				t.Fatal("ordinary rate reservation did not precede ledger")
			}
		})
	}
}

func TestCodeForwardClientCancellationPropagatesAndSettlesUnknown(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	started, canceled := make(chan struct{}), make(chan struct{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
		close(canceled)
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/code/coding/responses", strings.NewReader(`{"model":"native-model","stream":true}`))
	r.Header.Set("Authorization", "Bearer "+fullKey)
	r.Header.Set("Session_id", "root")
	response, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		t.Fatalf("generation refused: %d", response.StatusCode)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not started")
	}
	cancel()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream not canceled")
	}
	if ledger.wait(t).Total != nil || h.mock.count("a") != 1 {
		t.Fatal("canceled generation replayed or charged zero")
	}
}

func TestCodeForwardWebSocketEveryGenerationReauthenticatesAndPreservesBytes(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	frames := []string{`{ "type":"response.create", "model":"native-model", "input":[{"role":"user","content":"private"}] }`, `{"type":"response.create","model":"native-model","previous_response_id":"resp_1","input":[{"type":"function_call_output","call_id":"call_1","output":"private tool"}]}`}
	responses := []string{`{ "type":"response.completed", "response":{"id":"resp_1","usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}} }`, `{"type":"response.completed","response":{"id":"resp_2","usage":{"input_tokens":20,"output_tokens":3,"total_tokens":23}}}`}
	upstreamClosed := make(chan struct{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-only" || !reflect.DeepEqual(r.Header.Values("X-Native"), []string{"a", "b"}) {
			t.Error("handshake headers changed")
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		defer close(upstreamClosed)
		for i := range frames {
			kind, body, err := conn.Read(r.Context())
			if err != nil || kind != websocket.MessageText || string(body) != frames[i] {
				t.Errorf("frame %d changed: %s %v", i, body, err)
				return
			}
			ledger.mu.Lock()
			n := len(ledger.marks)
			ledger.mu.Unlock()
			if n != i+1 {
				t.Error("frame sent before admission/mark")
			}
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(responses[i])); err != nil {
				t.Error(err)
				return
			}
		}
		_, _, _ = conn.Read(r.Context())
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Session_id": {"root"}, "X-Native": {"a", "b"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	for i := range frames {
		if err := conn.Write(ctx, websocket.MessageText, []byte(frames[i])); err != nil {
			t.Fatal(err)
		}
		_, body, err := conn.Read(ctx)
		if err != nil || string(body) != responses[i] {
			t.Fatalf("upstream frame changed: %s %v", body, err)
		}
		if u := ledger.wait(t); u.Total == nil {
			t.Fatal("usage lost")
		}
	}
	h.rt.mu.Lock()
	delete(h.rt.keys, fullKey)
	h.rt.mu.Unlock()
	if err := conn.Write(ctx, websocket.MessageText, []byte(frames[0])); err != nil {
		t.Fatal(err)
	}
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("revoked key permitted: %v", err)
	}
	select {
	case <-upstreamClosed:
	case <-ctx.Done():
		t.Fatal("upstream socket not closed")
	}
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if len(ledger.inputs) != 2 || len(ledger.marks) != 2 || h.mock.count("a") != 1 {
		t.Fatal("socket treated as standing permit or reconnected")
	}
}

func TestCodeForwardWebSocketDisconnectLeavesUncertainty(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	started, closed := make(chan struct{}), make(chan struct{})
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		_, _, err = conn.Read(r.Context())
		if err != nil {
			t.Error(err)
			return
		}
		close(started)
		_, _, _ = conn.Read(r.Context())
		close(closed)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Session_id": {"root"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"native-model"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("no upstream generation")
	}
	_ = conn.CloseNow()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("disconnect did not propagate")
	}
	if ledger.wait(t).Total != nil {
		t.Fatal("disconnect settled as zero")
	}
}
