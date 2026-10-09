//go:build integration

package integration_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestRouteBodyLimitBoundsRealtimeClientMessages(t *testing.T) {
	h := newAccessHarness(t)
	provider := newStrictRealtimeFixture(t, "openai")
	cap := int64(32)
	slug, key := provisionStrictRealtimeWithBodyLimit(t, h, "openai", provider.URL+"/v1", &cap)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.HTTP.URL, "http")+"/v1/realtime?model="+slug, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + key}}})
	if err != nil {
		t.Fatalf("dial: %v %+v", err, response)
	}
	defer conn.CloseNow()
	_ = conn.Write(ctx, websocket.MessageText, strictRealtimeClientFrames[0])
	_, _, err = conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Fatalf("oversized client message close=%d err=%v", websocket.CloseStatus(err), err)
	}
}
