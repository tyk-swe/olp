package media

import (
	"encoding/json"
	"testing"
)

func int64ptr(v int64) *int64 { return &v }

func TestObserveMediaUsageImages(t *testing.T) {
	// With an output breakdown only its image share bills as image output.
	usage := ObserveMediaUsage(`{"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,
		"input_tokens_details":{"image_tokens":6},
		"output_tokens_details":{"image_tokens":3,"text_tokens":1}}}`, OpImageGeneration)
	if usage == nil || usage.ImageInput == nil || *usage.ImageInput != 6 ||
		usage.ImageOutput == nil || *usage.ImageOutput != 3 {
		t.Fatalf("usage %+v", usage)
	}
	// Without one, every output token is the produced image.
	usage = ObserveMediaUsage(`{"usage":{"input_tokens":10,"output_tokens":4}}`, OpImageEdit)
	if usage == nil || usage.ImageOutput == nil || *usage.ImageOutput != 4 {
		t.Fatalf("usage %+v", usage)
	}
}

func TestObserveMediaUsageSpeechAndTranscription(t *testing.T) {
	usage := ObserveMediaUsage(`{"usage":{"input_tokens":5,"output_tokens":20}}`, OpSpeech)
	if usage == nil || usage.AudioOutput == nil || *usage.AudioOutput != 20 {
		t.Fatalf("speech usage %+v", usage)
	}
	usage = ObserveMediaUsage(`{"usage":{"input_tokens":5,"output_tokens":3,
		"input_token_details":{"audio_tokens":4}}}`, OpTranscription)
	if usage == nil || usage.AudioInput == nil || *usage.AudioInput != 4 {
		t.Fatalf("transcription usage %+v", usage)
	}
}

func TestDecodeImageResponseCarriesImageShares(t *testing.T) {
	body := []byte(`{"created":1,"data":[{"url":"https://img"}],
		"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,
			"input_tokens_details":{"image_tokens":6},"output_tokens_details":{"image_tokens":3}}}`)
	result, failure := DecodeImageResponse(body, func(b64 string, index int) (*Artifact, *Error) {
		return &Artifact{}, nil
	})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Usage == nil || result.Usage.ImageInputTokens == nil || *result.Usage.ImageInputTokens != 6 ||
		result.Usage.ImageOutputTokens == nil || *result.Usage.ImageOutputTokens != 3 {
		t.Fatalf("usage %+v", result.Usage)
	}
}

func TestDecodeImageResponseTreatsOutputAsImage(t *testing.T) {
	body := []byte(`{"created":1,"data":[{"url":"https://img"}],
		"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,
			"input_tokens_details":{"image_tokens":6}}}`)
	result, failure := DecodeImageResponse(body, func(b64 string, index int) (*Artifact, *Error) {
		return &Artifact{}, nil
	})
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Usage == nil || result.Usage.ImageOutputTokens == nil || *result.Usage.ImageOutputTokens != 4 {
		t.Fatalf("usage %+v", result.Usage)
	}
}

func TestDecodeTranscriptionCarriesAudioInput(t *testing.T) {
	body := []byte(`{"text":"hello","duration":1.5,
		"usage":{"type":"tokens","input_tokens":50,"output_tokens":10,"total_tokens":60,
			"input_token_details":{"audio_tokens":42}}}`)
	result, failure := DecodeTranscriptionJSON(body)
	if failure != nil {
		t.Fatal(failure)
	}
	if result.Tokens == nil || result.Tokens.AudioInputTokens == nil || *result.Tokens.AudioInputTokens != 42 ||
		result.Tokens.InputTokens != 50 {
		t.Fatalf("tokens %+v", result.Tokens)
	}
}

func TestGeminiMediaModalityShares(t *testing.T) {
	// A Gemini usage's modality details map onto the shares: audio in and out,
	// images and video frames as image tokens.
	var result geminiResult
	if err := json.Unmarshal([]byte(`{"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4,"totalTokenCount":14,
		"promptTokensDetails":[{"modality":"AUDIO","tokenCount":3},{"modality":"VIDEO","tokenCount":2}],
		"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":4}]}}`), &result); err != nil {
		t.Fatal(err)
	}
	usage := result.tokens()
	if usage == nil || usage.AudioInputTokens == nil || *usage.AudioInputTokens != 3 ||
		usage.ImageInputTokens == nil || *usage.ImageInputTokens != 2 ||
		usage.ImageOutputTokens == nil || *usage.ImageOutputTokens != 4 {
		t.Fatalf("usage %+v", usage)
	}
}
