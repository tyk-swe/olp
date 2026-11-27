package media

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

func testPart(id, name, contentType string) staging.Part {
	return staging.Part{
		Handle:      staging.Handle(id),
		Digest:      "sha256:" + strings.Repeat("a", 64),
		Filename:    name,
		ContentType: contentType,
		Size:        128,
	}
}

func TestSilentWAV(t *testing.T) {
	wav := SilentWAV(250)
	if len(wav) != 44+16000*250/1000*2 {
		t.Fatalf("unexpected wav length: %d", len(wav))
	}
	if string(wav[:4]) != "RIFF" || string(wav[8:16]) != "WAVEfmt " || string(wav[36:40]) != "data" {
		t.Fatalf("invalid wav header")
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != uint32(len(wav)-44) {
		t.Fatalf("unexpected data size: %d", got)
	}
	for _, ms := range []int{0, -1, -5000} {
		clamped := SilentWAV(ms)
		if len(clamped) != 44 {
			t.Fatalf("SilentWAV(%d) length = %d, want 44", ms, len(clamped))
		}
		if string(clamped[:4]) != "RIFF" {
			t.Fatalf("SilentWAV(%d) missing RIFF header", ms)
		}
	}
}

func TestDecodeImageGenerationValidationAndExtras(t *testing.T) {
	req, failure := DecodeImageGeneration([]byte(`{"model":"img-route","prompt":"a cat","n":2,"stream":true,"seed":9007199254740993}`))
	if failure != nil {
		t.Fatalf("unexpected failure: %v", failure)
	}
	if req.Route() != "img-route" || req.Op() != OpImageGeneration || req.Mode() != "streaming" || !req.Stream() {
		t.Fatalf("unexpected request metadata: route=%q op=%q mode=%q", req.Route(), req.Op(), req.Mode())
	}
	if units := req.SeedMediaUnits(); units == nil || *units != "2" {
		t.Fatalf("unexpected seed media units: %v", units)
	}
	if got := string(req.jsonExtra["seed"]); got != "9007199254740993" {
		t.Fatalf("expected exact extra integer, got %q", got)
	}
	if want := []string{"n", "prompt", "seed"}; !slices.Equal(req.ParameterNames(), want) {
		t.Fatalf("ParameterNames() = %v, want %v", req.ParameterNames(), want)
	}

	cases := []struct {
		name string
		body string
	}{
		{"not json", `not-json`},
		{"array", `[]`},
		{"blank prompt", `{"model":"img-route","prompt":"   "}`},
		{"zero count", `{"model":"img-route","prompt":"a cat","n":0}`},
		{"negative count", `{"model":"img-route","prompt":"a cat","n":-3}`},
		{"invalid route", `{"model":"BAD ROUTE","prompt":"a cat"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeImageGeneration([]byte(tc.body)); err == nil {
				t.Fatalf("expected validation error for %s", tc.name)
			}
		})
	}
}

func TestDecodeImageEditAndVariationValidation(t *testing.T) {
	img := testPart("img-1", "cat.png", "image/png")
	mask := testPart("mask-1", "mask.png", "image/png")

	editForm := staging.FormOf([]staging.Field{
		{Name: "model", Value: staging.Text("edit-route")},
		{Name: "prompt", Value: staging.Text("add hat")},
		{Name: "image", Value: img},
		{Name: "mask", Value: mask},
		{Name: "n", Value: staging.Text("2")},
		{Name: "stream", Value: staging.Text("true")},
		{Name: "custom_flag", Value: staging.Text("yes")},
	}, false)
	req, failure := DecodeImageEdit(editForm)
	if failure != nil {
		t.Fatalf("unexpected edit failure: %v", failure)
	}
	if got := req.Uploads(); len(got) != 2 || got[0] != img.Handle || got[1] != mask.Handle {
		t.Fatalf("unexpected uploads: %v", got)
	}
	if units := req.SeedMediaUnits(); units == nil || *units != "2" {
		t.Fatalf("unexpected seed media units: %v", units)
	}
	if req.formExtra["custom_flag"] != "yes" {
		t.Fatalf("expected formExtra custom_flag=yes, got %v", req.formExtra)
	}

	for _, count := range []string{"0", "-2"} {
		badEdit := staging.FormOf([]staging.Field{
			{Name: "model", Value: staging.Text("edit-route")},
			{Name: "prompt", Value: staging.Text("add hat")},
			{Name: "image", Value: img},
			{Name: "n", Value: staging.Text(count)},
		}, false)
		if _, err := DecodeImageEdit(badEdit); err == nil {
			t.Fatalf("expected DecodeImageEdit to reject n=%s", count)
		}

		badVar := staging.FormOf([]staging.Field{
			{Name: "model", Value: staging.Text("var-route")},
			{Name: "image", Value: img},
			{Name: "n", Value: staging.Text(count)},
		}, false)
		if _, err := DecodeImageVariation(badVar); err == nil {
			t.Fatalf("expected DecodeImageVariation to reject n=%s", count)
		}
	}
}

func TestDecodeSpeechValidation(t *testing.T) {
	req, failure := DecodeSpeech([]byte(`{"model":"tts-route","input":"hello","voice":"alloy","speed":1.25,"stream_format":"sse"}`))
	if failure != nil {
		t.Fatalf("unexpected speech failure: %v", failure)
	}
	if !req.Stream() || req.Mode() != "streaming" {
		t.Fatalf("expected streaming speech request, got mode=%q", req.Mode())
	}

	for _, body := range []string{
		`{"model":"tts-route","input":"","voice":"alloy"}`,
		`{"model":"tts-route","input":"hi","voice":""}`,
		`{"model":"tts-route","input":"hi","voice":"alloy","speed":0.2}`,
		`{"model":"tts-route","input":"hi","voice":"alloy","speed":4.5}`,
	} {
		if _, err := DecodeSpeech([]byte(body)); err == nil {
			t.Fatalf("expected DecodeSpeech error for %s", body)
		}
	}
}

func TestDecodeTranscriptionAndTranslation(t *testing.T) {
	audio := testPart("aud-1", "clip.wav", "audio/wav")

	validForm := staging.FormOf([]staging.Field{
		{Name: "model", Value: staging.Text("stt-route")},
		{Name: "file", Value: audio},
		{Name: "response_format", Value: staging.Text("diarized_json")},
		{Name: "known_speaker_names[]", Value: staging.Text("Alice")},
		{Name: "known_speaker_references[]", Value: staging.Text("data:audio/wav;base64,AAAA")},
		{Name: "chunking_strategy", Value: staging.Text(`{"type":"server_vad"}`)},
	}, false)
	req, failure := DecodeTranscription(validForm)
	if failure != nil {
		t.Fatalf("unexpected transcription failure: %v", failure)
	}
	if req.TranscriptFormat() != "diarized_json" {
		t.Fatalf("TranscriptFormat() = %q", req.TranscriptFormat())
	}

	invalidForms := []struct {
		name   string
		fields []staging.Field
	}{
		{"missing file", []staging.Field{{Name: "model", Value: staging.Text("stt-route")}}},
		{"temperature NaN", []staging.Field{{Name: "model", Value: staging.Text("stt-route")}, {Name: "file", Value: audio}, {Name: "temperature", Value: staging.Text("NaN")}}},
		{"temperature > 1", []staging.Field{{Name: "model", Value: staging.Text("stt-route")}, {Name: "file", Value: audio}, {Name: "temperature", Value: staging.Text("1.5")}}},
		{"granularities without verbose_json", []staging.Field{{Name: "model", Value: staging.Text("stt-route")}, {Name: "file", Value: audio}, {Name: "timestamp_granularities[]", Value: staging.Text("word")}}},
		{"include without json", []staging.Field{{Name: "model", Value: staging.Text("stt-route")}, {Name: "file", Value: audio}, {Name: "response_format", Value: staging.Text("text")}, {Name: "include[]", Value: staging.Text("logprobs")}}},
		{"speaker bad reference", []staging.Field{{Name: "model", Value: staging.Text("stt-route")}, {Name: "file", Value: audio}, {Name: "response_format", Value: staging.Text("diarized_json")}, {Name: "known_speaker_names[]", Value: staging.Text("Alice")}, {Name: "known_speaker_references[]", Value: staging.Text("https://example.com/a.wav")}}},
		{"invalid chunking", []staging.Field{{Name: "model", Value: staging.Text("stt-route")}, {Name: "file", Value: audio}, {Name: "chunking_strategy", Value: staging.Text("invalid")}}},
	}
	for _, tc := range invalidForms {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeTranscription(staging.FormOf(tc.fields, false)); err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
		})
	}

	badTranslation := staging.FormOf([]staging.Field{
		{Name: "model", Value: staging.Text("tr-route")},
		{Name: "file", Value: audio},
		{Name: "language", Value: staging.Text("en")},
	}, false)
	if _, err := DecodeTranslation(badTranslation); err == nil || err.Code != "unsupported_parameter" {
		t.Fatalf("expected unsupported_parameter for translation control, got %v", err)
	}
}

func TestDecodeVideoCreateAndQueryValidators(t *testing.T) {
	ref := testPart("ref-1", "frame.png", "image/png")
	req, failure := DecodeVideoCreate(staging.FormOf([]staging.Field{
		{Name: "model", Value: staging.Text("vid-route")},
		{Name: "prompt", Value: staging.Text("ocean waves")},
		{Name: "seconds", Value: staging.Text("8")},
		{Name: "size", Value: staging.Text("1280x720")},
		{Name: "input_reference", Value: ref},
	}, false))
	if failure != nil {
		t.Fatalf("unexpected video create failure: %v", failure)
	}
	if req.Mode() != "async" {
		t.Fatalf("expected async mode, got %q", req.Mode())
	}
	if sec, ok := req.VideoSeconds(); !ok || sec != "8" {
		t.Fatalf("VideoSeconds() = (%q, %v), want (8, true)", sec, ok)
	}

	nonImageRef := testPart("ref-2", "clip.mp4", "video/mp4")
	if _, err := DecodeVideoCreate(staging.FormOf([]staging.Field{
		{Name: "model", Value: staging.Text("vid-route")},
		{Name: "prompt", Value: staging.Text("ocean")},
		{Name: "input_reference", Value: nonImageRef},
	}, false)); err == nil {
		t.Fatalf("expected non-image input_reference to fail")
	}

	cursor := uuid.New()
	listQ, err := ValidateVideoListQuery(url.Values{"limit": {"50"}, "order": {"asc"}, "after": {cursor.String()}})
	if err != nil {
		t.Fatalf("unexpected list query error: %v", err)
	}
	if listQ.Limit != 50 || listQ.Order != OrderAscending || listQ.After == nil || *listQ.After != cursor {
		t.Fatalf("unexpected list query: %+v", listQ)
	}
	for _, badQ := range []url.Values{
		{"extra": {"1"}},
		{"limit": {"0"}},
		{"limit": {"101"}},
		{"order": {"sideways"}},
		{"after": {"not-a-uuid"}},
	} {
		if _, err := ValidateVideoListQuery(badQ); err == nil {
			t.Fatalf("expected ValidateVideoListQuery error for %v", badQ)
		}
	}

	if variant, err := ValidateVideoContentQuery(url.Values{"variant": {"thumbnail"}}); err != nil || variant != "thumbnail" {
		t.Fatalf("unexpected content query result: (%q, %v)", variant, err)
	}
	if _, err := ValidateVideoContentQuery(url.Values{"variant": {"invalid"}}); err == nil {
		t.Fatalf("expected invalid variant to fail")
	}

	if !ValidUpstreamJobID("vid_123-abc") || ValidUpstreamJobID("") || ValidUpstreamJobID("bad\x00id") || ValidUpstreamJobID(" bad ") {
		t.Fatalf("ValidUpstreamJobID mismatch")
	}
	if path, err := VideoJobPath("vid_123", "content"); err != nil || path != "videos/vid_123/content" {
		t.Fatalf("VideoJobPath() = (%q, %v)", path, err)
	}
	if _, err := VideoJobPath("../etc", ""); err == nil {
		t.Fatalf("expected path traversal job ID to fail")
	}
}

func TestWriteFormResolveAndOutboundParameterNames(t *testing.T) {
	part := testPart("p1", `a"b.png`, "image/png")
	fields := []staging.Field{
		{Name: "model", Value: staging.Text("gpt-image-1")},
		{Name: "prompt", Value: staging.Text("sunset")},
		{Name: "image", Value: part},
		{Name: "mask", Value: testPart("p2", "m.png", "")},
	}

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	open := func(p staging.Part) (io.ReadCloser, staging.Part, error) {
		return io.NopCloser(strings.NewReader("payload-" + string(p.Handle))), p, nil
	}
	if err := WriteForm(writer, fields, open); err != nil {
		t.Fatalf("WriteForm failed: %v", err)
	}
	reader := multipart.NewReader(&buf, writer.Boundary())
	got := map[string]string{}
	for {
		p, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("NextPart failed: %v", err)
		}
		data, _ := io.ReadAll(p)
		got[p.FormName()] = string(data)
	}
	if got["model"] != "gpt-image-1" || got["prompt"] != "sunset" || got["image"] != "payload-p1" || got["mask"] != "payload-p2" {
		t.Fatalf("unexpected multipart fields: %v", got)
	}

	badWriter := multipart.NewWriter(&bytes.Buffer{})
	if err := WriteForm(badWriter, []staging.Field{{Name: "broken"}}, open); err == nil {
		t.Fatalf("expected WriteForm to fail on missing value")
	}

	names, err := OutboundParameterNames(&UpstreamCall{Body: FormBody(fields)}, nil)
	if err != nil {
		t.Fatalf("OutboundParameterNames(FormBody) failed: %v", err)
	}
	if !slices.Equal(names, []string{"mask", "prompt"}) {
		t.Fatalf("OutboundParameterNames(FormBody) = %v, want [mask prompt]", names)
	}

	jsonBody, _ := json.Marshal(map[string]string{"model": "m", "prompt": "hi", "size": "1024x1024", "__olp_internal": "x"})
	jsonNames, err := OutboundParameterNames(&UpstreamCall{Body: JSONBody(jsonBody)}, nil)
	if err != nil || !slices.Equal(jsonNames, []string{"prompt", "size"}) {
		t.Fatalf("OutboundParameterNames(JSONBody) = (%v, %v)", jsonNames, err)
	}

	resolvedURL, signedCfg, err := Resolve(Absolute{URL: "https://polly.us-east-1.amazonaws.com/v1/speech", SigningService: "polly"}, connectors.Config{}, "neural")
	if err != nil || resolvedURL != "https://polly.us-east-1.amazonaws.com/v1/speech" || signedCfg.SigningService != "polly" {
		t.Fatalf("Resolve(Absolute) = (%q, %q, %v)", resolvedURL, signedCfg.SigningService, err)
	}
	if _, _, err := Resolve(nil, connectors.Config{}, ""); err == nil {
		t.Fatalf("expected Resolve(nil) to fail")
	}
}
