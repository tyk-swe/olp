package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/codemode"
)

type codeHeaderAuthorizer http.Header

func (a codeHeaderAuthorizer) AuthorizeCode(context.Context, routing.Configuration, codemode.Account, codemode.Dispatch) (codemode.Authorization, error) {
	return codemode.Authorization{Principal: "principal", Headers: http.Header(a)}, nil
}

func TestCodeAuthorizationHeadersRefuseBeforeEitherTransportDispatches(t *testing.T) {
	for _, test := range []struct {
		name    string
		headers http.Header
	}{
		{"missing", nil},
		{"wrong-credential", http.Header{"X-Api-Key": {"key"}}},
		{"undeclared", http.Header{"Authorization": {"Bearer key"}, "X-Extra": {"value"}}},
		{"multiple", http.Header{"Authorization": {"Bearer key", "Bearer other"}}},
		{"no-value", http.Header{"Authorization": nil}},
		{"empty", http.Header{"Authorization": {""}}},
		{"whitespace", http.Header{"Authorization": {" \t"}}},
		{"newline", http.Header{"Authorization": {"Bearer key\r\nX-Extra: injected"}}},
		{"nul", http.Header{"Authorization": {"Bearer key\x00"}}},
		{"delete", http.Header{"Authorization": {"Bearer key\x7f"}}},
		{"duplicate-name", http.Header{"Authorization": {"Bearer key"}, "authorization": {"Bearer other"}}},
		{"extra-multiple", http.Header{"Authorization": {"Bearer key"}, "Chatgpt-Account-Id": {"account", "other"}}},
	} {
		for _, transport := range []string{"http", "websocket"} {
			t.Run(test.name+"/"+transport, func(t *testing.T) {
				h, ledger, server := newCodeForwardHarness(t)
				h.gateway.CodeAuthorizer = codeHeaderAuthorizer(test.headers)
				var response *http.Response
				if transport == "http" {
					response = codeDo(t, server, []byte(`{"model":"native-model"}`), nil)
				} else {
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					conn, rejected, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"invalid-authorization"}}})
					if conn != nil {
						conn.CloseNow()
					}
					if err == nil || rejected == nil {
						t.Fatalf("handshake accepted: %v", err)
					}
					response = rejected
				}
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				if response.StatusCode != 503 || !strings.Contains(string(body), "code_authorization_invalid") {
					t.Fatalf("%d %s", response.StatusCode, body)
				}
				ledger.mu.Lock()
				defer ledger.mu.Unlock()
				if len(ledger.marks) != 0 || h.mock.count("a") != 0 {
					t.Fatal("invalid authorization dispatched")
				}
				if transport == "http" && len(ledger.aborts) != 1 {
					t.Fatal("prepared admission was not aborted")
				}
			})
		}
	}
}

func TestCodePrincipalMismatchRefusesBothTransports(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			h, ledger, server := newCodeForwardHarness(t)
			h.gateway.CodeAuthorizer = codeTestAuthorizer{principal: "another-principal"}
			var response *http.Response
			status, code := 409, "code_principal_mismatch"
			if transport == "http" {
				response = codeDo(t, server, []byte(`{"model":"native-model"}`), nil)
			} else {
				status, code = 503, "code_account_unavailable"
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				conn, rejected, err := websocket.Dial(ctx, server.URL+"/code/coding/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}, "Thread-Id": {"principal-mismatch"}}})
				if conn != nil {
					conn.CloseNow()
				}
				if err == nil || rejected == nil {
					t.Fatalf("handshake accepted: %v", err)
				}
				response = rejected
			}
			defer response.Body.Close()
			body, _ := io.ReadAll(response.Body)
			if response.StatusCode != status || !strings.Contains(string(body), code) {
				t.Fatalf("%d %s", response.StatusCode, body)
			}
			ledger.mu.Lock()
			defer ledger.mu.Unlock()
			if len(ledger.marks) != 0 || h.mock.count("a") != 0 {
				t.Fatal("principal mismatch dispatched")
			}
		})
	}
}
