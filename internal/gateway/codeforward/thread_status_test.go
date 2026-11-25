package codeforward

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/platform/concurrency"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

type threadStatusRuntime struct{ current *release.Release }

func (r threadStatusRuntime) Release() *release.Release { return r.current }
func (r threadStatusRuntime) RoutingInputs() *routinginputs.Inputs {
	return nil
}
func (r threadStatusRuntime) Eligibility(string) routing.Eligibility { return routing.Eligible }
func (r threadStatusRuntime) Secret(context.Context, *release.Release, string) ([]byte, int64, error) {
	return nil, 0, release.ErrCredentialUnavailable
}
func (r threadStatusRuntime) NetworkSecret(context.Context, *release.Release, string, string) ([]byte, error) {
	return nil, release.ErrCredentialUnavailable
}
func (r threadStatusRuntime) CredentialRefused(string, int64) {}

func threadStatusForwarder(t *testing.T, ledger *fakeCodeLedger, now time.Time, capability *keyauthority.TaskCapability) (*Forwarder, codemode.Route) {
	t.Helper()
	route := codemode.Route{ID: "route", ProjectID: "project", Slug: "team", PoolID: "pool", Models: []string{"model"}, Enabled: true, RevisionID: "revision", Revision: 1, PublishedAt: &now}
	key := codeKeyFixture{authority: keyauthority.Authority{ID: "key", ProjectID: &route.ProjectID, Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}}, TaskCapability: capability}}
	snapshot, err := release.NewRelease("release", 1, &routing.Snapshot{Generation: routing.Generation{ID: uuid.NewString()}, CodeRoutes: map[string]codemode.Route{route.Slug: route}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &Forwarder{
		gate:    ingress.NewGate(key, concurrency.NewPool(4), nil, 0, func() time.Time { return now }),
		ledger:  ledger,
		runtime: threadStatusRuntime{current: snapshot},
		now:     func() time.Time { return now },
	}, route
}

func TestCodeThreadStatusReturnsLedgerStatusWithoutDispatch(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 2, 0, 0, time.UTC)
	observed := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	retry := time.Date(2026, 11, 5, 10, 5, 0, 0, time.UTC)
	ledger := &fakeCodeLedger{threadStatus: codemode.ThreadStatus{Status: codemode.ThreadUnavailable, CheckedAt: now, LastObservedAt: &observed, RetryAt: &retry}}
	forwarder, _ := threadStatusForwarder(t, ledger, now, nil)
	mux := http.NewServeMux()
	forwarder.Register(mux)
	request := httptest.NewRequest(http.MethodGet, "/code/team/status", nil)
	request.Header.Set("Authorization", "Bearer fixture")
	request.Header.Set("Thread-Id", "thread-1")
	request.Header.Set("X-Codex-Parent-Thread-Id", "parent-1")
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status response = %d, %s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 4 || body["status"] != "unavailable" || body["retry_at"] != "2026-11-05T10:05:00Z" || body["last_observed_at"] != "2026-11-05T10:00:00Z" || body["checked_at"] != "2026-11-05T10:02:00Z" {
		t.Fatalf("status body = %v, want only status, checked, observed and retry times", body)
	}
	if recorder.Header().Get("Retry-After") != "180" || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status headers = %v, want Retry-After 180 and no-store", recorder.Header())
	}
	if ledger.threadCalls != 1 || ledger.threadKey != "key" || ledger.threadIdentity.Conversation != "thread-1" || ledger.threadIdentity.Parent != "parent-1" {
		t.Fatalf("ledger call = %d, key %q, identity %+v", ledger.threadCalls, ledger.threadKey, ledger.threadIdentity)
	}
	if len(ledger.settlements) != 0 || len(ledger.health) != 0 || len(ledger.cooldowns) != 0 || ledger.confirmations != 0 {
		t.Fatal("status polling recorded dispatch side effects")
	}
}

func TestCodeThreadStatusReadsEveryClientIdentity(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 2, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		path    string
		headers map[string]string
		want    codemode.Identity
		prefix  string
	}{
		"claude agent": {
			path:    "/code/team/v1/status",
			headers: map[string]string{"X-Claude-Code-Session-Id": "sess-1", "X-Claude-Code-Agent-Id": "agent-1"},
			want:    codemode.Identity{Conversation: "sess-1/agent-1", Parent: "sess-1"},
		},
		"opencode child": {
			path:    "/code/team/status",
			headers: map[string]string{"X-Opencode-Session-Id": "child-1", "X-Opencode-Parent-Session-Id": "root-1"},
			want:    codemode.Identity{Conversation: "child-1", Parent: "root-1"},
		},
		"affinity": {
			path:    "/code/team/status",
			headers: map[string]string{"X-OLP-Code-Affinity": "my-thread"},
			prefix:  "olp:affinity/",
		},
	} {
		t.Run(name, func(t *testing.T) {
			ledger := &fakeCodeLedger{threadStatus: codemode.ThreadStatus{Status: codemode.ThreadUnknown, CheckedAt: now}}
			forwarder, _ := threadStatusForwarder(t, ledger, now, nil)
			mux := http.NewServeMux()
			forwarder.Register(mux)
			request := httptest.NewRequest(http.MethodGet, tc.path, nil)
			request.Header.Set("Authorization", "Bearer fixture")
			for name, value := range tc.headers {
				request.Header.Set(name, value)
			}
			recorder := httptest.NewRecorder()
			mux.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"status":"unknown"`) {
				t.Fatalf("status response = %d, %s", recorder.Code, recorder.Body.String())
			}
			if tc.prefix != "" {
				if !strings.HasPrefix(ledger.threadIdentity.Conversation, tc.prefix) {
					t.Fatalf("identity = %+v, want conversation prefix %q", ledger.threadIdentity, tc.prefix)
				}
				return
			}
			if ledger.threadIdentity != tc.want {
				t.Fatalf("identity = %+v, want %+v", ledger.threadIdentity, tc.want)
			}
		})
	}
}

func TestCodeThreadStatusRefusalsStayIndistinguishable(t *testing.T) {
	now := time.Date(2026, 11, 5, 10, 2, 0, 0, time.UTC)
	serve := func(t *testing.T, ledger *fakeCodeLedger, path string, headers map[string]string, capability *keyauthority.TaskCapability) *httptest.ResponseRecorder {
		t.Helper()
		forwarder, _ := threadStatusForwarder(t, ledger, now, capability)
		mux := http.NewServeMux()
		forwarder.Register(mux)
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer fixture")
		for name, value := range headers {
			request.Header.Set(name, value)
		}
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		return recorder
	}
	missing := serve(t, &fakeCodeLedger{threadErr: codemode.Refuse(404, "code_thread_not_found")}, "/code/team/status", map[string]string{"Thread-Id": "thread-1"}, nil)
	otherRouteLedger := &fakeCodeLedger{}
	otherRoute := serve(t, otherRouteLedger, "/code/other/status", map[string]string{"Thread-Id": "thread-1"}, nil)
	if missing.Code != http.StatusNotFound || otherRoute.Code != http.StatusNotFound ||
		!strings.Contains(missing.Body.String(), "code_thread_not_found") || !strings.Contains(otherRoute.Body.String(), "code_thread_not_found") {
		t.Fatalf("missing = %d %s, other route = %d %s", missing.Code, missing.Body.String(), otherRoute.Code, otherRoute.Body.String())
	}
	if otherRouteLedger.threadCalls != 0 {
		t.Fatal("status on an unknown route reached the ledger")
	}
	noIdentity := serve(t, &fakeCodeLedger{}, "/code/team/status", nil, nil)
	if noIdentity.Code != http.StatusBadRequest || !strings.Contains(noIdentity.Body.String(), "code_identity_invalid") {
		t.Fatalf("identity-free status = %d %s, want code_identity_invalid", noIdentity.Code, noIdentity.Body.String())
	}
	mixedLedger := &fakeCodeLedger{}
	mixed := serve(t, mixedLedger, "/code/team/status", map[string]string{"Thread-Id": "thread-1", "X-Claude-Code-Session-Id": "sess-1"}, nil)
	if mixed.Code != http.StatusBadRequest || !strings.Contains(mixed.Body.String(), "code_identity_ambiguous") || mixedLedger.threadCalls != 0 {
		t.Fatalf("mixed identity status = %d %s, calls %d", mixed.Code, mixed.Body.String(), mixedLedger.threadCalls)
	}
	taskLedger := &fakeCodeLedger{}
	task := serve(t, taskLedger, "/code/team/status", map[string]string{"Thread-Id": "thread-1"}, &keyauthority.TaskCapability{Digest: "task", RouteRevisions: []string{"revision"}})
	if task.Code != http.StatusForbidden || taskLedger.threadCalls != 0 {
		t.Fatalf("task capability status = %d %s, calls %d, want a refusal before the ledger", task.Code, task.Body.String(), taskLedger.threadCalls)
	}
}
