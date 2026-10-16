//go:build integration

package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/usage"
)

type signalFixture struct {
	t      *testing.T
	h      *accessHarness
	owner  *browser
	posts  *channelPosts
	server *httptest.Server
}

func newSignalFixture(t *testing.T) *signalFixture {
	t.Helper()
	h := newAccessHarness(t)
	h.Server.Egress = alertPolicy()
	posts := &channelPosts{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		posts.add(r.URL.Path, buf)
		w.WriteHeader(204)
	}))
	t.Cleanup(server.Close)
	f := &signalFixture{t: t, h: h, posts: posts, server: server}
	f.owner = h.owner()
	return f
}

func (f *signalFixture) destination(t *testing.T, project *string) string {
	t.Helper()
	body := map[string]any{"name": "signal-hook-" + uuid.NewString(), "url": f.server.URL + "/events"}
	if project != nil {
		body["project_id"] = *project
	}
	created := f.h.want(f.owner, "POST", "/api/v1/notifications/destinations", body,
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	return created["id"].(string)
}

func (f *signalFixture) rule(t *testing.T, event, destination string, configuration map[string]any, project *string) string {
	t.Helper()
	body := map[string]any{"name": "signal-" + event + "-" + uuid.NewString()[:8], "event": event, "destination_id": destination}
	if configuration != nil {
		body["configuration"] = configuration
	}
	if project != nil {
		body["project_id"] = *project
	}
	created := f.h.want(f.owner, "POST", "/api/v1/notifications/rules", body,
		map[string]string{"Idempotency-Key": uuid.NewString()}, 201)
	return created["id"].(string)
}

func (f *signalFixture) pass(t *testing.T, dependencies usage.NotificationDependencies) {
	t.Helper()
	deliveryPassWith(t, f.h, alertPolicy(), dependencies)
}

func (f *signalFixture) received(t *testing.T) []map[string]any {
	t.Helper()
	bodies, _ := f.posts.snapshot()
	var events []map[string]any
	for _, raw := range bodies {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatalf("delivered body %s", raw)
		}
		events = append(events, decoded)
	}
	return events
}

// An old unscoped checkpoint remains after this fleet adopts regions.

// A still-running unscoped fleet keeps its own independently stale signal.
