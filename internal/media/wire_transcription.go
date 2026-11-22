package media

import (
	"bytes"
	"encoding/json"
)

type TranscriptionResult struct {
	TextOnly        bool
	Tokens          *ImageUsage
	Text            string
	Language        *string
	DurationSeconds *float64
	Segments        []TranscriptionSegment
	Extra           map[string]any
}

type TranscriptionSegment struct {
	ID      json.RawMessage
	Start   float64
	End     float64
	Text    string
	Speaker *string
	Extra   map[string]any
}

func TranscriptionFormatIsText(format string) bool {
	return format == "text" || format == "srt" || format == "vtt"
}

func DecodeTranscriptionJSON(body []byte) (*TranscriptionResult, *Error) {
	var encodedWire struct {
		Text     *string  `json:"text"`
		Language *string  `json:"language"`
		Duration *float64 `json:"duration"`
		Segments []struct {
			ID      json.RawMessage `json:"id"`
			Start   float64         `json:"start"`
			End     float64         `json:"end"`
			Text    string          `json:"text"`
			Speaker *string         `json:"speaker"`
		} `json:"segments"`
		Usage *struct {
			Type         string `json:"type"`
			InputTokens  int64  `json:"input_tokens"`
			OutputTokens int64  `json:"output_tokens"`
			TotalTokens  int64  `json:"total_tokens"`
			InputDetail  *struct {
				AudioTokens *int64 `json:"audio_tokens"`
			} `json:"input_token_details"`
		} `json:"usage"`
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, ProtocolError("The provider transcription is not valid JSON.")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&encodedWire); err != nil {
		return nil, ProtocolError("The provider transcription is not valid JSON.")
	}
	if encodedWire.Text == nil {
		return nil, ProtocolError("The provider transcription is missing text.")
	}
	for _, segment := range encodedWire.Segments {
		if len(segment.ID) == 0 || string(segment.ID) == "null" {
			continue
		}
		var text string
		if err := json.Unmarshal(segment.ID, &text); err == nil {
			continue
		}
		var number int64
		if err := json.Unmarshal(segment.ID, &number); err != nil {
			return nil, ProtocolError("The provider transcription segment ID must be a string or integer.")
		}
	}
	result := &TranscriptionResult{
		Text: *encodedWire.Text, Language: encodedWire.Language, DurationSeconds: encodedWire.Duration,
		Extra: diffFields(doc, "text", "language", "duration", "segments"),
	}
	if encodedWire.Usage != nil && (encodedWire.Usage.Type == "" || encodedWire.Usage.Type == "tokens") {
		result.Tokens = &ImageUsage{
			InputTokens: encodedWire.Usage.InputTokens, OutputTokens: encodedWire.Usage.OutputTokens,
			TotalTokens: encodedWire.Usage.TotalTokens,
		}
		if encodedWire.Usage.InputDetail != nil {
			result.Tokens.AudioInputTokens = encodedWire.Usage.InputDetail.AudioTokens
		}
	}
	if segments, ok := doc["segments"].([]any); ok && len(segments) == len(encodedWire.Segments) {
		for i, item := range segments {
			extra := map[string]any(nil)
			if object, ok := item.(map[string]any); ok {
				extra = diffFields(object, "id", "start", "end", "text", "speaker")
			}
			segment := encodedWire.Segments[i]
			result.Segments = append(result.Segments, TranscriptionSegment{
				ID: segment.ID, Start: segment.Start, End: segment.End,
				Text: segment.Text, Speaker: segment.Speaker, Extra: extra,
			})
		}
	} else {
		for _, segment := range encodedWire.Segments {
			result.Segments = append(result.Segments, TranscriptionSegment{
				ID: segment.ID, Start: segment.Start, End: segment.End,
				Text: segment.Text, Speaker: segment.Speaker,
			})
		}
	}
	return result, nil
}

func EncodeTranscriptionJSON(result *TranscriptionResult) ([]byte, *Error) {
	doc := map[string]any{"text": result.Text}
	if result.TextOnly {
		body, err := json.Marshal(doc)
		if err != nil {
			return nil, ProtocolError("The provider transcription could not be encoded.")
		}
		return body, nil
	}
	if result.Language != nil {
		doc["language"] = *result.Language
	}
	if result.DurationSeconds != nil {
		doc["duration"] = *result.DurationSeconds
	}
	segments := make([]any, len(result.Segments))
	for i, segment := range result.Segments {
		item := map[string]any{"start": segment.Start, "end": segment.End, "text": segment.Text}
		if len(segment.ID) > 0 && string(segment.ID) != "null" {
			item["id"] = segment.ID
		}
		if segment.Speaker != nil {
			item["speaker"] = *segment.Speaker
		}
		for name, value := range segment.Extra {
			if _, exists := item[name]; !exists {
				item[name] = value
			}
		}
		segments[i] = item
	}
	if result.Segments != nil {
		doc["segments"] = segments
	}
	for name, value := range result.Extra {
		if _, exists := doc[name]; !exists {
			doc[name] = value
		}
	}
	body, err := json.Marshal(doc)
	if err != nil {
		return nil, ProtocolError("The provider transcription could not be encoded.")
	}
	return body, nil
}
