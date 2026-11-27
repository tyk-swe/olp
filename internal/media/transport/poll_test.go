package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/upstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type trackingReadCloser struct {
	reader io.Reader
	read   int64
	closed bool
}

func (t *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := t.reader.Read(p)
	t.read += int64(n)
	return n, err
}

func (t *trackingReadCloser) Close() error {
	t.closed = true
	return nil
}

type failingReadCloser struct{ err error }

func (f failingReadCloser) Read([]byte) (int, error) { return 0, f.err }
func (f failingReadCloser) Close() error             { return nil }

func newPollHarness(t *testing.T, rt roundTripFunc, endpoint string, targetFn func(*Target)) poller {
	t.Helper()
	policy := loopbackPolicy()
	client := &http.Client{Transport: rt}
	tr := New(Deps{
		Client:           client,
		Auth:             connectors.NewAuth(policy, func(string) string { return "" }),
		Egress:           policy,
		MaxResponseBytes: 4096,
		Log:              slog.New(slog.DiscardHandler),
	})
	target := Target{
		Config: connectors.Config{
			Endpoint: endpoint,
			AuthMode: connectors.AuthAPIKey,
		},
		Secret: []byte("sk-poll-secret"),
	}
	if targetFn != nil {
		targetFn(&target)
	}
	return poller{transport: tr, ctx: t.Context(), client: client, target: target}
}

func httpResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestPollerVisitAgainFinishedAndCleanup(t *testing.T) {
	t.Parallel()
	const base = "https://vendor.example.com"
	var requests []string
	var sawAuth, sawJSONBody bool
	var cleanupCalls int

	p := newPollHarness(t, func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.Method+" "+r.URL.String())
		switch r.URL.Path {
		case "/status":
			if r.Header.Get("Authorization") == "Bearer sk-poll-secret" {
				sawAuth = true
			}
			if len(requests) == 1 {
				return httpResponse(http.StatusOK, "application/json", `{"state":"pending"}`), nil
			}
			return httpResponse(http.StatusOK, "application/json", `{"state":"done","text":"final"}`), nil
		case "/cleanup":
			cleanupCalls++
			raw, _ := io.ReadAll(r.Body)
			if string(raw) == `{"delete":true}` && r.Header.Get("Content-Type") == "application/json" {
				sawJSONBody = true
			}
			return httpResponse(http.StatusInternalServerError, "text/plain", "cleanup ignored"), nil
		default:
			return httpResponse(http.StatusNotFound, "text/plain", "missing"), nil
		}
	}, base, nil)

	poll := &media.Poll{
		Domains: []string{"vendor.example.com"},
		Next: func(body []byte) (media.Step, *media.Error) {
			switch string(body) {
			case `{"state":"created"}`:
				return media.Visit{URL: base + "/status", Credentials: true}, nil
			case `{"state":"pending"}`:
				return media.Again{}, nil
			case `{"state":"done","text":"final"}`:
				return media.Finished{Cleanup: &media.Visit{
					Method:      http.MethodPost,
					URL:         base + "/cleanup",
					JSON:        []byte(`{"delete":true}`),
					Credentials: true,
				}}, nil
			default:
				return nil, media.Fail(http.StatusBadGateway, "unexpected", string(body))
			}
		},
	}

	got, fail := p.finish(poll, []byte(`{"state":"created"}`))
	if fail != nil {
		t.Fatalf("finish failed: %+v", fail)
	}
	if string(got) != `{"state":"done","text":"final"}` {
		t.Fatalf("final body = %q", got)
	}
	if !sawAuth || !sawJSONBody || cleanupCalls != 1 || len(requests) != 3 {
		t.Fatalf("unexpected trace: auth=%v json=%v cleanup=%d reqs=%v", sawAuth, sawJSONBody, cleanupCalls, requests)
	}
}

func TestPollerFetchAndTerminalMismatches(t *testing.T) {
	t.Parallel()
	const base = "https://vendor.example.com"

	p := newPollHarness(t, func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/good.png":
			return httpResponse(http.StatusOK, " Image/PNG; charset=binary", "PNG-BYTES"), nil
		case "/wrong.txt":
			return httpResponse(http.StatusOK, "text/plain", "not-an-image"), nil
		default:
			return httpResponse(http.StatusNotFound, "text/plain", "404"), nil
		}
	}, base, nil)

	asset, fail := p.fetch(media.Poll{
		Next: func([]byte) (media.Step, *media.Error) {
			return media.Fetch{URL: "https://cdn.other.example/good.png", Prefix: "image/"}, nil
		},
	}, []byte(`{}`), "missing image")
	if fail != nil || !asset.asset || string(asset.body) != "PNG-BYTES" {
		t.Fatalf("fetch = %+v / %+v", asset, fail)
	}

	_, fail = p.fetch(media.Poll{
		Next: func([]byte) (media.Step, *media.Error) {
			return media.Fetch{URL: base + "/wrong.txt", Prefix: "image/"}, nil
		},
	}, []byte(`{}`), "missing image")
	if fail == nil || fail.Class != upstream.Protocol {
		t.Fatalf("wrong content-type fetch = %+v, want Protocol", fail)
	}

	_, fail = p.fetch(media.Poll{
		Next: func([]byte) (media.Step, *media.Error) {
			return media.Finished{}, nil
		},
	}, []byte(`{}`), "no asset returned")
	if fail == nil || fail.Class != upstream.Protocol || fail.Detail != "no asset returned" {
		t.Fatalf("fetch ending in Finished = %+v, want missing detail", fail)
	}

	_, fail = p.finish(&media.Poll{
		Next: func([]byte) (media.Step, *media.Error) {
			return media.Fetch{URL: base + "/good.png", Prefix: "image/"}, nil
		},
	}, []byte(`{}`))
	if fail == nil || fail.Class != upstream.Protocol {
		t.Fatalf("finish ending in Fetch = %+v, want Protocol", fail)
	}
}

func TestPollerAgainGuardsAndMaxStepsCeiling(t *testing.T) {
	t.Parallel()
	const base = "https://vendor.example.com"

	t.Run("Again without prior Visit fails", func(t *testing.T) {
		t.Parallel()
		p := newPollHarness(t, func(*http.Request) (*http.Response, error) {
			t.Fatal("unexpected HTTP call")
			return nil, nil
		}, base, nil)
		_, fail := p.finish(&media.Poll{
			Next: func([]byte) (media.Step, *media.Error) { return media.Again{}, nil },
		}, []byte(`{}`))
		if fail == nil || fail.Class != upstream.Protocol {
			t.Fatalf("Again without Visit = %+v, want Protocol", fail)
		}
	})

	t.Run("Again after POST Visit fails", func(t *testing.T) {
		t.Parallel()
		p := newPollHarness(t, func(*http.Request) (*http.Response, error) {
			return httpResponse(http.StatusOK, "application/json", `{"step":2}`), nil
		}, base, nil)
		calls := 0
		_, fail := p.finish(&media.Poll{
			Next: func([]byte) (media.Step, *media.Error) {
				calls++
				if calls == 1 {
					return media.Visit{Method: http.MethodPost, URL: base + "/poll"}, nil
				}
				return media.Again{}, nil
			},
		}, []byte(`{}`))
		if fail == nil || fail.Class != upstream.Protocol {
			t.Fatalf("Again after POST = %+v, want Protocol", fail)
		}
	})

	t.Run("maxSteps ceiling terminates iteratively", func(t *testing.T) {
		t.Parallel()
		var visits atomic.Int64
		p := newPollHarness(t, func(*http.Request) (*http.Response, error) {
			visits.Add(1)
			return httpResponse(http.StatusOK, "application/json", `{"still":"running"}`), nil
		}, base, nil)
		_, fail := p.finish(&media.Poll{
			Next: func(body []byte) (media.Step, *media.Error) {
				if bytes.Equal(body, []byte(`{"init":true}`)) {
					return media.Visit{URL: base + "/poll"}, nil
				}
				return media.Again{}, nil
			},
		}, []byte(`{"init":true}`))
		if fail == nil || fail.Class != upstream.Timeout {
			t.Fatalf("maxSteps exhaustion = %+v, want Timeout", fail)
		}
		if visits.Load() != maxSteps {
			t.Fatalf("visits = %d, want %d", visits.Load(), maxSteps)
		}
	})

	t.Run("wait honors canceled context immediately", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		p := newPollHarness(t, func(*http.Request) (*http.Response, error) {
			t.Fatal("should not execute HTTP call after wait cancellation")
			return nil, nil
		}, base, nil)
		p.ctx = ctx
		_, fail := p.finish(&media.Poll{
			Next: func([]byte) (media.Step, *media.Error) {
				return media.Visit{URL: base + "/poll", Wait: time.Hour}, nil
			},
		}, []byte(`{}`))
		if fail == nil || fail.Class != upstream.Timeout {
			t.Fatalf("canceled wait = %+v, want Timeout", fail)
		}
	})
}

func TestPollerSecurityAndEgressGuards(t *testing.T) {
	t.Parallel()
	const base = "https://vendor.example.com"

	for _, tc := range []struct {
		name             string
		callerCredential bool
		visit            media.Visit
		domains          []string
		wantAllowed      bool
	}{
		{
			name:             "caller credential blocks cross origin even on listed domain",
			callerCredential: true,
			visit:            media.Visit{URL: "https://sub.vendor.example.com/status", Credentials: true},
			domains:          []string{"vendor.example.com"},
			wantAllowed:      false,
		},
		{
			name:             "caller credential allows same origin",
			callerCredential: true,
			visit:            media.Visit{URL: "https://vendor.example.com/status", Credentials: true},
			domains:          []string{"vendor.example.com"},
			wantAllowed:      true,
		},
		{
			name:             "caller credential allows uncredentialed cross origin https fetch",
			callerCredential: true,
			visit:            media.Visit{URL: "https://cdn.other.example/file.png", Credentials: false},
			domains:          []string{"vendor.example.com"},
			wantAllowed:      true,
		},
		{
			name:        "credentialed visit blocks unlisted domain",
			visit:       media.Visit{URL: "https://evil.example.net/steal", Credentials: true},
			domains:     []string{"vendor.example.com"},
			wantAllowed: false,
		},
		{
			name:        "credentialed visit allows listed subdomain",
			visit:       media.Visit{URL: "https://jobs.vendor.example.com/status", Credentials: true},
			domains:     []string{"vendor.example.com"},
			wantAllowed: true,
		},
		{
			name:        "blocks private metadata ip via egress policy",
			visit:       media.Visit{URL: "https://169.254.169.254/latest", Credentials: false},
			domains:     []string{"169.254.169.254"},
			wantAllowed: false,
		},
		{
			name:        "blocks embedded credentials in URL",
			visit:       media.Visit{URL: "https://user:pass@vendor.example.com/status", Credentials: false},
			domains:     []string{"vendor.example.com"},
			wantAllowed: false,
		},
		{
			name:        "blocks plain http to different host",
			visit:       media.Visit{URL: "http://other.example.com/status", Credentials: false},
			domains:     []string{"other.example.com"},
			wantAllowed: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var called bool
			p := newPollHarness(t, func(*http.Request) (*http.Response, error) {
				called = true
				return httpResponse(http.StatusOK, "application/json", `{"ok":true}`), nil
			}, base, func(target *Target) {
				if tc.callerCredential {
					*target = target.WithCaller([]byte("sk-caller"), &egress.Sensitive{})
				}
			})
			_, fail := p.visit(tc.visit, tc.domains)
			if tc.wantAllowed {
				if fail != nil || !called {
					t.Fatalf("expected allowed visit, got called=%v fail=%+v", called, fail)
				}
				return
			}
			if fail == nil || fail.Class != upstream.Protocol || called {
				t.Fatalf("expected Protocol rejection without HTTP call, got called=%v fail=%+v", called, fail)
			}
		})
	}
}

func TestPollerNon2xxDrainAndBodyErrors(t *testing.T) {
	t.Parallel()
	const base = "https://vendor.example.com"

	t.Run("non-2xx drains bounded body and closes for keep-alive reuse", func(t *testing.T) {
		t.Parallel()
		smallBody := &trackingReadCloser{reader: strings.NewReader("server overloaded")}
		p := newPollHarness(t, func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusBadGateway,
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       smallBody,
			}, nil
		}, base, nil)

		_, fail := p.visit(media.Visit{URL: base + "/poll"}, nil)
		if fail == nil || fail.Class != upstream.ServerError {
			t.Fatalf("non-2xx failure = %+v, want ServerError", fail)
		}
		if !smallBody.closed || smallBody.read != int64(len("server overloaded")) {
			t.Fatalf("body not drained/closed: read=%d closed=%v", smallBody.read, smallBody.closed)
		}

		hugeBody := &trackingReadCloser{reader: bytes.NewReader(bytes.Repeat([]byte("z"), errorBodyLimit*2))}
		pHuge := newPollHarness(t, func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     http.Header{"Content-Type": []string{"text/plain"}},
				Body:       hugeBody,
			}, nil
		}, base, nil)
		_, fail = pHuge.visit(media.Visit{URL: base + "/poll"}, nil)
		if fail == nil || fail.Class != upstream.ServerError {
			t.Fatalf("huge non-2xx failure = %+v, want ServerError", fail)
		}
		if !hugeBody.closed || hugeBody.read != errorBodyLimit {
			t.Fatalf("drain must be bounded to %d bytes: read=%d closed=%v", errorBodyLimit, hugeBody.read, hugeBody.closed)
		}
	})

	t.Run("read error classifies as Connect and oversized body classifies as Protocol", func(t *testing.T) {
		t.Parallel()
		pErr := newPollHarness(t, func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       failingReadCloser{err: errors.New("connection reset")},
			}, nil
		}, base, nil)
		_, fail := pErr.visit(media.Visit{URL: base + "/poll"}, nil)
		if fail == nil || fail.Class != upstream.Connect {
			t.Fatalf("read error = %+v, want Connect", fail)
		}

		pBig := newPollHarness(t, func(*http.Request) (*http.Response, error) {
			return httpResponse(http.StatusOK, "application/json", strings.Repeat("a", 5000)), nil
		}, base, nil)
		_, fail = pBig.visit(media.Visit{URL: base + "/poll"}, nil)
		if fail == nil || fail.Class != upstream.Protocol {
			t.Fatalf("oversized poll response = %+v, want Protocol", fail)
		}
	})
}
