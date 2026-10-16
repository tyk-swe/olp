package media

import (
	"net/url"
	"testing"
)

// Unrecognized fields are provider extensions and pass through.

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
