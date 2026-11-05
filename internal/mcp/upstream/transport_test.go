package upstream

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/mcp/messages"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestDecodeToolResultAcceptsTextAndMedia(t *testing.T) {
	data := base64.StdEncoding.EncodeToString([]byte("bytes"))
	raw := json.RawMessage(`{"content":[{"type":"text","text":"hello"},{"type":"image","data":"` + data + `","mimeType":"image/png"}],"structuredContent":{"ok":true}}`)
	result, err := decodeToolResult(raw)
	if err != nil {
		t.Fatalf("decodeToolResult: %v", err)
	}
	if len(result.Content) != 2 || result.Content[0].Text != "hello" || result.Content[1].Data != data || result.Content[1].MimeType != "image/png" {
		t.Fatalf("result = %+v", result)
	}
	if result.IsError {
		t.Fatal("result marked as error")
	}
}

func TestDecodeToolResultRejections(t *testing.T) {
	cases := []string{
		`{}`,
		`{"content":null}`,
		`{"content":[{"type":"text"}]}`,
		`{"content":[{"type":"text","text":"x","data":"eA=="}]}`,
		`{"content":[{"type":"image","data":"not-base64!","mimeType":"image/png"}]}`,
		`{"content":[{"type":"image","data":"eA=="}]}`,
		`{"content":[{"type":"resource","text":"x"}]}`,
		`{"content":[],"structuredContent":[1,2]}`,
		`{"content":[],"unknown":true}`,
	}
	for _, raw := range cases {
		if _, err := decodeToolResult(json.RawMessage(raw)); !errors.Is(err, ErrTransport) {
			t.Errorf("decodeToolResult(%s) err = %v, want ErrTransport", raw, err)
		}
	}
}

func TestDecodeResultRequiresMatchingReply(t *testing.T) {
	var out struct {
		Value string `json:"value"`
	}
	if err := decodeResult([]byte(`{"jsonrpc":"2.0","id":7,"result":{"value":"ok"}}`), 7, &out); err != nil || out.Value != "ok" {
		t.Fatalf("decodeResult = %v, %+v", err, out)
	}
	for _, raw := range []string{
		`{"jsonrpc":"2.0","id":8,"result":{"value":"ok"}}`,
		`{"jsonrpc":"2.0","id":7,"error":{"code":-1,"message":"no"}}`,
		`{"jsonrpc":"2.0","id":7,"result":null}`,
		`{"jsonrpc":"1.0","id":7,"result":{"value":"ok"}}`,
		`{"id":7,"result":{"value":"ok"}}`,
	} {
		if err := decodeResult([]byte(raw), 7, &out); !errors.Is(err, ErrTransport) {
			t.Errorf("decodeResult(%s) err = %v, want ErrTransport", raw, err)
		}
	}
}

func TestObserveNotificationOnlyMarksToolListChanges(t *testing.T) {
	transport := &httpConnection{}
	transport.observeNotification([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed","id":3}`))
	if transport.changed() {
		t.Fatal("request-shaped message marked the list changed")
	}
	transport.observeNotification([]byte(`{"jsonrpc":"2.0","method":"notifications/progress"}`))
	if transport.changed() {
		t.Fatal("unrelated notification marked the list changed")
	}
	transport.observeNotification([]byte(`{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`))
	if !transport.changed() {
		t.Fatal("list_changed notification was ignored")
	}
}

func TestToolContentMarshalOmitsForeignFields(t *testing.T) {
	text, err := json.Marshal(ToolContent{Type: "text", Text: "hi", Data: "eA==", MimeType: "image/png"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(text) != `{"type":"text","text":"hi"}` {
		t.Fatalf("text content = %s", text)
	}
	media, err := json.Marshal(ToolContent{Type: "audio", Text: "hi", Data: "eA==", MimeType: "audio/wav"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(media) != `{"type":"audio","data":"eA==","mimeType":"audio/wav"}` {
		t.Fatalf("media content = %s", media)
	}
}

func TestStreamedRequestErrorFailsWithoutWaitingForStreamClose(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request messages.Request
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("invalid request: %v", err)
			return
		}
		switch request.Method {
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{"tools":{}}}}`, request.ID, messages.SupportedProtocols()[0])
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":-32000,\"message\":\"refused\"}}\n\n", request.ID)
			w.(http.Flusher).Flush()
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}
	}))
	defer server.Close()
	defer close(release)
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	peer, err := NewDialer(policy, nil)(t.Context(), server.URL, nil, TransportOptions{ClientName: "test", Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close(t.Context())
	failure := make(chan error, 1)
	go func() {
		_, callErr := peer.CallTool(t.Context(), "lookup", json.RawMessage(`{}`))
		failure <- callErr
	}()
	select {
	case callErr := <-failure:
		if !errors.Is(callErr, ErrTransport) {
			t.Fatalf("got %v, want the transport failure", callErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a matching error reply left the call waiting on the open stream")
	}
}

func TestStreamableSessionLifecycle(t *testing.T) {
	for _, callFails := range []bool{false, true} {
		t.Run(fmt.Sprintf("call_fails=%t", callFails), func(t *testing.T) {
			requests := make(chan string, 8)
			protocol := messages.SupportedProtocols()[0]
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer credential" {
					t.Error("upstream credential was not forwarded")
				}
				if r.Method == http.MethodDelete {
					requests <- "delete"
					if r.Header.Get("Mcp-Session-Id") != "session" || r.Header.Get("MCP-Protocol-Version") != protocol {
						t.Error("session cleanup lost its protocol or binding")
					}
					w.WriteHeader(http.StatusNoContent)
					return
				}
				var request messages.Request
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil || !request.Valid() {
					t.Errorf("invalid request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests <- request.Method
				if request.Method != "initialize" && (r.Header.Get("Mcp-Session-Id") != "session" || r.Header.Get("MCP-Protocol-Version") != protocol) {
					t.Error("request lost its negotiated session")
				}
				w.Header().Set("Content-Type", "application/json")
				switch request.Method {
				case "initialize":
					w.Header().Set("Mcp-Session-Id", "session")
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":%q,"capabilities":{"tools":{}}}}`, request.ID, protocol)
				case "notifications/initialized":
					if len(request.ID) != 0 || len(request.Params) != 0 {
						t.Error("initialized notification acquired an ID or parameters")
					}
					w.WriteHeader(http.StatusAccepted)
				case "tools/list":
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\ndata: {\"jsonrpc\":\"2.0\",\"id\":%s,\"result\":{\"tools\":[]}}\n\n", request.ID)
				case "tools/call":
					if callFails {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"done"}]}}`, request.ID)
				default:
					t.Errorf("unexpected method %s", request.Method)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
			peer, err := NewDialer(policy, nil)(t.Context(), server.URL, []byte("credential"), TransportOptions{ClientName: "test", Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			closePeer := sync.OnceFunc(func() { peer.Close(t.Context()) })
			defer closePeer()
			listing, err := peer.ListTools(t.Context(), "")
			if err != nil || listing.Tools == nil || !peer.ListChanged() || peer.Protocol() != protocol {
				t.Fatalf("catalog or negotiated session was lost: %+v, %v", listing, err)
			}
			result, err := peer.CallTool(t.Context(), "lookup", json.RawMessage(`{}`))
			if callFails && !errors.Is(err, ErrTransport) || !callFails && (err != nil || len(result.Content) != 1 || result.Content[0].Text != "done") {
				t.Fatalf("unexpected call result: %+v, %v", result, err)
			}
			closePeer()
			close(requests)
			observed := slices.Collect(func(yield func(string) bool) {
				for method := range requests {
					if !yield(method) {
						return
					}
				}
			})
			if !slices.Equal(observed, []string{"initialize", "notifications/initialized", "tools/list", "tools/call", "delete"}) {
				t.Fatalf("session requests were retried or reordered: %v", observed)
			}
		})
	}
}
