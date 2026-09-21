package media

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestDecodeImageGenerationValidation(t *testing.T) {
	if _, failure := DecodeImageGeneration([]byte(`{"model":"image-model","prompt":"a castle"}`)); failure != nil {
		t.Fatal(failure)
	}
	for _, body := range []string{
		`{"model":"image-model"}`,
		`{"model":"image-model","prompt":"   "}`,
		`{"model":"image-model","prompt":"x","n":0}`,
		`{"model":"","prompt":"x"}`,
		`not json`,
	} {
		if _, failure := DecodeImageGeneration([]byte(body)); failure == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestDecodeSpeechValidation(t *testing.T) {
	if _, failure := DecodeSpeech([]byte(`{"model":"tts","input":"hello","voice":"alloy"}`)); failure != nil {
		t.Fatal(failure)
	}
	for _, body := range []string{
		`{"model":"tts","voice":"alloy"}`,
		`{"model":"tts","input":"","voice":"alloy"}`,
		`{"model":"tts","input":"x","voice":""}`,
	} {
		if _, failure := DecodeSpeech([]byte(body)); failure == nil {
			t.Fatalf("accepted %s", body)
		}
	}
}

func TestDecodeVideoCreateValidation(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	state := NewAdmissionState(MinCapacityBytes)
	parse := func(fields map[string]string, files map[string][]byte) (*Request, *Error) {
		contentType, body := buildMultipartTyped(t, fields, files, "image/png")
		r := httptest.NewRequest("POST", "/v1/videos", body)
		r.Header.Set("Content-Type", contentType)
		form, err := ParseMultipart(t.Context(), r, spool,
			&Admission{Lease: state.TryAdmit("k", DefaultVideoReferenceLimit), Route: RouteAdmission{Kind: RouteUnrestricted}},
			DefaultVideoReferenceLimit, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer form.Cleanup()
		return DecodeVideoCreate(form)
	}
	if _, failure := parse(map[string]string{"model": "video-model", "prompt": "waves"}, nil); failure != nil {
		t.Fatal(failure)
	}
	for _, fields := range []map[string]string{
		{"model": "video-model"},
		{"model": "video-model", "prompt": "x", "seconds": "5"},
		{"model": "video-model", "prompt": "x", "size": "640x480"},
		{"model": "video-model", "prompt": ""},
	} {
		if _, failure := parse(fields, nil); failure == nil {
			t.Fatalf("accepted %+v", fields)
		}
	}
	if _, failure := parse(map[string]string{"model": "video-model", "prompt": "waves"},
		map[string][]byte{"input_reference": []byte("rawimage")}); failure != nil {
		t.Fatal(failure)
	}
	// Unrecognized fields are provider extensions and pass through.
	if r, failure := parse(map[string]string{"model": "video-model", "prompt": "waves", "vendor_flag": "x"}, nil); failure != nil {
		t.Fatal(failure)
	} else if r.Extra["vendor_flag"] != "x" {
		t.Fatalf("extension dropped: %+v", r.Extra)
	}
}

func TestVideoListAndContentQueryValidation(t *testing.T) {
	if _, failure := ValidateVideoListQuery(url.Values{}); failure != nil {
		t.Fatal(failure)
	}
	if _, failure := ValidateVideoListQuery(url.Values{"limit": {"10"}}); failure != nil {
		t.Fatal(failure)
	}
	for _, q := range []url.Values{
		{"limit": {"0"}},
		{"limit": {"101"}},
		{"after": {"not-a-uuid"}},
		{"bogus": {"x"}},
	} {
		if _, failure := ValidateVideoListQuery(q); failure == nil {
			t.Fatalf("accepted %v", q)
		}
	}
	if variant, failure := ValidateVideoContentQuery(url.Values{}); failure != nil || variant != "" {
		t.Fatalf("variant: %q %v", variant, failure)
	}
	if _, failure := ValidateVideoContentQuery(url.Values{"variant": {"thumbnail"}}); failure != nil {
		t.Fatal(failure)
	}
	if _, failure := ValidateVideoContentQuery(url.Values{"variant": {"bogus"}}); failure == nil {
		t.Fatal("unsupported variant accepted")
	}
}

func TestValidUpstreamJobID(t *testing.T) {
	for _, ok := range []string{"video_abc-123_XYZ", "abc123"} {
		if !ValidUpstreamJobID(ok) {
			t.Fatalf("rejected %q", ok)
		}
	}
	for _, bad := range []string{"", " id", "id ", strings.Repeat("x", 1025), "id\x00nul"} {
		if ValidUpstreamJobID(bad) {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// TestVideoJobPathStrictness covers the stricter charset the provider path
// requires, beyond the permissive record identity check.
func TestVideoJobPathStrictness(t *testing.T) {
	for _, ok := range []string{"video_abc-123_XYZ", "abc123"} {
		if _, failure := VideoJobPath(ok, ""); failure != nil {
			t.Fatalf("rejected %q: %v", ok, failure)
		}
	}
	for _, bad := range []string{"", "../escape", "id with space", "id/slash", strings.Repeat("x", 257), "id.dot"} {
		if _, failure := VideoJobPath(bad, ""); failure == nil {
			t.Fatalf("accepted %q in path", bad)
		}
	}
	if path, failure := VideoJobPath("video_1", "content"); failure != nil || path != "videos/video_1/content" {
		t.Fatalf("path: %q %v", path, failure)
	}
}

func TestEncodeBuildsExpectedPaths(t *testing.T) {
	for _, tc := range []struct {
		op     string
		path   string
		method string
	}{
		{OpImageGeneration, "images/generations", "POST"},
		{OpImageEdit, "images/edits", "POST"},
		{OpImageVariation, "images/variations", "POST"},
		{OpSpeech, "audio/speech", "POST"},
		{OpTranscription, "audio/transcriptions", "POST"},
		{OpVideoCreate, "videos", "POST"},
		{OpVideoList, "videos", "GET"},
		{OpVideoGet, "videos/video_1", "GET"},
		{OpVideoContent, "videos/video_1/content", "GET"},
		{OpVideoDelete, "videos/video_1", "DELETE"},
	} {
		r := &Request{Op: tc.op, Route: "route", Prompt: "p", Input: "in", Voice: "v", JobID: "video_1"}
		call, failure := Encode(r, "openai", "upstream-model")
		if failure != nil {
			t.Fatalf("%s: %v", tc.op, failure)
		}
		if call.Path != tc.path || call.Method != tc.method {
			t.Fatalf("%s: %s %s; want %s %s", tc.op, call.Method, call.Path, tc.method, tc.path)
		}
	}
}

func TestDecodeVideoObjectValidation(t *testing.T) {
	result, failure := DecodeVideoObject([]byte(`{"id":"video_1","object":"video","status":"queued","model":"m","created_at":1700000000}`))
	if failure != nil {
		t.Fatal(failure)
	}
	if result.ID != "video_1" || result.Status != "queued" {
		t.Fatalf("result: %+v", result)
	}
	if _, failure := DecodeVideoObject([]byte(`{"id":"v","object":"list","status":"queued"}`)); failure == nil {
		t.Fatal("unexpected object type accepted")
	}
	if _, failure := DecodeVideoObject([]byte(`not json`)); failure == nil {
		t.Fatal("invalid JSON accepted")
	}
}
