package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/routing"
)

const sessionGolden = "testdata/sessions.golden.json"

type sessionStep struct {
	read  bool
	write string
}

type sessionSurface struct {
	name      string
	path      string
	client    string
	final     string
	strict    bool
	configure func(*routing.Provider, string)
	mock      string
	upstream  []sessionStep
}

func sessionSurfaces() []sessionSurface {
	done := `{"type":"response.done","response":{"id":"r1","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`
	return []sessionSurface{
		{
			name: "openai realtime", path: "/v1/realtime?model=" + routeSlug, final: "response.done", mock: "a",
			configure: func(p *routing.Provider, _ string) {
				p.Kind = "azure_openai"
				p.Capabilities = []routing.Capability{{Model: modelA, Operation: "realtime", Surface: "openai", Mode: "realtime"}}
			},
			upstream: []sessionStep{{write: done}},
		},
		{
			name: "gemini live", path: "/ws/" + connectors.GeminiLiveMethod, final: "usageMetadata", mock: "ws",
			client: `{"setup":{"model":"models/` + routeSlug + `"}}`,
			configure: func(p *routing.Provider, upstream string) {
				p.Kind, p.ProfileID, p.ProfileRevision = "gemini", "gemini-live", connectors.ProfileRevision
				p.Endpoint = upstream + "/v1beta"
				p.Capabilities = []routing.Capability{{Model: modelA, Operation: "realtime", Surface: "gemini", Mode: "realtime"}}
			},
			upstream: []sessionStep{
				{read: true}, {write: `{"setupComplete":{}}`},
				{write: `{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":11,"responseTokenCount":7,"totalTokenCount":18}}`},
			},
		},
		{
			name: "native duplex", path: "/native/xai-realtime/sessions/" + routeSlug, final: "response.done", mock: "a", strict: true,
			client: `{"type":"session.update","session":{"type":"realtime","voice":"eve"}}`,
			configure: func(p *routing.Provider, _ string) {
				p.ProfileID, p.ProfileRevision, p.VendorID = "xai-realtime", connectors.ProfileRevision, "xai"
				p.Capabilities = []routing.Capability{{Model: modelA, Operation: "realtime", Surface: "native", Mode: "realtime"}}
			},
			upstream: []sessionStep{{write: `{"type":"session.created","session":{"id":"sess-upstream"}}`}, {read: true}, {write: done}},
		},
	}
}

type sessionCondition struct {
	name           string
	limited        bool
	upstreamCloses bool
}

type sessionRecord struct {
	Handshake int                `json:"handshake"`
	Refusal   any                `json:"refusal,omitempty"`
	Received  []any              `json:"received"`
	Close     int                `json:"refusal_close,omitempty"`
	Reason    string             `json:"refusal_reason,omitempty"`
	Upstream  []any              `json:"upstream"`
	Dials     int                `json:"dials"`
	Envelopes []recordedEnvelope `json:"envelopes"`
}

func (s sessionSurface) publish(t *testing.T, h *surfaceHarness) {
	t.Helper()
	shape := func(snapshot *routing.Snapshot) {
		for id, provider := range snapshot.Providers {
			if provider.Name != "a" {
				continue
			}
			s.configure(&provider, h.upstream.URL)
			snapshot.Providers[id] = provider
		}
		route := snapshot.Routes[routeSlug]
		route.Operations, route.Targets = []string{"realtime"}, route.Targets[:1]
		snapshot.Routes[routeSlug] = route
	}
	if s.strict {
		strictRelease(t, h.harness, shape)
		return
	}
	h.republish(func(snapshot *routing.Snapshot, _, _ routing.Provider) { shape(snapshot) })
}

func runSessionCase(t *testing.T, surface sessionSurface, condition sessionCondition) sessionRecord {
	t.Helper()
	h := newSurfaceHarness(t, condition.limited)
	surface.publish(t, h)
	var mu sync.Mutex
	upstream := []any{}
	h.mock.set(surface.mock, func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "")
		for _, step := range surface.upstream {
			if !step.read {
				if conn.Write(r.Context(), websocket.MessageText, []byte(step.write)) != nil {
					return
				}
				continue
			}
			_, frame, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			mu.Lock()
			upstream = append(upstream, h.stable(frame))
			mu.Unlock()
		}
		for !condition.upstreamCloses {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	record := sessionRecord{Received: []any{}}
	var refusal error
	conn, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(h.server.URL, "http")+surface.path, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + fullKey}}})
	if response != nil {
		record.Handshake = response.StatusCode
	}
	if err != nil {
		if response != nil {
			raw, _ := io.ReadAll(response.Body)
			record.Refusal = h.stable(raw)
		}
	} else {
		defer conn.CloseNow()
		if surface.client != "" {
			if err := conn.Write(ctx, websocket.MessageText, []byte(surface.client)); err != nil {
				t.Fatal(err)
			}
		}
		for {
			_, frame, err := conn.Read(ctx)
			if err != nil {
				refusal = err
				break
			}
			record.Received = append(record.Received, h.stable(frame))
			if !condition.upstreamCloses && strings.Contains(string(frame), surface.final) {
				conn.Close(websocket.StatusNormalClosure, "")
				break
			}
		}
	}
	select {
	case <-h.returned:
	case <-ctx.Done():
		t.Fatal("session handler did not return")
	}
	mu.Lock()
	defer mu.Unlock()
	record.Upstream, record.Dials, record.Envelopes = upstream, h.mock.count(surface.mock), h.envelopes()
	if record.Dials == 0 && refusal != nil {
		record.Close, record.Reason = int(websocket.CloseStatus(refusal)), closeReason(refusal)
	}
	return record
}

func closeReason(err error) string {
	var closed websocket.CloseError
	if errors.As(err, &closed) {
		return closed.Reason
	}
	return ""
}

func TestSessionsGolden(t *testing.T) {
	conditions := []sessionCondition{{name: "client closes"}, {name: "upstream closes", upstreamCloses: true}, {name: "limits unavailable", limited: true}}
	cases := []goldenCase{}
	for _, surface := range sessionSurfaces() {
		for _, condition := range conditions {
			cases = append(cases, goldenCase{surface.name + ", " + condition.name, func(t *testing.T) any { return runSessionCase(t, surface, condition) }})
		}
	}
	checkGolden(t, sessionGolden, cases)
}
