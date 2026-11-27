package media

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

func withFixedClock(t *testing.T, unix int64) {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time { return time.Unix(unix, 0).UTC() }
	t.Cleanup(func() { nowFunc = prev })
}

func mustJSONFields(t *testing.T, body Body) map[string]json.RawMessage {
	t.Helper()
	var raw []byte
	switch b := body.(type) {
	case JSONBody:
		raw = b
	case NativeJSON:
		raw = b
	default:
		t.Fatalf("expected JSON body, got %T", body)
	}
	fields, err := parseRawObject(raw)
	if err != nil {
		t.Fatalf("invalid JSON body %s: %v", string(raw), err)
	}
	return fields
}

func TestVertexAndBedrockImageCodecs(t *testing.T) {
	withFixedClock(t, 1700000100)

	req, failure := DecodeImageGeneration([]byte(`{"model":"img","prompt":"mountains","n":2,"size":"1536x1024","response_format":"b64_json"}`))
	if failure != nil {
		t.Fatalf("decode failed: %v", failure)
	}

	vertexCall, _, failure := EncodeConfigured(req, connectors.Config{Kind: connectors.KindVertexAI}, "imagen-3.0-generate-002")
	if failure != nil {
		t.Fatalf("vertex encode failed: %v", failure)
	}
	vFields := mustJSONFields(t, vertexCall.Body)
	if !strings.Contains(string(vFields["parameters"]), `"aspectRatio":"3:2"`) {
		t.Fatalf("unexpected vertex parameters: %s", string(vFields["parameters"]))
	}
	vReply := vertexCall.Response.(ImageReply)
	vRes, failure := vReply.Images([]byte(`{"predictions":[{"bytesBase64Encoded":"AA==","mimeType":"image/png"},{"bytesBase64Encoded":"BB==","mimeType":"image/png"}]}`), stubStageImage)
	if failure != nil || vRes.CreatedAt != 1700000100 || len(vRes.Images) != 2 {
		t.Fatalf("vertex decode = (%+v, %v)", vRes, failure)
	}
	if _, err := vReply.Images([]byte(`{"predictions":[{"bytesBase64Encoded":"AA==","mimeType":"text/plain"}]}`), stubStageImage); err == nil {
		t.Fatalf("expected non-image MIME type error")
	}

	titanReq, _ := DecodeImageGeneration([]byte(`{"model":"img","prompt":"forest","n":1,"size":"512x512"}`))
	titanCall, _, failure := EncodeConfigured(titanReq, connectors.Config{Kind: connectors.KindBedrock}, "amazon.titan-image-generator-v2:0")
	if failure != nil {
		t.Fatalf("titan encode failed: %v", failure)
	}
	if !strings.Contains(string(titanCall.Body.(NativeJSON)), `"width":512`) {
		t.Fatalf("unexpected titan body: %s", string(titanCall.Body.(NativeJSON)))
	}

	stabReq, _ := DecodeImageGeneration([]byte(`{"model":"img","prompt":"forest","size":"1024x1024","output_format":"webp"}`))
	stabCall, _, failure := EncodeConfigured(stabReq, connectors.Config{Kind: connectors.KindBedrock}, "stability.sd3-5-large-v1:0")
	if failure != nil {
		t.Fatalf("bedrock stability encode failed: %v", failure)
	}
	sFields := mustJSONFields(t, stabCall.Body)
	if string(sFields["output_format"]) != `"webp"` || string(sFields["aspect_ratio"]) != `"1:1"` {
		t.Fatalf("unexpected bedrock stability fields: %v", sFields)
	}
	bReply := stabCall.Response.(ImageReply)
	bRes, failure := bReply.Images([]byte(`{"images":["CC=="],"finish_reasons":[null]}`), stubStageImage)
	if failure != nil || bRes.CreatedAt != 1700000100 || len(bRes.Images) != 1 {
		t.Fatalf("bedrock decode = (%+v, %v)", bRes, failure)
	}
	if _, err := bReply.Images([]byte(`{"images":["CC=="],"finish_reasons":["Filter reason: policy"]}`), stubStageImage); err == nil || err.Code != "content_filter" {
		t.Fatalf("expected content_filter error, got %v", err)
	}
}

func TestGeminiMediaCodec(t *testing.T) {
	withFixedClock(t, 1700000200)
	cfg := connectors.Config{Kind: connectors.KindGemini, VendorID: "google"}

	imgReq, _ := DecodeImageGeneration([]byte(`{"model":"gem","prompt":"sunset","size":"1792x1024"}`))
	imgCall, _, failure := EncodeConfigured(imgReq, cfg, "gemini-2.5-flash-image")
	if failure != nil {
		t.Fatalf("gemini image encode failed: %v", failure)
	}
	if !strings.Contains(string(imgCall.Body.(JSONBody)), `"aspectRatio":"16:9"`) {
		t.Fatalf("unexpected gemini image body: %s", string(imgCall.Body.(JSONBody)))
	}
	imgRes, failure := imgCall.Response.(ImageReply).Images([]byte(`{
		"candidates":[{"content":{"parts":[
			{"thought":true,"inlineData":{"mimeType":"image/png","data":"SKIP"}},
			{"inlineData":{"mimeType":"image/png","data":"REAL"}}
		]}}],
		"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":10,"totalTokenCount":15,"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":10}]}
	}`), stubStageImage)
	if failure != nil || imgRes.CreatedAt != 1700000200 || *imgRes.Images[0].Handle != "img-REAL" || *imgRes.Usage.ImageOutputTokens != 10 {
		t.Fatalf("unexpected gemini image result: %+v (%v)", imgRes, failure)
	}

	speechReq, _ := DecodeSpeech([]byte(`{"model":"gem","input":"hello","voice":"Kore","response_format":"wav"}`))
	speechCall, _, failure := EncodeConfigured(speechReq, cfg, "gemini-2.5-flash-preview-tts")
	if failure != nil {
		t.Fatalf("gemini speech encode failed: %v", failure)
	}
	pcm := make([]byte, 32)
	audioRes, failure := speechCall.Response.(InlineAudioReply).Decode([]byte(`{
		"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/L16;rate=24000","data":"` + base64.StdEncoding.EncodeToString(pcm) + `"}}]}}]
	}`))
	if failure != nil || audioRes.ContentType != "audio/wav" || len(audioRes.Audio) != 44+len(pcm) {
		t.Fatalf("unexpected gemini wav result: %+v (%v)", audioRes, failure)
	}

	pcmSpeechReq, _ := DecodeSpeech([]byte(`{"model":"gem","input":"hello","voice":"Kore","response_format":"pcm"}`))
	pcmCall, _, _ := EncodeConfigured(pcmSpeechReq, cfg, "gemini-2.5-flash-preview-tts")
	wavBytes := SilentWAV(10)
	pcmRes, failure := pcmCall.Response.(InlineAudioReply).Decode([]byte(`{
		"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"audio/wav","data":"` + base64.StdEncoding.EncodeToString(wavBytes) + `"}}]}}]
	}`))
	if failure != nil || pcmRes.ContentType != "audio/pcm" || len(pcmRes.Audio) != len(wavBytes)-44 {
		t.Fatalf("unexpected gemini pcm result: %+v (%v)", pcmRes, failure)
	}

	sttReq, _ := DecodeTranscription(staging.FormOf([]staging.Field{
		{Name: "model", Value: staging.Text("gem")},
		{Name: "file", Value: testPart("a1", "clip.wav", "audio/wav")},
		{Name: "temperature", Value: staging.Text("0.2")},
	}, false))
	sttCall, _, failure := EncodeConfigured(sttReq, cfg, "gemini-2.5-flash")
	if failure != nil {
		t.Fatalf("gemini transcription encode failed: %v", failure)
	}
	inlineBody, failure := sttCall.Body.(InlineBody)(func(staging.Part) ([]byte, *Error) { return []byte("wav"), nil })
	if failure != nil || !strings.Contains(string(inlineBody), `"temperature":0.2`) {
		t.Fatalf("unexpected gemini inline body: %s (%v)", string(inlineBody), failure)
	}
	trRes, failure := sttCall.Response.(TranscriptReply).Transcript([]byte(`{
		"candidates":[{"content":{"parts":[{"text":"thinking","thought":true},{"text":" hello "}]}}]
	}`))
	if failure != nil || trRes.Text != "hello" || !trRes.TextOnly {
		t.Fatalf("unexpected gemini transcript: %+v (%v)", trRes, failure)
	}
}

func TestStabilityRecraftBFLAndXAICodecs(t *testing.T) {
	withFixedClock(t, 1700000300)

	imgReq, _ := DecodeImageGeneration([]byte(`{"model":"r","prompt":"neon","size":"1024x1024","output_format":"png"}`))
	stabCall, _, failure := EncodeConfigured(imgReq, connectors.Config{VendorID: "stability"}, "sd3.5-large")
	if failure != nil || stabCall.Endpoint.(Path).Path != "v2beta/stable-image/generate/sd3" {
		t.Fatalf("unexpected stability call: %+v (%v)", stabCall, failure)
	}
	stabRes, failure := stabCall.Response.(ImageReply).Images([]byte(`{"image":"QQ==","finish_reason":"SUCCESS"}`), stubStageImage)
	if failure != nil || stabRes.CreatedAt != 1700000300 {
		t.Fatalf("stability decode = (%+v, %v)", stabRes, failure)
	}
	if _, err := stabCall.Response.(ImageReply).Images([]byte(`{"finish_reason":"CONTENT_FILTERED"}`), stubStageImage); err == nil || err.Code != "content_filter" {
		t.Fatalf("expected stability content_filter, got %v", err)
	}

	editReq, _ := DecodeImageEdit(staging.FormOf([]staging.Field{
		{Name: "model", Value: staging.Text("r")},
		{Name: "prompt", Value: staging.Text("fix sky")},
		{Name: "image", Value: testPart("i1", "a.png", "image/png")},
	}, false))
	stabEdit, _, failure := EncodeConfigured(editReq, connectors.Config{VendorID: "stability"}, "stable-image-inpaint")
	if failure != nil || stabEdit.Endpoint.(Path).Path != "v2beta/stable-image/edit/inpaint" {
		t.Fatalf("unexpected stability edit call: %+v (%v)", stabEdit, failure)
	}

	recraftCall, _, failure := EncodeConfigured(imgReq, connectors.Config{VendorID: "recraft"}, "recraftv3")
	if failure != nil {
		t.Fatalf("recraft encode failed: %v", failure)
	}
	rFields := mustJSONFields(t, recraftCall.Body)
	if string(rFields["size"]) != `"1:1"` || string(rFields["image_format"]) != `"png"` {
		t.Fatalf("unexpected recraft fields: %v", rFields)
	}

	bflCall, _, failure := EncodeConfigured(imgReq, connectors.Config{VendorID: "bfl"}, "flux-pro-1.1")
	if failure != nil {
		t.Fatalf("bfl encode failed: %v", failure)
	}
	bFields := mustJSONFields(t, bflCall.Body)
	if string(bFields["width"]) != "1024" || string(bFields["height"]) != "1024" {
		t.Fatalf("unexpected bfl fields: %v", bFields)
	}
	next := bflCall.Response.(ImageReply).Poll.Next
	if step, err := next([]byte(`{"polling_url":"https://api.bfl.ai/get?id=1"}`)); err != nil {
		t.Fatalf("bfl visit failed: %v", err)
	} else if _, ok := step.(Visit); !ok {
		t.Fatalf("expected Visit, got %T", step)
	}
	if step, err := next([]byte(`{"status":"Generating"}`)); err != nil {
		t.Fatalf("bfl again failed: %v", err)
	} else if _, ok := step.(Again); !ok {
		t.Fatalf("expected Again, got %T", step)
	}
	if step, err := next([]byte(`{"status":"Ready","result":{"sample":"https://cdn.bfl.ai/x.png"}}`)); err != nil {
		t.Fatalf("bfl fetch failed: %v", err)
	} else if f, ok := step.(Fetch); !ok || f.URL != "https://cdn.bfl.ai/x.png" {
		t.Fatalf("expected Fetch, got %#v", step)
	}
	if _, err := next([]byte(`{"status":"Content Moderated"}`)); err == nil || err.Code != "content_filter" {
		t.Fatalf("expected BFL content_filter, got %v", err)
	}

	badBFLReq, _ := DecodeImageGeneration([]byte(`{"model":"r","prompt":"neon","size":"1024x1024extra"}`))
	if _, _, err := EncodeConfigured(badBFLReq, connectors.Config{VendorID: "bfl"}, "flux-pro-1.1"); err == nil {
		t.Fatalf("expected trailing garbage size to be rejected by BFL")
	}

	xaiReq, _ := DecodeImageGeneration([]byte(`{"model":"r","prompt":"neon","size":"1792x1024"}`))
	xaiCall, _, failure := EncodeConfigured(xaiReq, connectors.Config{VendorID: "xai"}, "grok-2-image")
	if failure != nil {
		t.Fatalf("xai encode failed: %v", failure)
	}
	xRes, failure := xaiCall.Response.(ImageReply).Images([]byte(`{"data":[{"url":"https://img.x.ai/1.png"}]}`), stubStageImage)
	if failure != nil || xRes.CreatedAt != 1700000300 {
		t.Fatalf("expected xAI default created timestamp 1700000300, got %+v (%v)", xRes, failure)
	}
}

func TestSpeechAndTranscriptVendorCodecs(t *testing.T) {
	speechReq, _ := DecodeSpeech([]byte(`{"model":"r","input":"hi","voice":"thalia","response_format":"wav"}`))

	elReq, _ := DecodeSpeech([]byte(`{"model":"r","input":"hi","voice":"voice123","response_format":"mp3","speed":1.1}`))
	elCall, _, failure := EncodeConfigured(elReq, connectors.Config{VendorID: "elevenlabs"}, "eleven_multilingual_v2")
	if failure != nil || !strings.Contains(string(elCall.Body.(JSONBody)), `"speed":1.1`) {
		t.Fatalf("elevenlabs speech = (%+v, %v)", elCall, failure)
	}

	dgCall, _, failure := EncodeConfigured(speechReq, connectors.Config{VendorID: "deepgram"}, "aura-2-thalia-en")
	if failure != nil {
		t.Fatalf("deepgram speech failed: %v", failure)
	}
	dgQuery := dgCall.Endpoint.(Path).Query
	dgQuery["encoding"][0] = "mutated"
	if deepgramEncodings["wav"]["encoding"][0] != "linear16" {
		t.Fatalf("deepgramEncodings was mutated via query slice aliasing")
	}

	pollyReq, _ := DecodeSpeech([]byte(`{"model":"r","input":"hi","voice":"Joanna","response_format":"opus"}`))
	pollyCall, _, failure := EncodeConfigured(pollyReq, connectors.Config{Kind: connectors.KindBedrock, VendorID: "amazon-bedrock", CloudRegion: "us-east-1"}, "neural")
	if failure != nil {
		t.Fatalf("polly encode failed: %v", failure)
	}
	abs, ok := pollyCall.Endpoint.(Absolute)
	if !ok || abs.SigningService != "polly" || !strings.HasSuffix(abs.URL, "/v1/speech") {
		t.Fatalf("unexpected polly endpoint: %+v", pollyCall.Endpoint)
	}

	sttForm := func(format string) *Request {
		req, err := DecodeTranscription(staging.FormOf([]staging.Field{
			{Name: "model", Value: staging.Text("r")},
			{Name: "file", Value: testPart("f1", "a.wav", "audio/wav")},
			{Name: "response_format", Value: staging.Text(format)},
			{Name: "language", Value: staging.Text("en")},
		}, false))
		if err != nil {
			t.Fatalf("DecodeTranscription failed: %v", err)
		}
		return req
	}

	elStt, _, failure := EncodeConfigured(sttForm("verbose_json"), connectors.Config{VendorID: "elevenlabs"}, "scribe_v1")
	if failure != nil {
		t.Fatalf("elevenlabs stt failed: %v", failure)
	}
	elTr, failure := elStt.Response.(TranscriptReply).Transcript([]byte(`{"text":"hi there","language_code":"en","audio_duration_secs":1.2,"words":[{"text":"hi","type":"word","start":0,"end":0.4},{"text":" ","type":"spacing","start":0.4,"end":0.5}]}`))
	if failure != nil || !strings.Contains(string(elTr.Extra["words"]), `"word":"hi"`) || strings.Contains(string(elTr.Extra["words"]), `" "`) {
		t.Fatalf("unexpected elevenlabs transcript: %+v (%v)", elTr, failure)
	}

	dgStt, _, failure := EncodeConfigured(sttForm("verbose_json"), connectors.Config{VendorID: "deepgram"}, "nova-3")
	if failure != nil {
		t.Fatalf("deepgram stt failed: %v", failure)
	}
	dgTr, failure := dgStt.Response.(TranscriptReply).Transcript([]byte(`{"metadata":{"duration":2.0},"results":{"channels":[{"alternatives":[{"transcript":"hello","words":[{"word":"hello","start":0.1,"end":0.5}]}]}]}}`))
	if failure != nil || dgTr.Language == nil || *dgTr.Language != "en" || !strings.Contains(string(dgTr.Extra["words"]), `"hello"`) {
		t.Fatalf("unexpected deepgram transcript: %+v (%v)", dgTr, failure)
	}

	aaiCall, _, failure := EncodeConfigured(sttForm("verbose_json"), connectors.Config{VendorID: "assemblyai", Endpoint: "https://api.assemblyai.com/"}, "universal")
	if failure != nil {
		t.Fatalf("assemblyai encode failed: %v", failure)
	}
	aaiReply := aaiCall.Response.(TranscriptReply)
	step1, failure := aaiReply.Poll.Next([]byte(`{"upload_url":"https://cdn.assemblyai.com/u1"}`))
	if failure != nil || step1.(Visit).Method != http.MethodPost {
		t.Fatalf("assemblyai upload step = (%#v, %v)", step1, failure)
	}
	step2, failure := aaiReply.Poll.Next([]byte(`{"id":"tr-1","status":"queued"}`))
	if failure != nil || step2.(Visit).Wait != assemblyAIPoll {
		t.Fatalf("assemblyai poll step = (%#v, %v)", step2, failure)
	}
	step3, failure := aaiReply.Poll.Next([]byte(`{"id":"tr-1","status":"completed"}`))
	if failure != nil || step3.(Finished).Cleanup == nil {
		t.Fatalf("assemblyai finished step = (%#v, %v)", step3, failure)
	}
	if _, err := aaiReply.Poll.Next([]byte(`{"id":"tr-1","status":"error"}`)); err == nil || err.Code != "transcription_failed" {
		t.Fatalf("expected assemblyai transcription_failed, got %v", err)
	}
	aaiTr, failure := aaiReply.Transcript([]byte(`{"text":"hey","language_code":"en","audio_duration":1.5,"words":[{"text":"hey","start":500,"end":1500}]}`))
	if failure != nil || !strings.Contains(string(aaiTr.Extra["words"]), `"start":0.5`) {
		t.Fatalf("unexpected assemblyai transcript: %+v (%v)", aaiTr, failure)
	}

	groqCall, _, failure := EncodeConfigured(sttForm("json"), connectors.Config{VendorID: "groq"}, "whisper-large-v3")
	if failure != nil {
		t.Fatalf("groq encode failed: %v", failure)
	}
	groqTr, failure := groqCall.Response.(TranscriptReply).Transcript([]byte(`{"text":"fast","duration":1.0}`))
	if failure != nil || !groqTr.TextOnly || groqTr.Text != "fast" {
		t.Fatalf("unexpected groq transcript: %+v (%v)", groqTr, failure)
	}
}

func TestRunwayVideoCodec(t *testing.T) {
	withFixedClock(t, 1700000400)
	cfg := connectors.Config{VendorID: "runway"}

	req, _ := DecodeVideoCreate(staging.FormOf([]staging.Field{
		{Name: "model", Value: staging.Text("vid")},
		{Name: "prompt", Value: staging.Text("drone shot")},
		{Name: "seconds", Value: staging.Text("4")},
		{Name: "size", Value: staging.Text("1280x720")},
		{Name: "input_reference", Value: testPart("r1", "start.png", "image/png")},
	}, false))
	createCall, _, failure := EncodeConfigured(req, cfg, "gen4_turbo")
	if failure != nil || createCall.Endpoint.(Path).Path != "image_to_video" {
		t.Fatalf("unexpected runway create call: %+v (%v)", createCall, failure)
	}
	body, failure := createCall.Body.(InlineBody)(func(staging.Part) ([]byte, *Error) { return []byte("png"), nil })
	if failure != nil || !strings.Contains(string(body), `"ratio":"1280:720"`) || !strings.Contains(string(body), `"promptImage":"data:image/png;base64,`) {
		t.Fatalf("unexpected runway body: %s (%v)", string(body), failure)
	}
	created, failure := createCall.Response.(VideoJobReply).Video([]byte(`{"id":"task-1"}`))
	if failure != nil || created.ID != "task-1" || created.Status != "queued" || *created.CreatedAt != 1700000400 {
		t.Fatalf("unexpected runway created result: %+v (%v)", created, failure)
	}

	getCall, failure := EncodeJob(VideoGet{ID: "task-1"}, cfg)
	if failure != nil {
		t.Fatalf("runway get failed: %v", failure)
	}
	got, failure := getCall.Response.(VideoJobReply).Video([]byte(`{"id":"task-1","status":"RUNNING","progress":0.4,"createdAt":"2026-01-01T00:00:00Z"}`))
	if failure != nil || got.Status != "in_progress" || *got.Progress != 40 {
		t.Fatalf("unexpected runway get result: %+v (%v)", got, failure)
	}
	cancelled, failure := getCall.Response.(VideoJobReply).Video([]byte(`{"id":"task-1","status":"CANCELLED"}`))
	if failure != nil || cancelled.Status != "failed" || cancelled.ErrorCode == nil || *cancelled.ErrorCode != "cancelled" {
		t.Fatalf("unexpected runway cancelled result: %+v (%v)", cancelled, failure)
	}
	if _, err := getCall.Response.(VideoJobReply).Video([]byte(`{"id":"other","status":"RUNNING"}`)); err == nil {
		t.Fatalf("expected identity mismatch error on runway get")
	}

	contentCall, failure := EncodeJob(VideoContent{ID: "task-1", Variant: "video"}, cfg)
	if failure != nil {
		t.Fatalf("runway content failed: %v", failure)
	}
	step, failure := contentCall.Response.(VideoContentReply).Poll.Next([]byte(`{"status":"SUCCEEDED","output":["https://cdn.runway.ml/out.mp4"]}`))
	if failure != nil || step.(Fetch).URL != "https://cdn.runway.ml/out.mp4" {
		t.Fatalf("unexpected runway content step: %#v (%v)", step, failure)
	}
	if _, err := contentCall.Response.(VideoContentReply).Poll.Next([]byte(`{"status":"RUNNING"}`)); err == nil || err.Status != http.StatusConflict {
		t.Fatalf("expected 409 video_not_ready, got %v", err)
	}
	if _, err := EncodeJob(VideoContent{ID: "task-1", Variant: "thumbnail"}, cfg); err == nil {
		t.Fatalf("expected thumbnail variant rejection")
	}

	delCall, failure := EncodeJob(VideoDelete{ID: "task-1"}, cfg)
	if failure != nil || !delCall.Response.(VideoDeletedReply).NoContent {
		t.Fatalf("unexpected runway delete call: %+v (%v)", delCall, failure)
	}
}
