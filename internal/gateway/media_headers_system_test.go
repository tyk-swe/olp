//go:build integration

package gateway

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/limits"
)

// nextEnvelope waits for the terminal envelope of the request that has just been
// answered: the one after the first n.
func nextEnvelope(t *testing.T, c *capture, n int) Envelope {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		if len(c.envs) > n {
			event := c.envs[len(c.envs)-1]
			c.mu.Unlock()
			return event
		}
		c.mu.Unlock()
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no envelope emitted after the first %d within two seconds", n)
	return Envelope{}
}

// TestIntegrationVideoResponsesStateTheirAttempts proves what a key that opted in
// is told by the video endpoints: a create reports the route revision that served
// it, a call on a job reports its one pinned attempt and no revision, and a list
// of jobs reports the polls it made. The list polls its jobs concurrently, which is
// what the race detector holds the execution to.
func TestIntegrationVideoResponsesStateTheirAttempts(t *testing.T) {
	const vendor = "vendor-video"
	f := seedVendorMediaFixture(t, "none", false, vendor)
	f.upstream.getStatus.Store("in_progress")
	revision := f.snapshot.Routes["video-default"].RevisionID
	setPolicy := func(metadata bool) {
		f.rt.mu.Lock()
		defer f.rt.mu.Unlock()
		f.rt.keys[f.bearer] = access.Authority{ID: f.apiKeyID, Policy: access.KeyPolicy{Scopes: []string{"inference"}, ResponseMetadata: metadata}}
	}
	// call sends a request and returns its response with the envelope the gateway
	// recorded for it once the body is read.
	call := func(method, path, contentType string, body io.Reader) (*http.Response, map[string]any, Envelope) {
		t.Helper()
		f.sink.mu.Lock()
		before := len(f.sink.envs)
		f.sink.mu.Unlock()
		resp := f.call(t, method, path, contentType, body)
		var out map[string]any
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			out = decodeJSON(t, resp)
		} else {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		return resp, out, nextEnvelope(t, f.sink, before)
	}
	metadata := func(resp *http.Response) map[string]string {
		got := map[string]string{}
		for name, values := range resp.Header {
			if strings.HasPrefix(strings.ToLower(name), "x-olp-") {
				got[name] = strings.Join(values, ",")
			}
		}
		return got
	}
	// wantMetadata holds a response's metadata to exactly what it should say and to
	// the attempts the envelope recorded, which are the headers' source.
	wantMetadata := func(name string, resp *http.Response, env Envelope, want map[string]string) {
		t.Helper()
		got := metadata(resp)
		if len(got) != len(want) {
			t.Fatalf("%s: metadata = %v, want %v", name, got, want)
		}
		for header, value := range want {
			if got[header] != value {
				t.Fatalf("%s: %s = %q, want %q (all %v)", name, header, got[header], value, got)
			}
		}
		if attempts := strconv.Itoa(len(env.Attempts)); got["X-Olp-Attempts"] != attempts {
			t.Fatalf("%s: the headers state %s attempts and the envelope recorded %s", name, got["X-Olp-Attempts"], attempts)
		}
	}

	// A key that did not opt in is told nothing of how a job was served.
	var ids []string
	for range 3 {
		resp, created, _ := call(http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create: status %d body %v", resp.StatusCode, created)
		}
		if got := metadata(resp); len(got) != 0 {
			t.Fatalf("create: a key that did not opt in saw %v", got)
		}
		ids = append(ids, created["id"].(string))
	}
	if resp, _, _ := call(http.MethodGet, "/v1/videos?limit=20", "", nil); resp.StatusCode != http.StatusOK || len(metadata(resp)) != 0 {
		t.Fatalf("list: status %d, a key that did not opt in saw %v", resp.StatusCode, metadata(resp))
	}

	setPolicy(true)
	// A create is served by a route revision, and the creation is its one attempt.
	resp, created, env := call(http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d body %v", resp.StatusCode, created)
	}
	wantMetadata("create", resp, env, map[string]string{"X-Olp-Attempts": "1", "X-Olp-Route-Revision": revision, "X-Olp-Provider": vendor})
	ids = append(ids, created["id"].(string))

	// Every job is still in progress, so the list polls all four of them at once.
	polled := f.upstream.getCalls.Load()
	resp, listed, env := call(http.MethodGet, "/v1/videos?limit=20", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d body %v", resp.StatusCode, listed)
	}
	polls := f.upstream.getCalls.Load() - polled
	if polls != int64(len(ids)) {
		t.Fatalf("the list polled %d jobs, want %d", polls, len(ids))
	}
	// The attempts are the polls the list made, whichever order they finished in,
	// and one provider served them all. A list has no route revision.
	wantMetadata("list", resp, env, map[string]string{"X-Olp-Attempts": strconv.FormatInt(polls, 10), "X-Olp-Provider": vendor})
	if len(env.Attempts) != len(ids) {
		t.Fatalf("the list recorded %d attempts, want %d", len(env.Attempts), len(ids))
	}

	// A list that has nothing to poll makes no attempt, and has nothing to say.
	f.upstream.getStatus.Store("completed")
	if resp, _, _ = call(http.MethodGet, "/v1/videos?limit=20", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("list: status %d", resp.StatusCode)
	}
	resp, _, env = call(http.MethodGet, "/v1/videos?limit=20", "", nil)
	if resp.StatusCode != http.StatusOK || len(env.Attempts) != 0 || len(metadata(resp)) != 0 {
		t.Fatalf("a list of finished jobs: status %d, %d attempts, metadata %v", resp.StatusCode, len(env.Attempts), metadata(resp))
	}

	// A call on one job is its one pinned attempt, and its answer comes from the
	// job's own record, so no route revision.
	job := "/v1/videos/" + ids[0]
	want := map[string]string{"X-Olp-Attempts": "1", "X-Olp-Provider": vendor}
	resp, got, env := call(http.MethodGet, job, "", nil)
	if resp.StatusCode != http.StatusOK || got["status"] != "completed" {
		t.Fatalf("get: status %d body %v", resp.StatusCode, got)
	}
	wantMetadata("get", resp, env, want)
	resp, _, env = call(http.MethodGet, job+"/content", "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("content: status %d", resp.StatusCode)
	}
	wantMetadata("content", resp, env, want)
	resp, got, env = call(http.MethodDelete, job, "", nil)
	if resp.StatusCode != http.StatusOK || got["deleted"] != true {
		t.Fatalf("delete: status %d body %v", resp.StatusCode, got)
	}
	wantMetadata("delete", resp, env, want)

	// The tombstone answers without an upstream call: no attempt, no metadata.
	resp, _, env = call(http.MethodDelete, job, "", nil)
	if resp.StatusCode != http.StatusOK || len(env.Attempts) != 0 || len(metadata(resp)) != 0 {
		t.Fatalf("repeat delete: status %d, %d attempts, metadata %v", resp.StatusCode, len(env.Attempts), metadata(resp))
	}
}

// TestIntegrationVideoJobCallsAreAdmittedForAKeyWithATokenLimit proves a key with
// a requests and a tokens limit that can create a job can read it back: a job
// call carries no prompt, and used to reserve nothing, which a token limit refuses
// as invalid, and the gateway answered as it does an outage, for ever. Every call
// carries the allowance of the key's window, counting down.
func TestIntegrationVideoJobCallsAreAdmittedForAKeyWithATokenLimit(t *testing.T) {
	f := seedMediaFixture(t, "none", false)
	client, namespace := mediaValkey(t)
	limiter, err := limits.New(client, namespace)
	if err != nil {
		t.Fatal(err)
	}
	f.gateway.Admission = NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, f.log)
	authority := f.rt.keys[f.bearer]
	authority.LookupID = strings.ReplaceAll(uuid.NewString(), "-", "")
	requests, tokens := int64(100), int64(1_000_000)
	authority.Policy.RequestsPerMinute, authority.Policy.TokensPerMinute = &requests, &tokens
	f.rt.keys[f.bearer] = authority

	call := func(method, path, contentType string, body io.Reader) (*http.Response, map[string]any) {
		t.Helper()
		resp := f.call(t, method, path, contentType, body)
		var out map[string]any
		if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			out = decodeJSON(t, resp)
		} else {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		return resp, out
	}
	// The counts below are those of one fixed minute.
	mediaSettleInMinute(t, client, 10*time.Second)
	resp, created := call(http.MethodPost, "/v1/videos", videoCreateContentType, strings.NewReader(videoCreateBody))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: status %d body %v", resp.StatusCode, created)
	}
	job := "/v1/videos/" + created["id"].(string)
	f.upstream.getStatus.Store("completed")
	for i, tc := range []struct{ name, method, path string }{
		{"list", http.MethodGet, "/v1/videos?limit=5"},
		{"get", http.MethodGet, job},
		{"content", http.MethodGet, job + "/content"},
		{"delete", http.MethodDelete, job},
	} {
		resp, body := call(tc.method, tc.path, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d body %v", tc.name, resp.StatusCode, body)
		}
		// The create was the first request of the window.
		if got, want := resp.Header.Get("X-Ratelimit-Remaining-Requests"), strconv.Itoa(98-i); got != want {
			t.Errorf("%s: remaining requests = %q, want %s (headers %v)", tc.name, got, want, resp.Header)
		}
		if resp.Header.Get("X-Ratelimit-Limit-Requests") != "100" || resp.Header.Get("X-Ratelimit-Limit-Tokens") != "1000000" || resp.Header.Get("X-Ratelimit-Remaining-Tokens") == "" {
			t.Errorf("%s: the allowance of the key is missing from %v", tc.name, resp.Header)
		}
	}
}
