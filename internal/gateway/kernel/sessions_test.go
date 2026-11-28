package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contracts/duplex"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
	"github.com/tyk-swe/olp/tests/harness/money"
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
	budgeted  bool
	configure func(*routing.Provider, string)
	mock      string
	upstream  []sessionStep
}

func sessionSurfaces() []sessionSurface {
	done := `{"type":"response.done","response":{"id":"r1","usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`
	return []sessionSurface{
		{
			name: "openai realtime", path: "/v1/realtime?model=" + routeSlug, final: "response.done", mock: "a", budgeted: true,
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
	Holds     []money.Hold       `json:"holds,omitempty"`
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
	h.price(t, surface.budgeted && !condition.limited)
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
	record.Upstream, record.Dials, record.Envelopes, record.Holds = upstream, h.mock.count(surface.mock), h.envelopes(), h.ledger.Drain()
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

func TestVertexNativeVideoUsage(t *testing.T) {
	t.Parallel()
	config := connectors.Config{
		Kind: "vertex_ai", VendorID: "google-vertex", ProfileID: "vertex-realtime", ProfileRevision: connectors.ProfileRevision,
		Endpoint:    "https://us-central1-aiplatform.googleapis.com/v1/projects/audit-project/locations/us-central1/publishers/google",
		CloudRegion: "us-central1", CloudProject: "audit-project", AuthMode: "service_account",
	}
	template, err := duplex.Compile(duplex.Config{Provider: config, Model: "audit-model"})
	if err != nil {
		t.Fatal(err)
	}
	rate := func(s string) *string { return &s }
	price := rates.NewRoutingPrice(rates.RoutingPrice{Price: rates.Price{InputPerMillion: rate("1"), CachedInputPerMillion: rate("0.5"), ImageInputPerMillion: rate("5"), CachedImageInputPerMillion: rate("2"), OutputPerMillion: rate("1")}})
	reference := usage.AttemptUsage{
		Evidence: usage.EvidenceObservedComplete,
		UsageQuantities: usage.UsageQuantities{
			InputTokens: new(int64(10)), OutputTokens: new(int64(2)), CachedInputTokens: new(int64(3)),
			ImageInputTokens: new(int64(7)), CachedImageInputTokens: new(int64(3)),
		},
	}
	expectedCost, priced := price.Cost(reference, time.Time{}, true, nil)
	if !priced {
		t.Fatal("reference usage is unpriced")
	}
	for _, test := range []struct{ name, prompt, cached string }{
		{"image", `{"modality":"IMAGE","tokenCount":7}`, `{"modality":"IMAGE","tokenCount":3}`},
		{"video", `{"modality":"VIDEO","tokenCount":7}`, `{"modality":"VIDEO","tokenCount":3}`},
		{"mixed", `{"modality":"IMAGE","tokenCount":2},{"modality":"VIDEO","tokenCount":5}`, `{"modality":"IMAGE","tokenCount":1},{"modality":"VIDEO","tokenCount":2}`},
	} {
		o := template.Observer(1 << 20)
		frame := fmt.Sprintf(`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":10,"responseTokenCount":2,"totalTokenCount":12,"cachedContentTokenCount":3,"promptTokensDetails":[{"modality":"TEXT","tokenCount":3},%s],"cacheTokensDetails":[%s]}}`, test.prompt, test.cached)
		if _, err := o.ServerFrame([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		if !o.Complete() {
			t.Fatalf("%s usage is incomplete", test.name)
		}
		got := o.Usage()
		if got.ImageInputTokens == nil || *got.ImageInputTokens != 7 || got.CachedImageInputTokens == nil || *got.CachedImageInputTokens != 3 {
			t.Errorf("%s visual usage = image %v, cached image %v, want 7 and 3", test.name, got.ImageInputTokens, got.CachedImageInputTokens)
		}
		cost, ok := price.Cost(usage.ObservedAttemptUsage(got, o.Complete(), "realtime"), time.Time{}, true, nil)
		if !ok {
			t.Fatalf("%s usage is unpriced", test.name)
		}
		if cost.String() != expectedCost.String() {
			t.Errorf("%s cost = %s, want image-equivalent cost %s", test.name, cost.String(), expectedCost.String())
		}
	}
	observer := template.Observer(1 << 20)
	audio := `{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":12,"responseTokenCount":5,"totalTokenCount":17,"cachedContentTokenCount":5,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":2},{"modality":"AUDIO","tokenCount":7}],"cacheTokensDetails":[{"modality":"AUDIO","tokenCount":1},{"modality":"AUDIO","tokenCount":4}],"responseTokensDetails":[{"modality":"AUDIO","tokenCount":3},{"modality":"AUDIO","tokenCount":2}]}}`
	if _, err := observer.ServerFrame([]byte(audio)); err != nil {
		t.Fatal(err)
	}
	got := observer.Usage()
	for _, check := range []struct {
		name string
		got  *int64
		want int64
	}{
		{"input", got.AudioInputTokens, 9},
		{"cached", got.CachedAudioInputTokens, 5},
		{"output", got.AudioOutputTokens, 5},
	} {
		if check.got == nil || *check.got != check.want {
			t.Errorf("%s audio usage = %v, want %d", check.name, check.got, check.want)
		}
	}
}

func settledSessionUsage(t *testing.T, record sessionRecord) *wire.Usage {
	t.Helper()
	if len(record.Envelopes) != 1 || len(record.Envelopes[0].Attempts) != 1 {
		t.Fatalf("envelopes = %+v, want one envelope with one attempt", record.Envelopes)
	}
	settled, ok := record.Envelopes[0].Attempts[0].Usage.(*wire.Usage)
	if !ok || settled == nil {
		t.Fatalf("settled usage = %#v, want observed usage", record.Envelopes[0].Attempts[0].Usage)
	}
	return settled
}

func assertUsageShare(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %d", name, got, want)
	}
}

func TestGeminiLiveModalitySharesSettle(t *testing.T) {
	surface := sessionSurfaces()[1]
	surface.upstream = []sessionStep{
		{read: true}, {write: `{"setupComplete":{}}`},
		{write: `{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":16,"responseTokenCount":5,"totalTokenCount":21,"cachedContentTokenCount":6,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":7},{"modality":"IMAGE","tokenCount":3}],"cacheTokensDetails":[{"modality":"AUDIO","tokenCount":2},{"modality":"IMAGE","tokenCount":4}],"responseTokensDetails":[{"modality":"AUDIO","tokenCount":1}]}}`},
	}
	settled := settledSessionUsage(t, runSessionCase(t, surface, sessionCondition{name: "modality shares"}))
	assertUsageShare(t, "audio input", settled.AudioInputTokens, 7)
	assertUsageShare(t, "image input", settled.ImageInputTokens, 3)
	assertUsageShare(t, "cached audio input", settled.CachedAudioInputTokens, 2)
	assertUsageShare(t, "cached image input", settled.CachedImageInputTokens, 4)
	assertUsageShare(t, "audio output", settled.AudioOutputTokens, 1)

}

func TestGeminiLiveUsageAccumulatesAcrossTurns(t *testing.T) {
	t.Parallel()
	surface := sessionSurfaces()[1]
	surface.final = `{"serverContent":{"turnComplete":true}}`
	surface.upstream = []sessionStep{
		{read: true}, {write: `{"setupComplete":{}}`},
		{write: `{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":10,"responseTokenCount":5,"toolUsePromptTokenCount":3,"thoughtsTokenCount":2,"cachedContentTokenCount":4,"totalTokenCount":12,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":6}],"cacheTokensDetails":[{"modality":"IMAGE","tokenCount":2}],"responseTokensDetails":[{"modality":"AUDIO","tokenCount":1}]}}`},
		{write: `{"usageMetadata":{"promptTokenCount":7,"responseTokenCount":3,"thoughtsTokenCount":1,"cachedContentTokenCount":2,"totalTokenCount":99,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":2}],"cacheTokensDetails":[{"modality":"IMAGE","tokenCount":1}]}}`},
		{write: `{"serverContent":{"turnComplete":true}}`},
	}
	settled := settledSessionUsage(t, runSessionCase(t, surface, sessionCondition{name: "two turns"}))
	const want = `{"input_tokens":20,"output_tokens":11,"total_tokens":119,"cached_input_tokens":6,"audio_input_tokens":8,"audio_output_tokens":1,"cached_image_input_tokens":3,"reasoning_tokens":3}`
	if got, err := json.Marshal(settled); err != nil || string(got) != want {
		t.Fatalf("usage = %s, want %s: %v", got, want, err)
	}
}

func TestRealtimeResponseDoneSettlesModalityShares(t *testing.T) {
	surface := sessionSurfaces()[0]
	surface.upstream = []sessionStep{{write: `{"type":"response.done","response":{"id":"r1","usage":{"input_tokens":100,"output_tokens":50,"total_tokens":150,"input_token_details":{"cached_tokens":40,"audio_tokens":30,"image_tokens":10,"cached_tokens_details":{"audio_tokens":12,"image_tokens":4}},"output_token_details":{"audio_tokens":20}}}}`}}
	settled := settledSessionUsage(t, runSessionCase(t, surface, sessionCondition{name: "modality shares"}))
	if settled.InputTokens != 100 || settled.OutputTokens != 50 {
		t.Errorf("settled tokens = input %d, output %d, want 100 and 50", settled.InputTokens, settled.OutputTokens)
	}
	assertUsageShare(t, "cached input", settled.CachedInputTokens, 40)
	assertUsageShare(t, "audio input", settled.AudioInputTokens, 30)
	assertUsageShare(t, "image input", settled.ImageInputTokens, 10)
	assertUsageShare(t, "cached audio input", settled.CachedAudioInputTokens, 12)
	assertUsageShare(t, "cached image input", settled.CachedImageInputTokens, 4)
	assertUsageShare(t, "audio output", settled.AudioOutputTokens, 20)
}

func TestHoldSessionExposureSweepsExpired(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, time.January, 1, 0, 0, 0, 0, time.UTC)
	s := &Server{Now: func() time.Time { return now }, exposures: newSessionExposures()}
	s.HoldSessionExposure("live", now.Add(time.Hour))
	s.HoldSessionExposure("expired", now.Add(-time.Hour))
	s.HoldSessionExposure("other", now.Add(time.Hour))
	if _, ok := s.exposures.held["expired"]; ok {
		t.Error("expired exposure remained after holding another key")
	}
	if !s.localExposureHeld("live") || !s.localExposureHeld("other") {
		t.Errorf("live exposures = %v, want both keys held", s.exposures.held)
	}
}

func TestNativeDuplexUsageAccounting(t *testing.T) {
	t.Parallel()
	vertex := connectors.Config{
		Kind: "vertex_ai", VendorID: "google-vertex", ProfileID: "vertex-realtime", ProfileRevision: connectors.ProfileRevision,
		Endpoint:    "https://us-central1-aiplatform.googleapis.com/v1/projects/audit-project/locations/us-central1/publishers/google",
		CloudRegion: "us-central1", CloudProject: "audit-project", AuthMode: "service_account",
	}
	bedrock := connectors.Config{
		Kind: "bedrock", VendorID: "amazon-bedrock", ProfileID: "bedrock-realtime", ProfileRevision: connectors.ProfileRevision,
		Endpoint:    connectors.DefaultProfileEndpoint("bedrock", "bedrock-realtime", "us-east-1", ""),
		CloudRegion: "us-east-1", AuthMode: connectors.AuthStatic,
	}
	xai := connectors.Config{
		Kind: "openai_compatible", VendorID: "xai", ProfileID: "xai-realtime", ProfileRevision: connectors.ProfileRevision,
		Endpoint: "https://api.x.ai/v1", AuthMode: "api_key",
	}
	const manualTurns = `{"type":"session.update","session":{"turn_detection":null}}`
	for _, tc := range []struct {
		name           string
		provider       connectors.Config
		client, server []string
		want           string
		complete       bool
	}{
		{"Vertex sums turns and reads trailing usage", vertex, nil, []string{
			`{"serverContent":{"turnComplete":true},"usageMetadata":{"promptTokenCount":10,"responseTokenCount":5,"toolUsePromptTokenCount":3,"thoughtsTokenCount":2,"cachedContentTokenCount":4,"totalTokenCount":12,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":6}],"responseTokensDetails":[{"modality":"AUDIO","tokenCount":1}]}}`,
			`{"serverContent":{"turnComplete":true}}`,
			`{"usageMetadata":{"promptTokenCount":7,"responseTokenCount":3,"totalTokenCount":99,"promptTokensDetails":[{"modality":"AUDIO","tokenCount":2}]}}`,
		}, `{"input_tokens":20,"output_tokens":10,"total_tokens":119,"cached_input_tokens":4,"audio_input_tokens":8,"audio_output_tokens":1,"reasoning_tokens":2}`, true},
		{"Vertex turn without usage", vertex, nil, []string{`{"serverContent":{"turnComplete":true}}`}, `null`, false},
		{"Nova Sonic keeps the largest running total", bedrock, nil, []string{
			`{"event":{"completionStart":{}}}`,
			`{"event":{"usageEvent":{"totalInputTokens":10,"totalOutputTokens":5,"totalTokens":15,"details":{"total":{"input":{"speechTokens":6},"output":{"speechTokens":4}}}}}}`,
			`{"event":{"usageEvent":{"totalInputTokens":25,"totalOutputTokens":12,"totalTokens":37,"details":{"total":{"input":{"speechTokens":14},"output":{"speechTokens":9}}}}}}`,
			`{"event":{"usageEvent":{"totalInputTokens":1,"totalOutputTokens":1,"totalTokens":2}}}`,
			`{"event":{"completionEnd":{}}}`,
		}, `{"input_tokens":25,"output_tokens":12,"total_tokens":37,"audio_input_tokens":14,"audio_output_tokens":9}`, true},
		{"Nova Sonic usage without a total", bedrock, nil, []string{`{"event":{"usageEvent":{"totalInputTokens":10,"totalOutputTokens":5}}}`}, `null`, false},
		{"Nova Sonic completion without usage", bedrock, nil, []string{
			`{"event":{"usageEvent":{"totalInputTokens":10,"totalOutputTokens":5,"totalTokens":15}}}`,
			`{"event":{"completionStart":{}}}`,
			`{"event":{"completionEnd":{}}}`,
		}, `{"input_tokens":10,"output_tokens":5,"total_tokens":15}`, false},
		{"xAI sums responses", xai, []string{manualTurns}, []string{
			`{"type":"response.created","response":{"id":"r1"}}`,
			`{"type":"response.done","response":{"id":"r1","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":21,"billable_audio_seconds":1.5}}}`,
			`{"type":"response.created","response":{"id":"r2"}}`,
			`{"type":"response.done","response":{"id":"r2","usage":{"input_tokens":4,"output_tokens":2}}}`,
		}, `{"input_tokens":14,"output_tokens":7,"total_tokens":27,"media_units":"1.5"}`, true},
		{"xAI bills seconds without tokens", xai, []string{manualTurns}, []string{
			`{"type":"response.done","response":{"id":"r1","usage":{"billable_audio_seconds":2.5}}}`,
		}, `{"input_tokens":0,"output_tokens":0,"total_tokens":0,"media_units":"2.5"}`, true},
		{"xAI duration billed session stays incomplete", xai, nil, []string{
			`{"type":"response.done","response":{"id":"r1","usage":{"input_tokens":10,"output_tokens":5}}}`,
		}, `{"input_tokens":10,"output_tokens":5,"total_tokens":15}`, false},
		{"xAI response without usage", xai, []string{manualTurns}, []string{`{"type":"response.done","response":{"id":"r1"}}`}, `null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			template, err := duplex.Compile(duplex.Config{Provider: tc.provider, Model: "audit-model"})
			if err != nil {
				t.Fatal(err)
			}
			o := template.Observer(1 << 20)
			for _, frame := range tc.client {
				if err := o.ClientFrame([]byte(frame)); err != nil {
					t.Fatal(err)
				}
			}
			for _, frame := range tc.server {
				if _, err := o.ServerFrame([]byte(frame)); err != nil {
					t.Fatal(err)
				}
			}
			got, err := json.Marshal(o.Usage())
			if err != nil || string(got) != tc.want || o.Complete() != tc.complete {
				t.Fatalf("usage = %s, complete = %t, want %s, %t: %v", got, o.Complete(), tc.want, tc.complete, err)
			}
		})
	}
}
