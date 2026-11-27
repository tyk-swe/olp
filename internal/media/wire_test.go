package media

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/platform/staging"
)

func stubStageImage(b64 string, index int) (*staging.Artifact, *Error) {
	return &staging.Artifact{Handle: staging.Handle("img-" + b64)}, nil
}

func TestImageResponseRoundTripPreservesExactNumbers(t *testing.T) {
	body := []byte(`{
		"created": 1700000000,
		"data": [
			{"b64_json": "YWJj", "revised_prompt": "bright sky", "seed": 9007199254740993},
			{"url": "https://cdn.example.com/2.png", "score": 0.12500000000000001}
		],
		"usage": {
			"input_tokens": 10,
			"output_tokens": 20,
			"total_tokens": 30,
			"input_tokens_details": {"image_tokens": 4, "text_tokens": 6},
			"output_tokens_details": {"image_tokens": 18},
			"billing_id": 9007199254740995
		},
		"request_seq": 9007199254740997
	}`)

	decoded, failure := DecodeImageResponse(body, stubStageImage)
	if failure != nil {
		t.Fatalf("DecodeImageResponse failed: %v", failure)
	}
	if decoded.CreatedAt != 1700000000 || len(decoded.Images) != 2 {
		t.Fatalf("unexpected decoded image result: %+v", decoded)
	}
	if decoded.Usage == nil || decoded.Usage.ImageInputTokens == nil || *decoded.Usage.ImageInputTokens != 4 ||
		decoded.Usage.ImageOutputTokens == nil || *decoded.Usage.ImageOutputTokens != 18 {
		t.Fatalf("unexpected usage: %+v", decoded.Usage)
	}
	if got := string(decoded.Extra["request_seq"]); got != "9007199254740997" {
		t.Fatalf("Extra lost integer precision: %s", got)
	}
	if got := string(decoded.DataExtra[0]["seed"]); got != "9007199254740993" {
		t.Fatalf("DataExtra lost integer precision: %s", got)
	}
	if got := string(decoded.UsageExtra["billing_id"]); got != "9007199254740995" {
		t.Fatalf("UsageExtra lost integer precision: %s", got)
	}

	encoded, markers, failure := EncodeImageResponse(decoded)
	if failure != nil {
		t.Fatalf("EncodeImageResponse failed: %v", failure)
	}
	if len(markers) != 1 || !strings.Contains(string(encoded), markers[0]) {
		t.Fatalf("expected 1 streamed marker in encoded output, got %v", markers)
	}
	for _, exact := range []string{"9007199254740993", "9007199254740995", "9007199254740997", "0.12500000000000001"} {
		if !strings.Contains(string(encoded), exact) {
			t.Fatalf("encoded image response did not preserve exact literal %s: %s", exact, string(encoded))
		}
	}

	for _, bad := range []string{
		`null`,
		`[]`,
		`{"data":[]}`,
		`{"data":[{"url":"https://x","b64_json":"AA=="}]}`,
		`{"data":[{}]}`,
	} {
		if _, err := DecodeImageResponse([]byte(bad), stubStageImage); err == nil {
			t.Fatalf("expected DecodeImageResponse error for %s", bad)
		}
	}
}

func TestTranscriptionJSONRoundTripAndSegmentValidation(t *testing.T) {
	body := []byte(`{
		"text": "hello world",
		"language": "en",
		"duration": 3.5,
		"segments": [
			{"id": 1, "start": 0.0, "end": 1.5, "text": "hello", "speaker": "A", "tokens": [9007199254740993]},
			{"id": "seg-2", "start": 1.5, "end": 3.5, "text": "world"}
		],
		"usage": {"type": "tokens", "input_tokens": 12, "output_tokens": 5, "total_tokens": 17, "input_token_details": {"audio_tokens": 10}},
		"trace_id": 9007199254740999
	}`)

	decoded, failure := DecodeTranscriptionJSON(body)
	if failure != nil {
		t.Fatalf("DecodeTranscriptionJSON failed: %v", failure)
	}
	if decoded.Text != "hello world" || len(decoded.Segments) != 2 {
		t.Fatalf("unexpected decoded transcript: %+v", decoded)
	}
	if decoded.Tokens == nil || decoded.Tokens.AudioInputTokens == nil || *decoded.Tokens.AudioInputTokens != 10 {
		t.Fatalf("unexpected tokens: %+v", decoded.Tokens)
	}

	encoded, failure := EncodeTranscriptionJSON(decoded)
	if failure != nil {
		t.Fatalf("EncodeTranscriptionJSON failed: %v", failure)
	}
	for _, exact := range []string{"9007199254740993", "9007199254740999", `"seg-2"`} {
		if !strings.Contains(string(encoded), exact) {
			t.Fatalf("encoded transcript missing %s: %s", exact, string(encoded))
		}
	}

	textOnly := *decoded
	textOnly.TextOnly = true
	onlyBytes, failure := EncodeTranscriptionJSON(&textOnly)
	if failure != nil || string(onlyBytes) != `{"text":"hello world"}` {
		t.Fatalf("TextOnly encode = (%s, %v)", string(onlyBytes), failure)
	}

	emptySegsBytes, failure := EncodeTranscriptionJSON(&TranscriptionResult{Text: "ok", Segments: []TranscriptionSegment{}})
	if failure != nil || !strings.Contains(string(emptySegsBytes), `"segments":[]`) {
		t.Fatalf("empty segments should be preserved: %s (%v)", string(emptySegsBytes), failure)
	}

	for _, bad := range []string{
		`null`,
		`{}`,
		`{"text":"hi","segments":[{"id":1.5,"start":0,"end":1,"text":"a"}]}`,
		`{"text":"hi","segments":[{"id":true,"start":0,"end":1,"text":"a"}]}`,
	} {
		if _, err := DecodeTranscriptionJSON([]byte(bad)); err == nil {
			t.Fatalf("expected DecodeTranscriptionJSON error for %s", bad)
		}
	}
}

func TestVideoWireRoundTripStrictAndList(t *testing.T) {
	rawVideo := []byte(`{
		"id": "up-1",
		"object": "video",
		"model": "sora-2",
		"status": "failed",
		"progress": 50,
		"created_at": 1700000001,
		"prompt": "waves",
		"seconds": "4",
		"size": "1280x720",
		"remixed_from_video_id": "up-0",
		"error": {"code": "mod", "message": "blocked"},
		"vendor_seq": 9007199254740993
	}`)
	decoded, failure := DecodeVideoObject(rawVideo)
	if failure != nil {
		t.Fatalf("DecodeVideoObject failed: %v", failure)
	}
	if _, leaked := decoded.Extra["remixed_from_video_id"]; leaked {
		t.Fatalf("remixed_from_video_id must be excluded from Extra")
	}
	if string(decoded.Extra["vendor_seq"]) != "9007199254740993" {
		t.Fatalf("Extra lost precision: %s", string(decoded.Extra["vendor_seq"]))
	}

	encoded, failure := EncodeVideoObject(decoded, "local-1", "vid-route")
	if failure != nil {
		t.Fatalf("EncodeVideoObject failed: %v", failure)
	}
	if !strings.Contains(string(encoded), `"id":"local-1"`) || !strings.Contains(string(encoded), `"model":"vid-route"`) ||
		!strings.Contains(string(encoded), "9007199254740993") {
		t.Fatalf("unexpected encoded video object: %s", string(encoded))
	}

	listBytes, failure := EncodeVideoListResponse(&VideoListResult{
		Jobs:    []VideoJobResult{{ID: "local-1", Route: "vid-route", Status: "completed", Extra: decoded.Extra}},
		FirstID: new("local-1"),
		LastID:  new("local-1"),
		HasMore: true,
	})
	if failure != nil {
		t.Fatalf("EncodeVideoListResponse failed: %v", failure)
	}
	if !strings.Contains(string(listBytes), `"object":"list"`) || !strings.Contains(string(listBytes), "9007199254740993") {
		t.Fatalf("unexpected video list JSON: %s", string(listBytes))
	}

	doc, err := oif.ParseJSON(rawVideo, oif.Limits{})
	if err != nil {
		t.Fatalf("ParseJSON failed: %v", err)
	}
	if !ValidStrictVideoJobSource(doc, "up-1", "sora-2") {
		t.Fatalf("expected ValidStrictVideoJobSource to pass")
	}
	if ValidStrictVideoJobSource(doc, "wrong", "sora-2") {
		t.Fatalf("expected mismatched upstream ID to fail")
	}
	projected, failure := EncodeStrictVideoObject(doc, "up-1", "local-99", "vid-route")
	if failure != nil {
		t.Fatalf("EncodeStrictVideoObject failed: %v", failure)
	}
	var strictObj map[string]json.RawMessage
	if err := json.Unmarshal(projected, &strictObj); err != nil {
		t.Fatalf("projected JSON invalid: %v", err)
	}
	if string(strictObj["id"]) != `"local-99"` || string(strictObj["model"]) != `"vid-route"` {
		t.Fatalf("unexpected strict projection: %s", string(projected))
	}
	if _, err := EncodeStrictVideoObject(doc, "other", "local-99", "vid-route"); err == nil {
		t.Fatalf("expected identity mismatch error")
	}

	del, failure := DecodeVideoDeleteResponse([]byte(`{"id":"up-1","deleted":true,"receipt_no":9007199254740995}`))
	if failure != nil || !del.Deleted || string(del.Extra["receipt_no"]) != "9007199254740995" {
		t.Fatalf("DecodeVideoDeleteResponse = (%+v, %v)", del, failure)
	}
	delBytes, failure := EncodeVideoDeleteResponse(del, "local-1")
	if failure != nil || !strings.Contains(string(delBytes), `"object":"video.deleted"`) || !strings.Contains(string(delBytes), "9007199254740995") {
		t.Fatalf("EncodeVideoDeleteResponse = (%s, %v)", string(delBytes), failure)
	}
}

func TestObserveMediaUsageAndHelpers(t *testing.T) {
	imgUsage := ObserveMediaUsage(`{"usage":{"input_tokens":10,"output_tokens":20,"input_tokens_details":{"cached_tokens":3,"image_tokens":7},"output_tokens_details":{"image_tokens":19}}}`, OpImageGeneration)
	if imgUsage == nil || *imgUsage.Input != 10 || *imgUsage.Output != 20 || *imgUsage.Cached != 3 || *imgUsage.ImageInput != 7 || *imgUsage.ImageOutput != 19 {
		t.Fatalf("unexpected image usage: %+v", imgUsage)
	}

	sttUsage := ObserveMediaUsage(`{"usage":{"prompt_tokens":11,"completion_tokens":4,"prompt_tokens_details":{"cached_tokens":2},"input_token_details":{"audio_tokens":9}}}`, OpTranscription)
	if sttUsage == nil || *sttUsage.Input != 11 || *sttUsage.Output != 4 || *sttUsage.Cached != 2 || *sttUsage.AudioInput != 9 {
		t.Fatalf("unexpected transcription usage: %+v", sttUsage)
	}

	ttsUsage := ObserveMediaUsage(`{"usage":{"input_tokens":5,"output_tokens":30}}`, OpSpeech)
	if ttsUsage == nil || ttsUsage.AudioOutput == nil || *ttsUsage.AudioOutput != 30 {
		t.Fatalf("unexpected speech usage: %+v", ttsUsage)
	}

	if ObserveMediaUsage(`not json`, OpSpeech) != nil || ObserveMediaUsage(`{"type":"delta"}`, OpSpeech) != nil {
		t.Fatalf("expected nil usage when absent or invalid")
	}

	if !IsMediaTerminal("response.completed") || !IsMediaTerminal("transcript.text.done") || !IsMediaTerminal("speech.audio.done") || IsMediaTerminal("transcript.text.delta") {
		t.Fatalf("IsMediaTerminal mismatch")
	}
	if UnixTime(nil) != nil || UnixTime(new(int64(1700000000))).Unix() != 1700000000 {
		t.Fatalf("UnixTime mismatch")
	}
}
