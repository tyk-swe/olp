package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"image"
	"image/png"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/staging"
)

var update = flag.Bool("update", false, "rewrite testdata/media.golden.json from the current behavior")

const mediaGolden = "testdata/media.golden.json"

type mediaReply struct {
	status      int
	contentType string
	body        string
}

type mediaFile struct {
	field, name, contentType string
	content                  []byte
}

type mediaCase struct {
	name      string
	model     string
	provider  func(*routing.Provider)
	route     func(*routing.Route)
	operation media.Operation
	mode      string
	path      string
	json      string
	fields    [][2]string
	files     []mediaFile
	replies   []mediaReply
}

type recordedPart struct {
	Name        string `json:"name"`
	Filename    string `json:"filename,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Text        string `json:"text,omitempty"`
	Digest      string `json:"digest,omitempty"`
}

type recordedRequest struct {
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query,omitempty"`
	Headers map[string]string   `json:"headers"`
	JSON    json.RawMessage     `json:"json,omitempty"`
	Parts   []recordedPart      `json:"parts,omitempty"`
	Digest  string              `json:"digest,omitempty"`
	Form    map[string][]string `json:"form,omitempty"`
}

type recordedAttempt struct {
	Mode      string `json:"mode"`
	Status    int    `json:"status"`
	Class     string `json:"class"`
	Committed bool   `json:"committed"`
	Usage     any    `json:"usage,omitempty"`
}

type mediaRecord struct {
	Upstream    []recordedRequest `json:"upstream"`
	Status      int               `json:"status"`
	ContentType string            `json:"content_type"`
	Body        any               `json:"body"`
	Operation   string            `json:"operation"`
	Usage       any               `json:"usage,omitempty"`
	Attempts    []recordedAttempt `json:"attempts"`
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]) + "/" + strconv.Itoa(len(b))
}

func onePixelPNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func canonicalJSON(raw []byte, upstream string) (json.RawMessage, bool) {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil, false
	}
	out, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(strings.ReplaceAll(string(out), upstream, "{upstream}")), true
}

func recordRequest(t *testing.T, r *http.Request, upstream string) recordedRequest {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		t.Errorf("read upstream body: %v", err)
	}
	headers := map[string]string{}
	for name, values := range r.Header {
		switch name {
		case "Accept-Encoding", "User-Agent", "Content-Length", "Traceparent", "Tracestate":
			continue
		}
		headers[name] = strings.Join(values, ",")
	}
	got := recordedRequest{Method: r.Method, Path: strings.TrimPrefix(r.URL.Path, "/a"), Query: r.URL.RawQuery, Headers: headers}
	mediaType, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch {
	case mediaType == "multipart/form-data":
		headers["Content-Type"] = mediaType
		reader := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("read upstream multipart: %v", err)
				break
			}
			content, _ := io.ReadAll(part)
			recorded := recordedPart{Name: part.FormName(), Filename: part.FileName(), ContentType: part.Header.Get("Content-Type")}
			if part.FileName() == "" {
				recorded.Text = string(content)
			} else {
				recorded.Digest = digest(content)
			}
			got.Parts = append(got.Parts, recorded)
		}
	case len(raw) == 0:
	default:
		if canonical, ok := canonicalJSON(raw, upstream); ok {
			got.JSON = canonical
		} else {
			got.Digest = digest(raw)
		}
	}
	return got
}

func scripted(t *testing.T, replies []mediaReply, upstream func() string) (http.HandlerFunc, func() []recordedRequest) {
	var mu sync.Mutex
	var seen []recordedRequest
	return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			seen = append(seen, recordRequest(t, r, upstream()))
			n := len(seen)
			mu.Unlock()
			if n > len(replies) {
				http.Error(w, "unscripted upstream request", http.StatusTeapot)
				return
			}
			reply := replies[n-1]
			w.Header().Set("Content-Type", reply.contentType)
			w.WriteHeader(reply.status)
			io.WriteString(w, strings.ReplaceAll(reply.body, "{upstream}", upstream()))
		}, func() []recordedRequest {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(seen)
		}
}

func runMediaCase(t *testing.T, tc mediaCase) mediaRecord {
	t.Helper()
	h := newHarness(t, Config{MaxMediaBodyBytes: 1 << 20})
	spool, err := staging.NewSpool(t.TempDir(), staging.MinCapacityBytes, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = spool.Close() })
	h.gateway.deps.Media = &MediaDeps{Transport: transport.New(transport.Deps{
		Client: h.gateway.client, Auth: h.gateway.auth, Egress: h.gateway.egress,
		Spool: spool, MaxResponseBytes: 1 << 20, Log: h.gateway.log,
	})}
	h.gateway.deps.Staging = spool
	h.gateway.deps.Uploads = staging.NewAdmission(spool.CapacityBytes())
	mode := tc.mode
	if mode == "" {
		mode = "unary"
	}
	h.republish(func(snapshot *routing.Snapshot, a, _ routing.Provider) {
		model := a.Capabilities[0].Model
		if tc.model != "" {
			model = tc.model
		}
		a.Capabilities = []routing.Capability{{Model: model, Operation: string(tc.operation), Surface: "openai", Mode: mode}}
		if tc.provider != nil {
			tc.provider(&a)
		}
		snapshot.Providers[a.ID] = a
		route := snapshot.Routes[routeSlug]
		route.Operations = []string{string(tc.operation)}
		route.Targets = route.Targets[:1]
		route.Targets[0].ProviderModel = model
		if tc.route != nil {
			tc.route(&route)
		}
		snapshot.Routes[routeSlug] = route
	})
	handler, seen := scripted(t, tc.replies, func() string { return h.upstream.URL })
	h.mock.set("a", handler)
	mux := http.NewServeMux()
	h.gateway.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	var body bytes.Buffer
	contentType := "application/json"
	if tc.json != "" {
		body.WriteString(strings.ReplaceAll(tc.json, "{route}", routeSlug))
	} else {
		form := multipart.NewWriter(&body)
		if err := form.WriteField("model", routeSlug); err != nil {
			t.Fatal(err)
		}
		for _, field := range tc.fields {
			if err := form.WriteField(field[0], field[1]); err != nil {
				t.Fatal(err)
			}
		}
		for _, file := range tc.files {
			header := textproto.MIMEHeader{}
			header.Set("Content-Disposition", `form-data; name="`+file.field+`"; filename="`+file.name+`"`)
			header.Set("Content-Type", file.contentType)
			part, err := form.CreatePart(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(file.content); err != nil {
				t.Fatal(err)
			}
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		contentType = form.FormDataContentType()
	}
	started := time.Now()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+tc.path, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+fullKey)
	req.Header.Set("Content-Type", contentType)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	record := mediaRecord{Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type")}
	switch mediaType, _, _ := mime.ParseMediaType(record.ContentType); {
	case mediaType == "application/json":
		var body map[string]any
		if json.Unmarshal(raw, &body) != nil {
			t.Fatalf("client body is not a JSON object: %s", raw)
		}
		if created, ok := body["created"].(float64); ok && int64(created) >= started.Unix() {
			body["created"] = "{now}"
		}
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		canonical, _ := canonicalJSON(encoded, h.upstream.URL)
		record.Body = canonical
	case strings.HasPrefix(mediaType, "text/"):
		record.Body = strings.ReplaceAll(string(raw), h.upstream.URL, "{upstream}")
	default:
		record.Body = digest(raw)
	}
	envelope := h.sink.last(t)
	record.Upstream, record.Operation = seen(), envelope.Operation
	if envelope.Usage != nil {
		record.Usage = envelope.Usage
	}
	for _, attempt := range envelope.Attempts {
		recorded := recordedAttempt{Mode: attempt.Mode, Status: attempt.Status, Class: attempt.Class, Committed: attempt.Committed}
		if attempt.Usage != nil {
			recorded.Usage = attempt.Usage
		}
		record.Attempts = append(record.Attempts, recorded)
	}
	return record
}

func vendor(kind, vendorID string) func(*routing.Provider) {
	return func(p *routing.Provider) { p.Kind, p.VendorID = kind, vendorID }
}

func reply(contentType, body string) []mediaReply {
	return []mediaReply{{status: http.StatusOK, contentType: contentType, body: body}}
}

func mediaCases(t *testing.T) []mediaCase {
	pixel := onePixelPNG(t)
	pixel64 := base64.StdEncoding.EncodeToString(pixel)
	audio := media.SilentWAV(10)
	image := mediaFile{"image", "pixel.png", "image/png", pixel}
	mask := mediaFile{"mask", "mask.png", "image/png", pixel}
	wav := mediaFile{"file", "silence.wav", "audio/wav", audio}
	openAIImage := `{"created":1,"data":[{"b64_json":"` + pixel64 + `"}],"usage":{"input_tokens":5,"output_tokens":7,"total_tokens":12,"input_tokens_details":{"text_tokens":5,"image_tokens":0}}}`
	transcript := `{"text":"hello there","usage":{"type":"duration","seconds":1.5}}`
	return []mediaCase{
		{name: "openai image generation", operation: media.OpImageGeneration, path: "/v1/images/generations",
			json:    `{"model":"{route}","prompt":"a red fox","n":1,"size":"1024x1024","quality":"high","response_format":"b64_json","user":"u-1","vendor_flag":true}`,
			replies: reply("application/json", openAIImage)},
		{name: "openai image generation stream", operation: media.OpImageGeneration, mode: "streaming", path: "/v1/images/generations",
			json:    `{"model":"{route}","prompt":"a red fox","stream":true,"partial_images":1}`,
			replies: reply("text/event-stream", "event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\""+pixel64+"\",\"usage\":{\"input_tokens\":5,\"output_tokens\":7,\"total_tokens\":12}}\n\n")},
		{name: "openai image edit", operation: media.OpImageEdit, path: "/v1/images/edits",
			fields: [][2]string{{"prompt", "add a hat"}, {"n", "1"}, {"input_fidelity", "high"}}, files: []mediaFile{image, mask},
			replies: reply("application/json", openAIImage)},
		{name: "openai image variation", operation: media.OpImageVariation, path: "/v1/images/variations",
			fields: [][2]string{{"n", "2"}, {"size", "256x256"}, {"response_format", "url"}}, files: []mediaFile{image},
			replies: reply("application/json", `{"created":1,"data":[{"url":"https://cdn.example.test/1.png"},{"url":"https://cdn.example.test/2.png"}]}`)},
		{name: "openai speech", operation: media.OpSpeech, path: "/v1/audio/speech",
			json:    `{"model":"{route}","input":"hello there","voice":"alloy","response_format":"mp3","speed":1.25,"instructions":"cheerful"}`,
			replies: reply("audio/mpeg", "ID3-fake-mp3")},
		{name: "openai speech stream", operation: media.OpSpeech, mode: "streaming", path: "/v1/audio/speech",
			json:    `{"model":"{route}","input":"hello","voice":"alloy","stream_format":"sse"}`,
			replies: reply("text/event-stream", "data: {\"type\":\"speech.audio.delta\",\"audio\":\"AAAA\"}\n\ndata: {\"type\":\"speech.audio.done\",\"usage\":{\"input_tokens\":3,\"output_tokens\":9,\"total_tokens\":12}}\n\n")},
		{name: "openai transcription json", operation: media.OpTranscription, path: "/v1/audio/transcriptions",
			fields: [][2]string{{"language", "en"}, {"prompt", "names: Ada"}, {"temperature", "0.2"}}, files: []mediaFile{wav},
			replies: reply("application/json", transcript)},
		{name: "openai transcription verbose", operation: media.OpTranscription, path: "/v1/audio/transcriptions",
			fields: [][2]string{{"response_format", "verbose_json"}, {"timestamp_granularities[]", "word"}}, files: []mediaFile{wav},
			replies: reply("application/json", `{"task":"transcribe","language":"english","duration":1.5,"text":"hello there","words":[{"word":"hello","start":0,"end":0.5},{"word":"there","start":0.6,"end":1.1}]}`)},
		{name: "openai transcription text", operation: media.OpTranscription, path: "/v1/audio/transcriptions",
			fields: [][2]string{{"response_format", "text"}}, files: []mediaFile{wav},
			replies: reply("text/plain", "hello there\n")},
		{name: "openai transcription stream", operation: media.OpTranscription, mode: "streaming", path: "/v1/audio/transcriptions",
			fields: [][2]string{{"stream", "true"}}, files: []mediaFile{wav},
			replies: reply("text/event-stream", "data: {\"type\":\"transcript.text.delta\",\"delta\":\"hello\"}\n\ndata: {\"type\":\"transcript.text.done\",\"text\":\"hello\",\"usage\":{\"type\":\"tokens\",\"input_tokens\":4,\"output_tokens\":2,\"total_tokens\":6}}\n\n")},
		{name: "openai translation", operation: media.OpTranslation, path: "/v1/audio/translations",
			fields: [][2]string{{"prompt", "names: Ada"}}, files: []mediaFile{wav},
			replies: reply("application/json", `{"text":"hello there"}`)},
		{name: "openai upstream rate limit", operation: media.OpImageGeneration, path: "/v1/images/generations",
			json:    `{"model":"{route}","prompt":"a red fox"}`,
			replies: []mediaReply{{status: http.StatusTooManyRequests, contentType: "application/json", body: `{"error":{"message":"slow down","type":"rate_limit_error","code":"rate_limit_exceeded"}}`}}},
		{name: "profiled defaults", operation: media.OpImageGeneration, path: "/v1/images/generations",
			provider: func(p *routing.Provider) {
				p.ProfileID, p.ProfileRevision = "compatible-chat", connectors.ProfileRevision
				p.OperationDefaults = map[string]connectors.DefaultSet{string(media.OpImageGeneration): {Dialect: "direct-compatible/image_generation", Values: map[string]json.RawMessage{"quality": json.RawMessage(`"low"`), "size": json.RawMessage(`"512x512"`)}}}
			},
			json:    `{"model":"{route}","prompt":"a red fox","size":"1024x1024"}`,
			replies: reply("application/json", openAIImage)},
		{name: "strict image generation", operation: media.OpImageGeneration, path: "/v1/images/generations",
			provider: func(p *routing.Provider) {
				p.ProfileID, p.ProfileRevision = "compatible-chat", connectors.ProfileRevision
			},
			route:   func(r *routing.Route) { r.Fidelity = routing.RouteFidelity{Mode: routing.FidelityStrict} },
			json:    `{"model":"{route}","prompt":"a red fox","size":"1024x1024","vendor_flag":true}`,
			replies: reply("application/json", openAIImage)},
		{name: "xai image generation", operation: media.OpImageGeneration, path: "/v1/images/generations", provider: vendor("openai_compatible", "xai"),
			json:    `{"model":"{route}","prompt":"a red fox","n":2,"size":"1792x1024","response_format":"b64_json"}`,
			replies: reply("application/json", `{"data":[{"b64_json":"`+pixel64+`"},{"b64_json":"`+pixel64+`"}]}`)},
		{name: "groq transcription", operation: media.OpTranscription, path: "/v1/audio/transcriptions", provider: vendor("openai_compatible", "groq"),
			fields: [][2]string{{"language", "en"}}, files: []mediaFile{wav},
			replies: reply("application/json", `{"text":"hello there","x_groq":{"id":"req_1"}}`)},
		{name: "groq translation", operation: media.OpTranslation, path: "/v1/audio/translations", provider: vendor("openai_compatible", "groq"),
			files: []mediaFile{wav}, replies: reply("application/json", `{"text":"hello there"}`)},
		{name: "elevenlabs speech", operation: media.OpSpeech, path: "/v1/audio/speech", provider: vendor("openai_compatible", "elevenlabs"),
			json:    `{"model":"{route}","input":"hello there","voice":"voice-1","response_format":"mp3","speed":1.1}`,
			replies: reply("audio/mpeg", "ID3-fake-mp3")},
		{name: "elevenlabs transcription", operation: media.OpTranscription, path: "/v1/audio/transcriptions", provider: vendor("openai_compatible", "elevenlabs"),
			fields: [][2]string{{"response_format", "verbose_json"}, {"language", "en"}}, files: []mediaFile{wav},
			replies: reply("application/json", `{"language_code":"en","text":"hello there","words":[{"text":"hello","start":0,"end":0.5,"type":"word"},{"text":" ","start":0.5,"end":0.6,"type":"spacing"},{"text":"there","start":0.6,"end":1.1,"type":"word"}]}`)},
		{name: "deepgram speech", model: "aura-2-thalia-en", operation: media.OpSpeech, path: "/v1/audio/speech", provider: vendor("openai_compatible", "deepgram"),
			json:    `{"model":"{route}","input":"hello there","voice":"thalia","response_format":"wav"}`,
			replies: []mediaReply{{status: http.StatusOK, contentType: "audio/wav", body: string(audio)}}},
		{name: "deepgram transcription", operation: media.OpTranscription, path: "/v1/audio/transcriptions", provider: vendor("openai_compatible", "deepgram"),
			fields: [][2]string{{"response_format", "verbose_json"}, {"language", "en"}}, files: []mediaFile{wav},
			replies: reply("application/json", `{"metadata":{"duration":1.5},"results":{"channels":[{"detected_language":"en","alternatives":[{"transcript":"hello there","words":[{"word":"hello","start":0,"end":0.5},{"word":"there","start":0.6,"end":1.1}]}]}]}}`)},
		{name: "assemblyai transcription", operation: media.OpTranscription, path: "/v1/audio/transcriptions", provider: vendor("openai_compatible", "assemblyai"),
			fields: [][2]string{{"language", "en"}}, files: []mediaFile{wav},
			replies: []mediaReply{
				{http.StatusOK, "application/json", `{"upload_url":"https://cdn.assemblyai.com/upload/abc"}`},
				{http.StatusOK, "application/json", `{"id":"tr_1","status":"queued"}`},
				{http.StatusOK, "application/json", `{"id":"tr_1","status":"completed","text":"hello there","language_code":"en","audio_duration":1.5,"words":[{"text":"hello","start":0,"end":500}]}`},
				{http.StatusOK, "application/json", `{"id":"tr_1","status":"completed"}`},
			}},
		{name: "stability image generation", model: "stable-image-core", operation: media.OpImageGeneration, path: "/v1/images/generations", provider: vendor("openai_compatible", "stability"),
			json:    `{"model":"{route}","prompt":"a red fox","size":"1024x1024","output_format":"png"}`,
			replies: reply("application/json", `{"image":"`+pixel64+`","finish_reason":"SUCCESS","seed":1}`)},
		{name: "stability image edit", model: "stable-image-inpaint", operation: media.OpImageEdit, path: "/v1/images/edits", provider: vendor("openai_compatible", "stability"),
			fields: [][2]string{{"prompt", "add a hat"}}, files: []mediaFile{image, mask},
			replies: reply("application/json", `{"image":"`+pixel64+`","finish_reason":"SUCCESS","seed":1}`)},
		{name: "recraft image generation", operation: media.OpImageGeneration, path: "/v1/images/generations", provider: vendor("openai_compatible", "recraft"),
			json:    `{"model":"{route}","prompt":"a red fox","n":1,"size":"1024x1024","response_format":"b64_json"}`,
			replies: reply("application/json", `{"created":1,"data":[{"b64_json":"`+pixel64+`"}]}`)},
		{name: "bfl image generation", model: "flux-pro-1.1", operation: media.OpImageGeneration, path: "/v1/images/generations", provider: vendor("openai_compatible", "bfl"),
			json: `{"model":"{route}","prompt":"a red fox","size":"1024x768","output_format":"png","response_format":"b64_json"}`,
			replies: []mediaReply{
				{http.StatusOK, "application/json", `{"id":"job-1","polling_url":"{upstream}/a/poll/job-1"}`},
				{http.StatusOK, "application/json", `{"status":"Ready","result":{"sample":"{upstream}/a/sample.png"}}`},
				{http.StatusOK, "image/png", string(pixel)},
			}},
		{name: "bfl refuses image count", operation: media.OpImageGeneration, path: "/v1/images/generations", provider: vendor("openai_compatible", "bfl"),
			json: `{"model":"{route}","prompt":"a red fox","n":2}`},
		{name: "gemini image generation", operation: media.OpImageGeneration, path: "/v1/images/generations", provider: vendor("gemini", "google"),
			json:    `{"model":"{route}","prompt":"a red fox","size":"1024x1024","response_format":"b64_json"}`,
			replies: reply("application/json", `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":"`+pixel64+`"}}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":9,"totalTokenCount":13,"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":9}]}}`)},
		{name: "gemini speech", operation: media.OpSpeech, path: "/v1/audio/speech", provider: vendor("gemini", "google"),
			json:    `{"model":"{route}","input":"hello there","voice":"Kore","response_format":"wav"}`,
			replies: reply("application/json", `{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/L16;codec=pcm;rate=24000","data":"AAAAAA=="}}]}}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":5,"totalTokenCount":8}}`)},
		{name: "gemini transcription", operation: media.OpTranscription, path: "/v1/audio/transcriptions", provider: vendor("gemini", "google"),
			files: []mediaFile{wav}, replies: reply("application/json", `{"candidates":[{"content":{"parts":[{"text":"hello there"}]}}],"usageMetadata":{"promptTokenCount":30,"candidatesTokenCount":3,"totalTokenCount":33}}`)},
	}
}

func TestMediaUpstreamGolden(t *testing.T) {
	want := map[string]json.RawMessage{}
	if !*update {
		raw, err := os.ReadFile(mediaGolden)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	got := map[string]json.RawMessage{}
	cases := mediaCases(t)
	t.Run("cases", func(t *testing.T) {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				encoded, err := json.MarshalIndent(runMediaCase(t, tc), "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				mu.Lock()
				got[tc.name] = encoded
				mu.Unlock()
				if !*update && !jsonEqual(want[tc.name], encoded) {
					t.Errorf("media behavior changed\nwant %s\ngot  %s", want[tc.name], encoded)
				}
			})
		}
	})
	if !*update {
		return
	}
	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaGolden, append(encoded, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d cases: %v", len(got), slices.Sorted(maps.Keys(got)))
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
