package kernel

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols"
)

type entryReply struct {
	status int
	retry  string
	code   string
	param  string
}

func enter(t *testing.T, h *harness, headers map[string]string, body string) entryReply {
	t.Helper()
	resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(body), headers)
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded struct {
		Error struct {
			Code  string  `json:"code"`
			Param *string `json:"param"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("reply %q is not JSON: %v", raw, err)
	}
	reply := entryReply{status: resp.StatusCode, retry: resp.Header.Get("X-Should-Retry"), code: decoded.Error.Code}
	if decoded.Error.Param != nil {
		reply.param = *decoded.Error.Param
	}
	return reply
}

func failingProvider(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	io.WriteString(w, `{"error":{"message":"boom","type":"server_error"}}`)
}

const entryChat = `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}]}`

func TestAProviderFailureFallsBackOnATransformedRouteAndIsFinalWithNoRetryHintOnAStrictOne(t *testing.T) {
	for name, tc := range map[string]struct {
		harness *harness
		want    entryReply
		second  int
	}{
		"transformed": {newHarness(t, Config{}), entryReply{status: http.StatusBadGateway, code: "upstream_unavailable"}, 1},
		"strict":      {strictHarness(t, nil), entryReply{status: http.StatusBadGateway, code: "ambiguous_upstream_result", retry: "false"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			h := tc.harness
			h.mock.set("a", failingProvider)
			h.mock.set("b", failingProvider)
			if got := enter(t, h, nil, entryChat); got != tc.want {
				t.Errorf("reply = %+v, want %+v", got, tc.want)
			}
			if first, second := h.mock.count("a"), h.mock.count("b"); first != 1 || second != tc.second {
				t.Errorf("provider calls = %d and %d, want 1 and %d", first, second, tc.second)
			}
			if env := h.sink.last(t); env.Status != http.StatusBadGateway || len(env.Attempts) != 1+tc.second {
				t.Errorf("recorded status %d with %d attempts, want 502 with %d", env.Status, len(env.Attempts), 1+tc.second)
			}
		})
	}
}

func TestRequestsTheGatewayCannotUnderstandAreRefusedBeforeAnyProviderAndRecorded(t *testing.T) {
	image := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAAAAAA"}}]}]}`
	approval := `{"model":"` + routeSlug + `","messages":[{"role":"user","content":"hi"}],"olp_approval":{"approved":true}}`
	for name, tc := range map[string]struct {
		headers map[string]string
		body    string
		want    entryReply
	}{
		"routing preferences that are not an object": {map[string]string{"X-OLP-Routing": "{"}, entryChat,
			entryReply{status: http.StatusBadRequest, code: "invalid_request", param: "X-OLP-Routing"}},
		"a priority above the key's ceiling": {map[string]string{"X-OLP-Routing": `{"priority":"critical"}`}, entryChat,
			entryReply{status: http.StatusBadRequest, code: "priority_increase_forbidden", param: "X-OLP-Routing"}},
		"attribution that is not an object": {map[string]string{"X-OLP-Attribution": "!!!"}, entryChat,
			entryReply{status: http.StatusBadRequest, code: "invalid_attribution"}},
		"inline media over the installation limit": {nil, image,
			entryReply{status: http.StatusBadRequest, code: "unsupported_parameter", param: "content"}},
		"a tool approval on a route that runs no tools": {nil, approval,
			entryReply{status: http.StatusForbidden, code: "tool_approval_invalid"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{InlineMedia: protocols.InlineMediaLimits{Items: 4, ItemBytes: 2, TotalBytes: 2}})
			if got := enter(t, h, tc.headers, tc.body); got != tc.want {
				t.Errorf("reply = %+v, want %+v", got, tc.want)
			}
			if calls := h.mock.count("a") + h.mock.count("b"); calls != 0 {
				t.Errorf("provider calls = %d, want none", calls)
			}
			if env := h.sink.last(t); env.Status != tc.want.status || len(env.Attempts) != 0 {
				t.Errorf("recorded status %d with %d attempts, want %d with none", env.Status, len(env.Attempts), tc.want.status)
			}
		})
	}
}

type lostCaller struct {
	header       http.Header
	write, flush error
}

func (c lostCaller) Header() http.Header            { return c.header }
func (lostCaller) WriteHeader(int)                  {}
func (c lostCaller) Write(b []byte) (int, error)    { return len(b), c.write }
func (lostCaller) SetReadDeadline(time.Time) error  { return nil }
func (lostCaller) SetWriteDeadline(time.Time) error { return nil }
func (c lostCaller) FlushError() error              { return c.flush }

func TestAWholeReplyCountsAsDeliveredOnlyWhenTheCallerReceivesIt(t *testing.T) {
	gone := errors.New("caller is gone")
	lost := func(caller lostCaller) func(*harness) {
		return func(h *harness) {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(entryChat))
			req.Header.Set("Authorization", "Bearer "+fullKey)
			req.Header.Set("Content-Type", "application/json")
			h.server.Config.Handler.ServeHTTP(caller, req)
		}
	}
	for name, tc := range map[string]struct {
		send      func(*harness)
		outcome   string
		delivered bool
	}{
		"received": {func(h *harness) {
			resp := h.do(t.Context(), http.MethodPost, "/v1/chat/completions", fullKey, []byte(entryChat), nil)
			resp.Body.Close()
		}, "success", true},
		"reply not written": {lost(lostCaller{header: http.Header{}, write: gone}), "cancelled", false},
		"reply not flushed": {lost(lostCaller{header: http.Header{}, flush: gone}), "cancelled", false},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, Config{})
			tc.send(h)
			env := h.sink.last(t)
			attempt := env.Attempts[len(env.Attempts)-1]
			if env.Outcome != tc.outcome || !env.Committed || !attempt.Committed || (env.FirstByte != nil) != tc.delivered {
				t.Fatalf("envelope outcome %q, committed %t, attempt committed %t, first byte %v, want outcome %q and delivered %t",
					env.Outcome, env.Committed, attempt.Committed, env.FirstByte, tc.outcome, tc.delivered)
			}
		})
	}
}
