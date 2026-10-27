package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/observability"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
)

const (
	webrtcOfferSDP  = "v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\na=olp-test-marker-offer\r\n"
	webrtcAnswerSDP = "v=0\r\no=- 3 4 IN IP4 127.0.0.1\r\ns=-\r\na=olp-test-marker-answer\r\n"
)

func TestWebRTCAccumulateUsageKeepsModalityShares(t *testing.T) {
	share := func(v int64) *int64 { return &v }
	sess := &webrtcSession{}
	sess.accumulateUsage(&wire.Usage{
		InputTokens: 100, OutputTokens: 40,
		CachedInputTokens: share(10), AudioInputTokens: share(60),
		CachedAudioInputTokens: share(5), AudioOutputTokens: share(30),
		ImageInputTokens: share(8), CachedImageInputTokens: share(2),
	})
	sess.accumulateUsage(&wire.Usage{
		InputTokens: 50, OutputTokens: 20,
		CachedInputTokens: share(4), AudioInputTokens: share(25),
		CachedAudioInputTokens: share(1), AudioOutputTokens: share(10),
		ImageInputTokens: share(3), CachedImageInputTokens: share(1),
	})
	u := sess.usage
	if !sess.haveUsage || u.InputTokens != 150 || u.OutputTokens != 60 {
		t.Fatalf("totals = %+v haveUsage=%v", u, sess.haveUsage)
	}
	for name, got := range map[string]*int64{
		"cached": u.CachedInputTokens, "audioIn": u.AudioInputTokens,
		"cachedAudioIn": u.CachedAudioInputTokens, "audioOut": u.AudioOutputTokens,
		"imageIn": u.ImageInputTokens, "cachedImageIn": u.CachedImageInputTokens,
	} {
		if got == nil {
			t.Fatalf("%s share dropped", name)
		}
	}
	if *u.AudioInputTokens != 85 || *u.AudioOutputTokens != 40 || *u.ImageInputTokens != 11 || *u.CachedAudioInputTokens != 6 || *u.CachedImageInputTokens != 3 || *u.CachedInputTokens != 14 {
		t.Fatalf("modality shares = %+v", u)
	}
}

type webrtcMock struct {
	mu          sync.Mutex
	mode        string
	creates     int
	hangups     int
	controls    int
	session     []byte
	hangupSeen  chan struct{}
	createSeen  chan struct{}
	release     chan struct{}
	sessionSeen int
}

func newWebRTCMock(mode string) *webrtcMock {
	return &webrtcMock{mode: mode, hangupSeen: make(chan struct{}, 4), createSeen: make(chan struct{}, 4), release: make(chan struct{})}
}

func (m *webrtcMock) counts() (int, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.creates, m.controls, m.hangups
}

func (m *webrtcMock) handler(t *testing.T, registeredAtCreate func() bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/realtime/calls"):
			if r.Header.Get("Authorization") != "Bearer "+secretA {
				t.Errorf("calls credential = %q", r.Header.Get("Authorization"))
			}
			if registeredAtCreate != nil {
				m.mu.Lock()
				if registeredAtCreate() {
					m.sessionSeen++
				}
				m.mu.Unlock()
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("upstream multipart: %v", err)
				http.Error(w, "bad multipart", 400)
				return
			}
			if got := r.FormValue("sdp"); got != webrtcOfferSDP {
				t.Errorf("upstream sdp changed: %q", got)
			}
			m.mu.Lock()
			m.creates++
			m.session = []byte(r.FormValue("session"))
			mode := m.mode
			m.mu.Unlock()
			m.createSeen <- struct{}{}
			if mode == "slow" {
				<-m.release
			}
			if mode == "reject" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":{"message":"invalid session","type":"invalid_request_error","code":"invalid_value"}}`)
				return
			}
			if mode != "noloc" {
				w.Header().Set("Location", "/v1/realtime/calls/rtc_test1")
			}
			if mode == "truncated" {
				w.Header().Set("Content-Length", "1024")
			}
			w.Header().Set("Content-Type", "application/sdp")
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, webrtcAnswerSDP)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/realtime"):
			if r.URL.Query().Get("call_id") != "rtc_test1" {
				t.Errorf("control call_id = %q", r.URL.Query().Get("call_id"))
				http.Error(w, "missing call", 404)
				return
			}
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			m.mu.Lock()
			m.controls++
			mode := m.mode
			m.mu.Unlock()
			if mode == "drop" {
				_ = conn.CloseNow()
				return
			}
			defer conn.CloseNow()
			_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.done","response":{"id":"resp_1","usage":{"input_tokens":3,"output_tokens":2,"input_token_details":{"cached_tokens":1}}}}`))
			if mode == "emptyusage" {
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.done","response":{"id":"resp_2","usage":{}}}`))
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			if mode == "transcribe" {
				_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"conversation.item.input_audio_transcription.completed","transcript":"hello"}`))
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			if mode == "close" {
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			if mode == "hold" {
				<-m.release
				_ = conn.Close(websocket.StatusNormalClosure, "")
				return
			}
			_, _, _ = conn.Read(r.Context())
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/hangup"):
			m.mu.Lock()
			m.hangups++
			mode := m.mode
			m.mu.Unlock()
			m.hangupSeen <- struct{}{}
			if mode == "drop" {
				http.Error(w, "still running", 500)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}
}

func webrtcHarness(t *testing.T, mock *webrtcMock, withWebRTC bool) *harness {
	t.Helper()
	return webrtcHarnessCfg(t, mock, withWebRTC, Config{})
}

func webrtcHarnessCfg(t *testing.T, mock *webrtcMock, withWebRTC bool, cfg Config) *harness {
	t.Helper()
	return webrtcHarnessBodyLimit(t, mock, withWebRTC, cfg, nil)
}

func webrtcHarnessBodyLimit(t *testing.T, mock *webrtcMock, withWebRTC bool, cfg Config, maxBodyBytes *int64) *harness {
	t.Helper()
	var h *harness
	h = strictHarnessCfg(t, cfg, func(snapshot *routing.Snapshot) {
		for id, provider := range snapshot.Providers {
			if provider.Name != "a" {
				continue
			}
			provider.Kind = "openai"
			provider.ProfileID = "openai-responses"
			provider.ProfileRevision = connectors.ProfileRevision
			provider.VendorID = "openai"
			provider.Capabilities = append(provider.Capabilities, routing.Capability{Model: modelA, Operation: "realtime", Surface: "openai", Mode: "realtime"})
			if withWebRTC {
				provider.Capabilities = append(provider.Capabilities, routing.Capability{Model: modelA, Operation: "realtime", Surface: "openai", Mode: "webrtc"})
			}
			snapshot.Providers[id] = provider
		}
		route := snapshot.Routes[routeSlug]
		route.Operations = []string{"generation", "realtime"}
		route.Targets = route.Targets[:1]
		route.MaxBodyBytes = maxBodyBytes
		snapshot.Routes[routeSlug] = route
	})
	h.mock.set("a", mock.handler(t, func() bool {
		return h != nil && h.gateway.webrtcSessionCount() > 0
	}))
	return h
}

func webrtcCreateBody(t *testing.T, session string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	sdpPart, err := writer.CreatePart(textproto.MIMEHeader{
		"Content-Disposition": {`form-data; name="sdp"`},
		"Content-Type":        {"application/sdp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(sdpPart, webrtcOfferSDP); err != nil {
		t.Fatal(err)
	}
	if session != "" {
		part, err := writer.CreatePart(textproto.MIMEHeader{
			"Content-Disposition": {`form-data; name="session"`},
			"Content-Type":        {"application/json"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, session); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), writer.FormDataContentType()
}

func webrtcCreate(t *testing.T, h *harness, key, session string) *http.Response {
	t.Helper()
	body, contentType := webrtcCreateBody(t, session)
	return h.do(t.Context(), http.MethodPost, "/v1/realtime/calls", key, body, map[string]string{"Content-Type": contentType})
}

func webrtcCreateOrigin(t *testing.T, h *harness, key, origin string) *http.Response {
	t.Helper()
	body, contentType := webrtcCreateBody(t, `{"type":"realtime","model":"`+routeSlug+`"}`)
	return h.do(t.Context(), http.MethodPost, "/v1/realtime/calls", key, body, map[string]string{"Content-Type": contentType, "Origin": origin})
}

func waitEnvelopes(t *testing.T, h *harness, n int) []Envelope {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.sink.mu.Lock()
		if len(h.sink.envs) >= n {
			envs := append([]Envelope(nil), h.sink.envs...)
			h.sink.mu.Unlock()
			return envs
		}
		h.sink.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("wanted %d envelopes", n)
	return nil
}

func sessionEnvelope(t *testing.T, envs []Envelope) Envelope {
	t.Helper()
	for _, env := range envs {
		if env.Mode == "webrtc" && len(env.Attempts) > 0 {
			return env
		}
	}
	t.Fatalf("no webrtc session envelope in %+v", envs)
	return Envelope{}
}

func TestWebRTCCreateOnWebRTCOnlyTargetPublishesAndServes(t *testing.T) {
	mock := newWebRTCMock("close")
	var h *harness
	h = strictHarnessCfg(t, Config{}, func(snapshot *routing.Snapshot) {
		for id, provider := range snapshot.Providers {
			if provider.Name != "a" {
				continue
			}
			provider.Kind = "openai"
			provider.ProfileID = "openai-responses"
			provider.ProfileRevision = connectors.ProfileRevision
			provider.VendorID = "openai"
			provider.Capabilities = append(provider.Capabilities, routing.Capability{Model: modelA, Operation: "realtime", Surface: "openai", Mode: "webrtc"})
			snapshot.Providers[id] = provider
		}
		route := snapshot.Routes[routeSlug]
		route.Operations = []string{"generation", "realtime"}
		route.Targets = route.Targets[:1]
		snapshot.Routes[routeSlug] = route
	})
	h.mock.set("a", mock.handler(t, func() bool {
		return h != nil && h.gateway.webrtcSessionCount() > 0
	}))
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(body) != webrtcAnswerSDP {
		t.Fatalf("webrtc-only target create status=%d body=%q", resp.StatusCode, body)
	}
	_ = sessionEnvelope(t, waitEnvelopes(t, h, 1))
}

func TestWebRTCTokenCappedKeyRefusedBeforeDispatch(t *testing.T) {
	mock := newWebRTCMock("usage")
	h := webrtcHarness(t, mock, true)
	authority := h.rt.keys[fullKey]
	authority.Policy.TokensPerMinute = int64ptr(1000)
	h.rt.keys[fullKey] = authority
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 400 || !strings.Contains(string(body), "unbounded_work_budget") {
		t.Fatalf("token-capped create status=%d body=%q", resp.StatusCode, body)
	}
	if creates, _, _ := mock.counts(); creates != 0 {
		t.Fatal("provider session created for a token-capped key")
	}
}

func TestWebRTCCostCappedKeyRefusedBeforeDispatch(t *testing.T) {
	mock := newWebRTCMock("usage")
	h := webrtcHarness(t, mock, true)
	authority := h.rt.keys[fullKey]
	authority.Policy.DailyCostLimit = stringptr("10")
	h.rt.keys[fullKey] = authority
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 400 || !strings.Contains(string(body), "unbounded_work_budget") {
		t.Fatalf("cost-capped create status=%d body=%q", resp.StatusCode, body)
	}
	if creates, _, _ := mock.counts(); creates != 0 {
		t.Fatal("provider session created for a cost-capped key")
	}
}

func TestWebRTCCreateReservesBeforeProviderAndSettlesObservedUsageOnce(t *testing.T) {
	mock := newWebRTCMock("close")
	h := webrtcHarness(t, mock, true)
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(body) != webrtcAnswerSDP {
		t.Fatalf("create status=%d body=%q", resp.StatusCode, body)
	}
	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "/v1/realtime/calls/") || strings.Contains(location, "rtc_test1") {
		t.Fatalf("location leaks provider call id or is malformed: %q", location)
	}
	if strings.Contains(string(body), "client_secret") || strings.Contains(location, "secret") {
		t.Fatal("raw client secret material in response")
	}
	// The dialer can receive the handshake before the mock counts it.
	// Settlement synchronizes the assertions with its sideband frames.
	envs := waitEnvelopes(t, h, 1)
	creates, controls, _ := mock.counts()
	if creates != 1 || controls != 1 {
		t.Fatalf("creates=%d controls=%d, control must exist before admission completes", creates, controls)
	}
	mock.mu.Lock()
	if mock.sessionSeen != 1 {
		t.Fatalf("provider session was created before the gateway reserved its session record")
	}
	var session map[string]any
	if err := json.Unmarshal(mock.session, &session); err != nil || session["model"] != modelA {
		t.Fatalf("upstream session model not bound: %s err=%v", mock.session, err)
	}
	mock.mu.Unlock()

	time.Sleep(50 * time.Millisecond)
	h.sink.mu.Lock()
	if len(h.sink.envs) != 1 {
		t.Fatalf("settlement envelopes = %d, want one session envelope only", len(h.sink.envs))
	}
	h.sink.mu.Unlock()
	env := sessionEnvelope(t, envs)
	if _, _, hangups := mock.counts(); hangups != 1 {
		t.Fatalf("hangups=%d, normal sideband close must hang up the provider call before settling", hangups)
	}
	fact := env.Attempts[len(env.Attempts)-1]
	if fact.Usage == nil || fact.Usage.InputTokens != 3 || fact.Usage.OutputTokens != 2 || !fact.UsageObserved || fact.BillingUncertain {
		t.Fatalf("settled fact = %+v", fact)
	}
	encoded, _ := json.Marshal(env)
	if strings.Contains(string(encoded), "olp-test-marker") || strings.Contains(string(encoded), secretA) {
		t.Fatalf("SDP or credential in durable envelope: %s", encoded)
	}
}

func TestWebRTCCreateBrowserOriginPolicy(t *testing.T) {
	t.Run("same-origin browser creation succeeds without a CORS allowlist", func(t *testing.T) {
		mock := newWebRTCMock("close")
		h := webrtcHarness(t, mock, true)
		resp := webrtcCreateOrigin(t, h, fullKey, h.server.URL)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated || string(body) != webrtcAnswerSDP {
			t.Fatalf("same-origin create status=%d body=%q", resp.StatusCode, body)
		}
		if creates, _, _ := mock.counts(); creates != 1 {
			t.Fatalf("creates=%d, want the provider session dispatched once", creates)
		}
		_ = sessionEnvelope(t, waitEnvelopes(t, h, 1))
	})
	t.Run("allowlisted cross-origin browser creation succeeds", func(t *testing.T) {
		mock := newWebRTCMock("close")
		h := webrtcHarnessCfg(t, mock, true, Config{CORSAllowedOrigins: []string{"https://app.example"}})
		resp := webrtcCreateOrigin(t, h, fullKey, "https://app.example")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated || string(body) != webrtcAnswerSDP {
			t.Fatalf("allowlisted create status=%d body=%q", resp.StatusCode, body)
		}
		if creates, _, _ := mock.counts(); creates != 1 {
			t.Fatalf("creates=%d, want the provider session dispatched once", creates)
		}
		_ = sessionEnvelope(t, waitEnvelopes(t, h, 1))
	})
	t.Run("foreign origin is refused before provider dispatch", func(t *testing.T) {
		mock := newWebRTCMock("close")
		h := webrtcHarness(t, mock, true)
		resp := webrtcCreateOrigin(t, h, fullKey, "https://evil.example")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "origin_forbidden") {
			t.Fatalf("foreign create status=%d body=%q", resp.StatusCode, body)
		}
		if creates, _, _ := mock.counts(); creates != 0 {
			t.Fatal("dispatched for a foreign origin")
		}
	})
}

func TestWebRTCRefusalsDispatchNothing(t *testing.T) {
	t.Run("unauthenticated", func(t *testing.T) {
		mock := newWebRTCMock("usage")
		h := webrtcHarness(t, mock, true)
		resp := webrtcCreate(t, h, "", `{"type":"realtime","model":"`+routeSlug+`"}`)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		if creates, _, _ := mock.counts(); creates != 0 {
			t.Fatal("dispatched without a key")
		}
	})
	t.Run("key without route", func(t *testing.T) {
		mock := newWebRTCMock("usage")
		h := webrtcHarness(t, mock, true)
		resp := webrtcCreate(t, h, otherKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status=%d", resp.StatusCode)
		}
		if creates, _, _ := mock.counts(); creates != 0 {
			t.Fatal("dispatched for a forbidden route")
		}
	})
	t.Run("session model mismatch", func(t *testing.T) {
		mock := newWebRTCMock("usage")
		h := webrtcHarness(t, mock, true)
		resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"someone-elses-route"}`)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			t.Fatal("admitted a session naming another route")
		}
		if creates, _, _ := mock.counts(); creates != 0 {
			t.Fatal("dispatched on model mismatch")
		}
	})
	t.Run("websocket certification does not imply webrtc", func(t *testing.T) {
		mock := newWebRTCMock("usage")
		h := webrtcHarness(t, mock, false)
		resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			t.Fatal("admitted webrtc on a websocket-only tuple")
		}
		if creates, _, _ := mock.counts(); creates != 0 {
			t.Fatal("dispatched without webrtc certification")
		}
	})
	t.Run("client secret request refused", func(t *testing.T) {
		mock := newWebRTCMock("usage")
		h := webrtcHarness(t, mock, true)
		resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`","client_secret":{"value":"x"}}`)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusCreated {
			t.Fatal("admitted caller-supplied client secret material")
		}
		if creates, _, _ := mock.counts(); creates != 0 {
			t.Fatal("dispatched with client secret material")
		}
	})
}

func TestWebRTCRevokedKeyClosesSession(t *testing.T) {
	mock := newWebRTCMock("usage")
	h := webrtcHarnessCfg(t, mock, true, Config{WebRTCReauth: 20 * time.Millisecond, WebRTCHeartbeat: time.Hour})
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	h.rt.mu.Lock()
	delete(h.rt.keys, fullKey)
	h.rt.mu.Unlock()
	select {
	case <-mock.hangupSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("revocation did not close the provider session")
	}
	env := sessionEnvelope(t, waitEnvelopes(t, h, 1))
	fact := env.Attempts[len(env.Attempts)-1]
	if !fact.BillingUncertain {
		t.Fatalf("revoked session must not settle as complete: %+v", fact)
	}
}

func TestWebRTCCreationHonorsDeploymentBodyLimit(t *testing.T) {
	mock := newWebRTCMock("usage")
	h := webrtcHarnessCfg(t, mock, true, Config{MaxBodyBytes: 16})
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("oversized creation admitted: %d", resp.StatusCode)
	}
	if creates, _, _ := mock.counts(); creates != 0 {
		t.Fatal("provider session created for an oversized request")
	}
}

func TestWebRTCCreationHonorsRouteBodyLimit(t *testing.T) {
	form, contentType := webrtcCreateBody(t, `{"type":"realtime","model":"`+routeSlug+`"}`)
	for _, offer := range []struct {
		name        string
		body        []byte
		contentType string
	}{
		{"sdp", []byte(webrtcOfferSDP), "application/sdp"},
		{"multipart", form, contentType},
		{"multipart epilogue", append(bytes.Clone(form), bytes.Repeat([]byte("x"), 8192)...), contentType},
	} {
		for _, chunked := range []bool{false, true} {
			for _, spare := range []int64{-1, 0, 1} {
				t.Run(fmt.Sprintf("%s/chunked=%t/spare=%d", offer.name, chunked, spare), func(t *testing.T) {
					mock := newWebRTCMock("close")
					limit := int64(len(offer.body)) + spare
					h := webrtcHarnessBodyLimit(t, mock, true, Config{}, &limit)
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.server.URL+"/v1/realtime/calls?model="+routeSlug, bytes.NewReader(offer.body))
					if err != nil {
						t.Fatal(err)
					}
					if chunked {
						req.ContentLength = -1
					}
					req.Header.Set("Authorization", "Bearer "+fullKey)
					req.Header.Set("Content-Type", offer.contentType)
					resp, err := h.server.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					body, readErr := io.ReadAll(resp.Body)
					resp.Body.Close()
					if readErr != nil {
						t.Fatal(readErr)
					}
					if spare < 0 {
						if creates, _, _ := mock.counts(); resp.StatusCode != http.StatusRequestEntityTooLarge || creates != 0 {
							t.Fatalf("oversized offer: status=%d creates=%d body=%s", resp.StatusCode, creates, body)
						}
						return
					}
					if creates, _, _ := mock.counts(); resp.StatusCode != http.StatusCreated || creates != 1 || string(body) != webrtcAnswerSDP {
						t.Fatalf("bounded offer: status=%d creates=%d body=%s", resp.StatusCode, creates, body)
					}
					_ = sessionEnvelope(t, waitEnvelopes(t, h, 1))
				})
			}
		}
	}
}

func TestWebRTCCallerRoutingExclusionDispatchesNothing(t *testing.T) {
	mock := newWebRTCMock("usage")
	h := webrtcHarness(t, mock, true)
	body, contentType := webrtcCreateBody(t, `{"type":"realtime","model":"`+routeSlug+`"}`)
	resp := h.do(t.Context(), http.MethodPost, "/v1/realtime/calls", fullKey, body, map[string]string{
		"Content-Type":  contentType,
		"X-OLP-Routing": `{"only":["provider:00000000-0000-0000-0000-000000000000"]}`,
	})
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("caller-excluded provider created a session")
	}
	if creates, _, _ := mock.counts(); creates != 0 {
		t.Fatal("provider session created despite caller routing exclusion")
	}
}

func TestWebRTCEmptyUsageCountersStayUncertain(t *testing.T) {
	mock := newWebRTCMock("emptyusage")
	h := webrtcHarness(t, mock, true)
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	env := sessionEnvelope(t, waitEnvelopes(t, h, 1))
	fact := env.Attempts[len(env.Attempts)-1]
	if !fact.BillingUncertain {
		t.Fatalf("counter-less terminal usage settled as complete: %+v", fact)
	}
}

func TestWebRTCInputTranscriptionRefusedAndObservedWorkUncertain(t *testing.T) {
	mock := newWebRTCMock("transcribe")
	h := webrtcHarness(t, mock, true)
	for _, session := range []string{
		`{"type":"realtime","model":"` + routeSlug + `","audio":{"input":{"transcription":{"model":"whisper-1"}}}}`,
		`{"type":"realtime","model":"` + routeSlug + `","input_audio_transcription":{"model":"whisper-1"}}`,
	} {
		resp := webrtcCreate(t, h, fullKey, session)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode < 400 || !strings.Contains(string(body), "unsupported_session_field") {
			t.Fatalf("transcription session admitted: %d %s", resp.StatusCode, body)
		}
	}
	if creates, _, _ := mock.counts(); creates != 0 {
		t.Fatal("provider session created for a refused transcription configuration")
	}
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	env := sessionEnvelope(t, waitEnvelopes(t, h, 3))
	fact := env.Attempts[len(env.Attempts)-1]
	if !fact.BillingUncertain {
		t.Fatalf("observed transcription work settled as complete: %+v", fact)
	}
}

func TestWebRTCClientCancellationDuringCreationHangsUpSession(t *testing.T) {
	mock := newWebRTCMock("slow")
	h := webrtcHarness(t, mock, true)
	body, contentType := webrtcCreateBody(t, `{"type":"realtime","model":"`+routeSlug+`"}`)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.server.URL+"/v1/realtime/calls", bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+fullKey)
		req.Header.Set("Content-Type", contentType)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp != nil && resp.Body != nil {
			resp.Body.Close()
		}
	}()
	select {
	case <-mock.createSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("provider creation never started")
	}
	cancel()
	close(mock.release)
	select {
	case <-mock.hangupSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("abandoned creation left the provider session established")
	}
	<-done
	_ = sessionEnvelope(t, waitEnvelopes(t, h, 1))
	if got := h.gateway.webrtcSessionCount(); got != 0 {
		t.Fatalf("orphaned sessions retained: %d", got)
	}
}

func TestWebRTCAddedCapClosesSessionAtReauthorization(t *testing.T) {
	mock := newWebRTCMock("usage")
	h := webrtcHarnessCfg(t, mock, true, Config{WebRTCReauth: 20 * time.Millisecond, WebRTCHeartbeat: time.Hour})
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	h.rt.mu.Lock()
	authority := h.rt.keys[fullKey]
	authority.Policy.TokensPerMinute = int64ptr(1000)
	h.rt.keys[fullKey] = authority
	h.rt.mu.Unlock()
	select {
	case <-mock.hangupSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("newly added token cap did not close the provider session")
	}
	_ = sessionEnvelope(t, waitEnvelopes(t, h, 1))
}

func TestWebRTCLostControlHoldsExposureAndRefusesNewWork(t *testing.T) {
	mock := newWebRTCMock("drop")
	h := webrtcHarness(t, mock, true)
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	env := sessionEnvelope(t, waitEnvelopes(t, h, 1))
	fact := env.Attempts[len(env.Attempts)-1]
	if !fact.BillingUncertain || fact.UsageComplete {
		t.Fatalf("lost control must settle uncertain, never complete or free: %+v", fact)
	}
	if _, _, hangups := mock.counts(); hangups == 0 {
		t.Fatal("lost control never attempted a provider close")
	}
	second := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	body, _ := io.ReadAll(second.Body)
	second.Body.Close()
	if second.StatusCode != http.StatusConflict || !strings.Contains(string(body), "webrtc_exposure_unresolved") {
		t.Fatalf("second create status=%d body=%s", second.StatusCode, body)
	}
	if creates, _, _ := mock.counts(); creates != 1 {
		t.Fatalf("new work dispatched against unresolved exposure: creates=%d", creates)
	}
}

func TestWebRTCMissingControlIdentitySettlesUncertainWithoutSDP(t *testing.T) {
	mock := newWebRTCMock("noloc")
	h := webrtcHarness(t, mock, true)
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "olp-test-marker") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if _, controls, _ := mock.counts(); controls != 0 {
		t.Fatal("control dialed without a call identity")
	}
	env := sessionEnvelope(t, waitEnvelopes(t, h, 1))
	if fact := env.Attempts[len(env.Attempts)-1]; !fact.BillingUncertain {
		t.Fatalf("uncontrollable session settled as certain: %+v", fact)
	}
}

func TestWebRTCSessionBoundClosesProviderSession(t *testing.T) {
	mock := newWebRTCMock("usage")
	h := webrtcHarnessCfg(t, mock, true, Config{WebRTCBound: 150 * time.Millisecond})
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status=%d", resp.StatusCode)
	}
	select {
	case <-mock.hangupSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("session bound never closed the provider session")
	}
	_ = sessionEnvelope(t, waitEnvelopes(t, h, 1))
}

func TestWebRTCTruncatedAnswerHangsUpCreatedCall(t *testing.T) {
	mock := newWebRTCMock("truncated")
	h := webrtcHarness(t, mock, true)
	resp := webrtcCreate(t, h, fullKey, `{"type":"realtime","model":"`+routeSlug+`"}`)
	resp.Body.Close()
	creates, controls, hangups := mock.counts()
	if resp.StatusCode != http.StatusBadGateway || creates != 1 || controls != 0 || hangups != 1 {
		t.Fatalf("unreadable answer left a call open: status=%d creates=%d controls=%d hangups=%d", resp.StatusCode, creates, controls, hangups)
	}
}

func TestWebRTCActiveSessionsRetainProcessCapacity(t *testing.T) {
	for _, middleware := range []bool{false, true} {
		t.Run(fmt.Sprint(middleware), func(t *testing.T) {
			mock := newWebRTCMock("hold")
			release := sync.OnceFunc(func() { close(mock.release) })
			defer release()
			h := webrtcHarnessCfg(t, mock, true, Config{MaxInFlight: 1})
			if middleware {
				mux := http.NewServeMux()
				h.gateway.Register(mux)
				admission := observability.PublicAdmission{Inference: h.gateway.AdmissionPool(), Management: observability.NewPool(1), InferenceEnabled: true, Reject: func(w http.ResponseWriter, r *http.Request, surface string) {
					writeSurfaceError(w, overloaded, surface)
				}}
				server := httptest.NewServer(admission.Wrap(mux))
				t.Cleanup(server.Close)
				h.server = server
			}
			session := `{"type":"realtime","model":"` + routeSlug + `"}`
			first := webrtcCreate(t, h, fullKey, session)
			io.Copy(io.Discard, first.Body)
			first.Body.Close()
			second := webrtcCreate(t, h, fullKey, session)
			second.Body.Close()
			beforeOrdinary := h.mock.count("a")
			ordinary := h.do(t.Context(), http.MethodPost, "/v1/responses", fullKey, []byte(`{"model":"`+routeSlug+`","input":"hello","max_output_tokens":16,"store":false}`), nil)
			ordinaryBody, _ := io.ReadAll(ordinary.Body)
			ordinary.Body.Close()
			if creates, _, _ := mock.counts(); first.StatusCode != http.StatusCreated || second.StatusCode != http.StatusServiceUnavailable || ordinary.StatusCode != http.StatusServiceUnavailable || h.mock.count("a") != beforeOrdinary || creates != 1 {
				t.Fatalf("active session lost shared capacity: first=%d second=%d ordinary=%d body=%s creates=%d", first.StatusCode, second.StatusCode, ordinary.StatusCode, ordinaryBody, creates)
			}
			release()
			deadline := time.Now().Add(time.Second)
			for {
				resp := webrtcCreate(t, h, fullKey, session)
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusCreated {
					break
				}
				if resp.StatusCode != http.StatusServiceUnavailable || time.Now().After(deadline) {
					t.Fatalf("settled session retained shared capacity: status=%d", resp.StatusCode)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}
