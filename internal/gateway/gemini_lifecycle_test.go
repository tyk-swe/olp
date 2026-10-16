package gateway

import (
	"context"
	"testing"

	"github.com/coder/websocket"
)

func TestGeminiProjectedIdentityScanDecodesEscapedExtensions(t *testing.T) {
	for _, test := range []struct {
		name, body string
		leaks      bool
	}{
		{"plain", `{"id":"local","native":"int_foo"}`, true},
		{"escaped string", `{"id":"local","native":{"ref":"int\u005ffoo"}}`, true},
		{"escaped member", `{"id":"local","native":{"int\u005ffoo":true}}`, true},
		{"array", `{"id":"local","native":["other","int\u005ffoo"]}`, true},
		{"safe", `{"id":"local","native":{"ref":"other"}}`, false},
		{"invalid", `{"id":"local","native":`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := containsNativeResourceID([]byte(test.body), "int_foo"); got != test.leaks {
				t.Fatalf("decoded identity leak=%t, want %t", got, test.leaks)
			}
		})
	}
}

type stalledGeminiWriter struct{}

func (stalledGeminiWriter) Write(ctx context.Context, _ websocket.MessageType, _ []byte) error {
	<-ctx.Done()
	return ctx.Err()
}
