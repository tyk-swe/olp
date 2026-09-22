package media

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
)

func mediaDefaultsConfig(t *testing.T, operation string, values, native map[string]json.RawMessage) connectors.Config {
	t.Helper()
	profile, err := connectors.LookupProfile("openai-chat", "1")
	if err != nil {
		t.Fatal(err)
	}
	return connectors.Config{
		Kind: "openai", AuthMode: "api_key", ProfileID: profile.ID, ProfileRevision: profile.Revision,
		OperationDefaults: map[string]connectors.DefaultSet{operation: {Dialect: profile.OperationDialect(operation), Values: values, NativeOptions: native}},
	}
}

func TestConfiguredImageDefaultsPreserveSourcePresenceAndNumbers(t *testing.T) {
	body := []byte(`{"model":"images","prompt":"original","n":null,"size":"","output_compression":0,"vendor_false":false,"vendor_array":[],"vendor_schema":{"properties":{"constant":{"const":123456789012345678901234567890}}}}`)
	r, failure := DecodeImageGeneration(body)
	if failure != nil {
		t.Fatal(failure)
	}
	cfg := mediaDefaultsConfig(t, OpImageGeneration, map[string]json.RawMessage{
		"n": json.RawMessage(`3`), "size": json.RawMessage(`"1024x1024"`), "output_compression": json.RawMessage(`80`), "quality": json.RawMessage(`"high"`),
	}, map[string]json.RawMessage{"vendor_false": json.RawMessage(`true`), "vendor_array": json.RawMessage(`["default"]`), "vendor_schema": json.RawMessage(`{"other":"default"}`)})
	before, _ := json.Marshal(r)
	call, effective, failure := EncodeConfigured(r, cfg, "native-image-model")
	if failure != nil {
		t.Fatal(failure)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(call.JSON, &got); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"model": `"native-image-model"`, "n": `null`, "size": `""`, "output_compression": `0`, "vendor_false": `false`, "vendor_array": `[]`, "quality": `"high"`, "vendor_schema": `{"properties":{"constant":{"const":123456789012345678901234567890}}}`} {
		if string(got[name]) != want {
			t.Errorf("%s: got %s, want %s", name, got[name], want)
		}
	}
	if _, exists := got["style"]; exists {
		t.Fatal("omitted field became native null")
	}
	if effective.Count != nil || effective.Quality == nil || *effective.Quality != "high" {
		t.Fatalf("effective controls: %+v", effective)
	}
	after, _ := json.Marshal(r)
	if !bytes.Equal(before, after) {
		t.Fatal("source request mutated by defaults")
	}
	body[0] = '['
	if string(r.SourceFields["prompt"]) != `"original"` {
		t.Fatal("source fields alias caller input")
	}
	effective.SourceFields["prompt"][1] = 'X'
	*effective.Size = "changed"
	effective.Extra["vendor_schema"].(map[string]any)["changed"] = true
	if *r.Size != "" || string(r.SourceFields["prompt"]) != `"original"` {
		t.Fatal("effective request aliases source metadata")
	}
	if _, changed := r.Extra["vendor_schema"].(map[string]any)["changed"]; changed {
		t.Fatal("effective extension map aliases source")
	}
}

func TestConfiguredJSONDefaultsUseBindingAtomsAndRetainLegacyEncoding(t *testing.T) {
	r, failure := DecodeImageGeneration([]byte(`{"model":"images","prompt":"p"}`))
	if failure != nil {
		t.Fatal(failure)
	}
	cfg := mediaDefaultsConfig(t, OpImageGeneration, map[string]json.RawMessage{"n": json.RawMessage(`2`)}, map[string]json.RawMessage{"vendor_options": json.RawMessage(`{"provider":true,"nested":{"first":1}}`)})
	cfg.Bindings = map[string]connectors.Binding{"chosen": {Model: "resolved", Defaults: map[string]connectors.DefaultSet{OpImageGeneration: {
		Dialect:       cfg.OperationDefaults[OpImageGeneration].Dialect,
		NativeOptions: map[string]json.RawMessage{"vendor_options": json.RawMessage(`{"binding":true,"nested":[]}`)},
	}}}}
	call, effective, failure := EncodeConfigured(r, cfg, "chosen")
	if failure != nil {
		t.Fatal(failure)
	}
	var body map[string]json.RawMessage
	json.Unmarshal(call.JSON, &body)
	if string(body["model"]) != `"resolved"` || string(body["vendor_options"]) != `{"binding":true,"nested":[]}` || effective.Count == nil || *effective.Count != 2 {
		t.Fatalf("binding defaults: %s %+v", call.JSON, effective.Count)
	}
	legacy := connectors.Config{Kind: "openai", OperationDefaults: cfg.OperationDefaults}
	want, failure := Encode(r, legacy.Kind, "chosen")
	if failure != nil {
		t.Fatal(failure)
	}
	got, _, failure := EncodeConfigured(r, legacy, "chosen")
	if failure != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("legacy encoding changed")
	}
}

func TestConfiguredSpeechKeepsNullAndDeliveryMode(t *testing.T) {
	for _, test := range []struct{ name, body, format string }{
		{"omitted", `{"model":"tts","input":"hello","voice":"alloy"}`, `"wav"`},
		{"native null", `{"model":"tts","input":"hello","voice":"alloy","response_format":null,"speed":null,"instructions":""}`, `null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, failure := DecodeSpeech([]byte(test.body))
			if failure != nil {
				t.Fatal(failure)
			}
			cfg := mediaDefaultsConfig(t, OpSpeech, map[string]json.RawMessage{"response_format": json.RawMessage(`"wav"`), "speed": json.RawMessage(`1.5`), "instructions": json.RawMessage(`"default voice instruction"`)}, nil)
			call, effective, failure := EncodeConfigured(r, cfg, "tts-native")
			if failure != nil {
				t.Fatal(failure)
			}
			var body map[string]json.RawMessage
			json.Unmarshal(call.JSON, &body)
			if string(body["response_format"]) != test.format || call.Kind != ResponseBinary || effective.Stream {
				t.Fatalf("speech format/mode changed: %s", call.JSON)
			}
			if test.name == "native null" && (string(body["speed"]) != "null" || string(body["instructions"]) != `""`) {
				t.Fatalf("speech presence lost: %s", call.JSON)
			}
		})
	}
	r, failure := DecodeSpeech([]byte(`{"model":"tts","input":"hello","voice":"alloy"}`))
	if failure != nil {
		t.Fatal(failure)
	}
	cfg := mediaDefaultsConfig(t, OpSpeech, map[string]json.RawMessage{"stream_format": json.RawMessage(`"sse"`)}, nil)
	if call, _, failure := EncodeConfigured(r, cfg, "tts-native"); failure == nil || call != nil || failure.Code != "unsupported_parameter" {
		t.Fatal("default changed unary speech to streaming")
	}
}

func TestConfiguredMultipartPreservesFilesAndAppendsAtomicDefaults(t *testing.T) {
	spool := testSpool(t, MinCapacityBytes)
	state := NewAdmissionState(MinCapacityBytes)
	contentType, body := buildMultipartTyped(t,
		map[string]string{"model": "images", "prompt": "p", "quality": "", "output_compression": "0"},
		map[string][]byte{"image[0]": []byte("first image bytes"), "image[1]": []byte("second image bytes"), "mask": []byte("mask bytes")}, "image/png")
	req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", body)
	req.Header.Set("Content-Type", contentType)
	form, err := ParseMultipart(t.Context(), req, spool, parseAdmission(t, state, int64(body.Len())), 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer form.Cleanup()
	r, failure := DecodeImageEdit(form)
	if failure != nil {
		t.Fatal(failure)
	}
	cfg := mediaDefaultsConfig(t, OpImageEdit, map[string]json.RawMessage{"n": json.RawMessage(`2`), "quality": json.RawMessage(`"high"`), "output_compression": json.RawMessage(`80`)}, map[string]json.RawMessage{"vendor_array": json.RawMessage(`[]`), "vendor_schema": json.RawMessage(`{"required":["x"],"properties":{"x":{"const":42}}}`)})
	original, failure := Encode(r, cfg.Kind, "native")
	if failure != nil {
		t.Fatal(failure)
	}
	call, effective, failure := EncodeConfigured(r, cfg, "native")
	if failure != nil {
		t.Fatal(failure)
	}
	if len(call.Fields) <= len(original.Fields) || !reflect.DeepEqual(call.Fields[:len(original.Fields)], original.Fields) {
		t.Fatal("caller fields or file ordering changed")
	}
	if effective.Count == nil || *effective.Count != 2 || r.Count != nil {
		t.Fatal("effective image count did not detach from source")
	}
	wantFiles := []string{"first image bytes", "second image bytes", "mask bytes"}
	var gotFiles []string
	for _, field := range call.Fields {
		if field.File == nil {
			continue
		}
		opened, err := spool.Open(field.File.Handle)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(opened.File)
		opened.File.Close()
		if err != nil {
			t.Fatal(err)
		}
		gotFiles = append(gotFiles, string(data))
	}
	if !reflect.DeepEqual(gotFiles, wantFiles) {
		t.Fatalf("file bytes/order: %v", gotFiles)
	}
	values := map[string]string{}
	for _, field := range call.Fields {
		if field.Text != nil {
			values[field.Name] = *field.Text
		}
	}
	if values["vendor_array"] != "[]" || values["vendor_schema"] != `{"required":["x"],"properties":{"x":{"const":42}}}` || values["quality"] != "" || values["output_compression"] != "0" {
		t.Fatalf("multipart defaults lost shape/presence: %+v", values)
	}
	effective.Images[0].Filename = "changed"
	if r.Images[0].Filename == "changed" {
		t.Fatal("effective files alias source metadata")
	}
}

func TestConfiguredMultipartArrayDefaultsAreAtomicAndOrdered(t *testing.T) {
	r := &Request{Op: OpTranscription, Route: "route", File: &Part{Handle: "staged-file"}, Format: new("verbose_json"), Temperature: new(float64(0)), Language: new("")}
	cfg := mediaDefaultsConfig(t, OpTranscription, map[string]json.RawMessage{
		"temperature": json.RawMessage(`0.5`), "language": json.RawMessage(`"en"`),
		"timestamp_granularities": json.RawMessage(`["segment","word"]`),
		"chunking_strategy":       json.RawMessage(`{"type":"server_vad","silence_duration_ms":500}`),
	}, nil)
	call, effective, failure := EncodeConfigured(r, cfg, "transcription-model")
	if failure != nil {
		t.Fatal(failure)
	}
	var granularities []string
	values := map[string]string{}
	for _, field := range call.Fields {
		if field.Text == nil {
			continue
		}
		values[field.Name] = *field.Text
		if field.Name == "timestamp_granularities[]" {
			granularities = append(granularities, *field.Text)
		}
	}
	if !reflect.DeepEqual(granularities, []string{"segment", "word"}) || values["temperature"] != "0" || values["language"] != "" || values["chunking_strategy"] != `{"type":"server_vad","silence_duration_ms":500}` {
		t.Fatalf("multipart control shape/order changed: %+v %v", values, granularities)
	}
	if !reflect.DeepEqual(effective.TimestampGranularities, granularities) || r.TimestampGranularities != nil || r.ChunkingStrategy != nil {
		t.Fatal("array defaults did not detach from source")
	}
}

func TestConfiguredTranscriptionFormatSelectsActualResponseDecoder(t *testing.T) {
	transport, server := testTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		form, err := r.MultipartReader()
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		values := map[string]string{}
		for {
			part, err := form.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := io.ReadAll(part)
			values[part.FormName()] = string(data)
		}
		if values["response_format"] != "text" || values["file"] != "audio bytes" {
			t.Errorf("wrong effective request: %+v", values)
		}
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "native transcript")
	}))
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	writer.WriteField("model", "transcribe")
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
	defer form.Cleanup()
	r, failure := DecodeTranscription(form)
	if failure != nil {
		t.Fatal(failure)
	}
	cfg := mediaDefaultsConfig(t, OpTranscription, map[string]json.RawMessage{"response_format": json.RawMessage(`"text"`)}, nil)
	call, effective, failure := EncodeConfigured(r, cfg, "whisper-1")
	if failure != nil {
		t.Fatal(failure)
	}
	if effective.Format == nil || *effective.Format != "text" || r.Format != nil {
		t.Fatal("response format not detached")
	}
	result, upstreamFailure := transport.Do(t.Context(), testTarget(server.URL), call, effective)
	if upstreamFailure != nil || result == nil || string(result.Text) != "native transcript" {
		t.Fatalf("text response was not decoded: %+v %v", result, upstreamFailure)
	}
}

func TestConfiguredMediaRejectsUnrepresentableDefaultsAndCollisions(t *testing.T) {
	for _, test := range []struct {
		name           string
		request        *Request
		values, native map[string]json.RawMessage
	}{
		{"multipart null", &Request{Op: OpTranscription, Route: "route"}, map[string]json.RawMessage{"temperature": json.RawMessage(`null`)}, nil},
		{"empty repeated array", &Request{Op: OpTranscription, Route: "route"}, map[string]json.RawMessage{"timestamp_granularities": json.RawMessage(`[]`)}, nil},
		{"decoder conflict", &Request{Op: OpTranscription, Route: "route", Include: []string{"logprobs"}}, map[string]json.RawMessage{"response_format": json.RawMessage(`"text"`)}, nil},
		{"bad speed", &Request{Op: OpSpeech, Route: "route", Input: "hi", Voice: "alloy"}, map[string]json.RawMessage{"speed": json.RawMessage(`20`)}, nil},
		{"native control collision", &Request{Op: OpImageGeneration, Route: "route", Prompt: "p"}, map[string]json.RawMessage{"quality": json.RawMessage(`"high"`)}, map[string]json.RawMessage{"quality": json.RawMessage(`"low"`)}},
		{"multipart member collision", &Request{Op: OpTranscription, Route: "route", Extra: map[string]any{"vendor[0]": "caller"}}, nil, map[string]json.RawMessage{"vendor": json.RawMessage(`["default"]`)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := mediaDefaultsConfig(t, test.request.Op, test.values, test.native)
			call, _, failure := EncodeConfigured(test.request, cfg, "native")
			if failure == nil || call != nil {
				t.Fatalf("unresolved default admitted: %+v", call)
			}
		})
	}
}

func TestConfiguredCloudImageDefaultsKeepQualifiedWrapperMappings(t *testing.T) {
	for _, test := range []struct{ id, kind, auth, model, native string }{
		{"vertex-gemini", "vertex_ai", "adc", "imagen-3.0-generate-002", "vertex_ai"},
		{"bedrock-converse", "bedrock", "default_chain", "amazon.titan-image-generator-v2:0", "bedrock"},
	} {
		t.Run(test.id, func(t *testing.T) {
			profile, err := connectors.LookupProfile(test.id, "1")
			if err != nil {
				t.Fatal(err)
			}
			cfg := connectors.Config{Kind: test.kind, AuthMode: test.auth, ProfileID: test.id, ProfileRevision: "1", OperationDefaults: map[string]connectors.DefaultSet{OpImageGeneration: {Dialect: profile.OperationDialect(OpImageGeneration), Values: map[string]json.RawMessage{"n": json.RawMessage(`2`), "size": json.RawMessage(`"1024x1024"`)}}}}
			r, failure := DecodeImageGeneration([]byte(`{"model":"images","prompt":"unaltered"}`))
			if failure != nil {
				t.Fatal(failure)
			}
			call, effective, failure := EncodeConfigured(r, cfg, test.model)
			if failure != nil {
				t.Fatal(failure)
			}
			if effective.Count == nil || *effective.Count != 2 || call.Native != test.native || bytes.Contains(call.JSON, []byte(`"n"`)) {
				t.Fatalf("wrapper changed: %s", call.JSON)
			}
			set := cfg.OperationDefaults[OpImageGeneration]
			set.NativeOptions = map[string]json.RawMessage{"parameters": json.RawMessage(`{"sampleCount":3}`)}
			cfg.OperationDefaults[OpImageGeneration] = set
			if call, _, failure := EncodeConfigured(r, cfg, test.model); failure == nil || call != nil || !strings.Contains(failure.Message, "parameters") {
				t.Fatal("unqualified wrapper extension accepted")
			}
			set.NativeOptions = nil
			cfg.OperationDefaults[OpImageGeneration] = set
			r, failure = DecodeImageGeneration([]byte(`{"model":"images","prompt":"unaltered","n":null}`))
			if failure != nil {
				t.Fatal(failure)
			}
			if call, _, failure := EncodeConfigured(r, cfg, test.model); failure == nil || call != nil {
				t.Fatal("cloud wrapper erased explicit native null")
			}
		})
	}
}

func TestMediaSourceJSONRejectsAmbiguousNestedMembersAndUnicode(t *testing.T) {
	for _, body := range []string{
		`{"model":"images","prompt":"one","prompt":"two"}`,
		`{"model":"images","prompt":"one"} {}`,
		`{"model":"images","prompt":"one","native_extension":{"path":1,"\u0070ath":2}}`,
		`{"model":"images","prompt":"one","native_extension":"\ud800"}`,
	} {
		if _, failure := DecodeImageGeneration([]byte(body)); failure == nil {
			t.Fatal("ambiguous source body accepted")
		}
	}
	for _, body := range []string{
		`{"model":"voice","input":"hello","voice":"fixture","native_extension":{"x":1,"x":2}}`,
		`{"model":"voice","input":"hello","voice":"fixture","native_extension":"\ud800"}`,
	} {
		if _, failure := DecodeSpeech([]byte(body)); failure == nil {
			t.Fatal("ambiguous speech source body accepted")
		}
	}
}

func TestMediaSourceRetainsNativeNumberLexemesAndOwnsInput(t *testing.T) {
	input := []byte(`{"model":"images","prompt":"one","native_extension":{"long":9007199254740993,"minus_zero":-0,"tiny":1e-400,"null":null}}`)
	r, failure := DecodeImageGeneration(input)
	if failure != nil {
		t.Fatal(failure)
	}
	want := `{"long":9007199254740993,"minus_zero":-0,"tiny":1e-400,"null":null}`
	if string(r.SourceFields["native_extension"]) != want {
		t.Fatal("native source number or presence changed")
	}
	if r.SourceDocument().Raw() != string(input) {
		t.Fatal("complete immutable media source changed before encoding")
	}
	start := bytes.Index(input, []byte(want))
	if start < 0 {
		t.Fatal("fixture has no native source")
	}
	input[start] = 'x'
	if string(r.SourceFields["native_extension"]) != want {
		t.Fatal("caller mutation changed the retained native source")
	}
	if r.SourceDocument().Raw() == string(input) {
		t.Fatal("complete immutable media source aliased caller bytes")
	}
	r.SourceFields["native_extension"][1] = 'X'
	if input[start+1] != '"' {
		t.Fatal("retained source mutation changed caller bytes")
	}
}
