package media

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTranscriptionJSONRoundTripsSegmentIDs(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantID string
	}{
		{"diarized string id",
			`{"text":"hi","segments":[{"type":"transcript.text.segment","id":"seg_001","start":0,"end":1.5,"text":"hi","speaker":"A"}]}`,
			`"seg_001"`},
		{"verbose integer id",
			`{"text":"hi","segments":[{"id":0,"start":0,"end":1.5,"text":"hi"}]}`,
			`0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, failure := DecodeTranscriptionJSON([]byte(tc.body))
			if failure != nil {
				t.Fatal(failure)
			}
			if len(result.Segments) != 1 || string(result.Segments[0].ID) != tc.wantID {
				t.Fatalf("segments %+v", result.Segments)
			}
			body, failure := EncodeTranscriptionJSON(result)
			if failure != nil {
				t.Fatal(failure)
			}
			var doc struct {
				Segments []map[string]json.RawMessage `json:"segments"`
			}
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Fatal(err)
			}
			if len(doc.Segments) != 1 || string(doc.Segments[0]["id"]) != tc.wantID {
				t.Fatalf("encoded %s", body)
			}
			if strings.Contains(tc.body, "speaker") && (string(doc.Segments[0]["speaker"]) != `"A"` ||
				string(doc.Segments[0]["type"]) != `"transcript.text.segment"`) {
				t.Fatalf("encoded %s", body)
			}
		})
	}
}

func TestEncodeOmitsAbsentOptionalJSONFields(t *testing.T) {
	image, failure := DecodeImageGeneration([]byte(`{"model":"r","prompt":"x"}`))
	if failure != nil {
		t.Fatal(failure)
	}
	speech, failure := DecodeSpeech([]byte(`{"model":"r","input":"x","voice":"v"}`))
	if failure != nil {
		t.Fatal(failure)
	}
	counted, failure := DecodeImageGeneration([]byte(`{"model":"r","prompt":"x","n":2}`))
	if failure != nil {
		t.Fatal(failure)
	}
	for _, tc := range []struct {
		req     *Request
		absent  []string
		present map[string]string
	}{
		{image, []string{"n", "size", "quality", "response_format", "style", "user", "background",
			"moderation", "output_compression", "output_format", "partial_images"}, nil},
		{speech, []string{"instructions", "speed", "response_format"}, nil},
		{counted, nil, map[string]string{"n": "2"}},
	} {
		call, failure := Encode(tc.req, "openai_compatible", "m")
		if failure != nil {
			t.Fatal(failure)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(call.JSON, &doc); err != nil {
			t.Fatal(err)
		}
		for _, name := range tc.absent {
			if value, ok := doc[name]; ok {
				t.Errorf("%s: field %q sent as %s, want omitted", tc.req.Op, name, value)
			}
		}
		for name, want := range tc.present {
			if string(doc[name]) != want {
				t.Errorf("%s: field %q = %s, want %s", tc.req.Op, name, doc[name], want)
			}
		}
	}
}

func TestJSONExtensionNumbersKeepClientSpelling(t *testing.T) {
	r, failure := DecodeImageGeneration([]byte(`{"model":"r","prompt":"x","seed":12345678901234567891,"g":1.0,"a":[1.50],"o":{"b":2.00}}`))
	if failure != nil {
		t.Fatal(failure)
	}
	body, failure := jsonDoc(nil, r.Extra)
	if failure != nil {
		t.Fatal(failure)
	}
	for _, want := range []string{`"seed":12345678901234567891`, `"g":1.0`, `"a":[1.50]`, `"o":{"b":2.00}`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("upstream body %s lacks %s", body, want)
		}
	}
	if got := extraText(r.Extra["seed"]); got != "12345678901234567891" {
		t.Errorf("multipart extension text %q", got)
	}
}

func TestDecodeTranscriptionRejectsInvalidTemperature(t *testing.T) {
	transport, _ := testTransport(t, http.NotFoundHandler())
	for _, tc := range []struct {
		value string
		ok    bool
	}{{"NaN", false}, {"nan", false}, {"-1", false}, {"1.5", false}, {"0.5", true}} {
		var buffer bytes.Buffer
		writer := multipart.NewWriter(&buffer)
		writer.WriteField("model", "transcribe")
		writer.WriteField("temperature", tc.value)
		file, _ := writer.CreateFormFile("file", "input.wav")
		io.WriteString(file, "audio bytes")
		writer.Close()
		req := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", &buffer)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		state := NewAdmissionState(MinCapacityBytes)
		form, err := ParseMultipart(t.Context(), req, transport.Spool, parseAdmission(t, state, int64(buffer.Len())), 1<<20, 1)
		if err != nil {
			t.Fatal(err)
		}
		r, failure := DecodeTranscription(form)
		form.Cleanup()
		if tc.ok != (failure == nil) || tc.ok != (r != nil) {
			t.Errorf("temperature %q: request %v failure %v", tc.value, r, failure)
		}
	}
}

func TestNextTerminalReconciliationHonoursExpiry(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }
	for _, tc := range []struct {
		name   string
		record JobRecord
		want   time.Time
	}{
		{"expires within a day", JobRecord{Lifecycle: LifecycleActive, CreatedAt: now, ExpiresAt: at(time.Hour)}, now.Add(time.Hour)},
		{"retention closes within a day", JobRecord{Lifecycle: LifecycleActive, CreatedAt: now.Add(-30*24*time.Hour + 2*time.Hour)}, now.Add(2 * time.Hour)},
		{"already expired", JobRecord{Lifecycle: LifecycleActive, CreatedAt: now, ExpiresAt: at(-time.Hour)}, now},
		{"no expiry close", JobRecord{Lifecycle: LifecycleActive, CreatedAt: now, ExpiresAt: at(48 * time.Hour)}, now.Add(24 * time.Hour)},
		{"not active", JobRecord{Lifecycle: LifecycleDeleted, CreatedAt: now, ExpiresAt: at(time.Hour)}, now.Add(24 * time.Hour)},
	} {
		if got := nextTerminalReconciliation(tc.record, now); !got.Equal(tc.want) {
			t.Errorf("%s: next %v, want %v", tc.name, got, tc.want)
		}
	}
}
