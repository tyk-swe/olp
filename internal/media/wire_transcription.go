package media

import "encoding/json"

type TranscriptionResult struct {
	TextOnly        bool
	Tokens          *ImageUsage
	Text            string
	Language        *string
	DurationSeconds *float64
	Segments        []TranscriptionSegment
	Extra           map[string]json.RawMessage
}

type TranscriptionSegment struct {
	ID      json.RawMessage
	Start   float64
	End     float64
	Text    string
	Speaker *string
	Extra   map[string]json.RawMessage
}

type transcriptionAudioDetailWire struct {
	AudioTokens *int64 `json:"audio_tokens"`
}

type transcriptionUsageWire struct {
	Type         string                        `json:"type"`
	InputTokens  int64                         `json:"input_tokens"`
	OutputTokens int64                         `json:"output_tokens"`
	TotalTokens  int64                         `json:"total_tokens"`
	InputDetail  *transcriptionAudioDetailWire `json:"input_token_details"`
}

type transcriptionTextOnlyWire struct {
	Text string `json:"text"`
}

type transcriptionSegmentWire struct {
	ID      json.RawMessage `json:"id,omitempty"`
	Start   float64         `json:"start"`
	End     float64         `json:"end"`
	Text    string          `json:"text"`
	Speaker *string         `json:"speaker,omitempty"`
}

type transcriptionResponseWire struct {
	Text     string             `json:"text"`
	Language *string            `json:"language,omitempty"`
	Duration *float64           `json:"duration,omitempty"`
	Segments *[]json.RawMessage `json:"segments,omitempty"`
}

func TranscriptionFormatIsText(format string) bool {
	return format == "text" || format == "srt" || format == "vtt"
}

func DecodeTranscriptionJSON(body []byte) (*TranscriptionResult, *Error) {
	raw, err := parseRawObject(body)
	if err != nil {
		return nil, ProtocolError("The provider transcription is not valid JSON.")
	}
	var text, language *string
	var duration *float64
	var rawSegments []json.RawMessage
	var usage *transcriptionUsageWire
	if firstErr(
		decodeField(raw, "text", &text),
		decodeField(raw, "language", &language),
		decodeField(raw, "duration", &duration),
		decodeField(raw, "segments", &rawSegments),
		decodeField(raw, "usage", &usage),
	) != nil {
		return nil, ProtocolError("The provider transcription is not valid JSON.")
	}
	if text == nil {
		return nil, ProtocolError("The provider transcription is missing text.")
	}
	segments, failure := decodeTranscriptionSegments(rawSegments)
	if failure != nil {
		return nil, failure
	}
	return &TranscriptionResult{
		Text:            *text,
		Language:        language,
		DurationSeconds: duration,
		Segments:        segments,
		Tokens:          transcriptionTokens(usage),
		Extra:           diffRawFields(raw, "text", "language", "duration", "segments"),
	}, nil
}

func decodeTranscriptionSegments(rawSegments []json.RawMessage) ([]TranscriptionSegment, *Error) {
	if len(rawSegments) == 0 {
		return nil, nil
	}
	segments := make([]TranscriptionSegment, len(rawSegments))
	for i, itemBytes := range rawSegments {
		segment, failure := decodeTranscriptionSegment(itemBytes)
		if failure != nil {
			return nil, failure
		}
		segments[i] = segment
	}
	return segments, nil
}

func decodeTranscriptionSegment(raw []byte) (TranscriptionSegment, *Error) {
	fields, err := parseRawObject(raw)
	if err != nil {
		return TranscriptionSegment{}, ProtocolError("The provider transcription is not valid JSON.")
	}
	var start, end float64
	var text string
	var speaker *string
	if firstErr(
		decodeField(fields, "start", &start),
		decodeField(fields, "end", &end),
		decodeField(fields, "text", &text),
		decodeField(fields, "speaker", &speaker),
	) != nil {
		return TranscriptionSegment{}, ProtocolError("The provider transcription is not valid JSON.")
	}
	id := fields["id"]
	if failure := validateSegmentID(id); failure != nil {
		return TranscriptionSegment{}, failure
	}
	return TranscriptionSegment{
		ID:      id,
		Start:   start,
		End:     end,
		Text:    text,
		Speaker: speaker,
		Extra:   diffRawFields(fields, "id", "start", "end", "text", "speaker"),
	}, nil
}

func validateSegmentID(id json.RawMessage) *Error {
	if len(id) == 0 || isJSON(id, "null") {
		return nil
	}
	var text string
	if json.Unmarshal(id, &text) == nil {
		return nil
	}
	var number int64
	if json.Unmarshal(id, &number) == nil {
		return nil
	}
	return ProtocolError("The provider transcription segment ID must be a string or integer.")
}

func transcriptionTokens(usage *transcriptionUsageWire) *ImageUsage {
	if usage == nil || (usage.Type != "" && usage.Type != "tokens") {
		return nil
	}
	tokens := &ImageUsage{
		InputTokens:  usage.InputTokens,
		OutputTokens: usage.OutputTokens,
		TotalTokens:  usage.TotalTokens,
	}
	if usage.InputDetail != nil {
		tokens.AudioInputTokens = usage.InputDetail.AudioTokens
	}
	return tokens
}

func EncodeTranscriptionJSON(result *TranscriptionResult) ([]byte, *Error) {
	if result.TextOnly {
		body, err := json.Marshal(transcriptionTextOnlyWire{Text: result.Text})
		if err != nil {
			return nil, ProtocolError("The provider transcription could not be encoded.")
		}
		return body, nil
	}
	var encodedSegments *[]json.RawMessage
	if result.Segments != nil {
		items := make([]json.RawMessage, len(result.Segments))
		for i, segment := range result.Segments {
			wireSeg := transcriptionSegmentWire{
				Start:   segment.Start,
				End:     segment.End,
				Text:    segment.Text,
				Speaker: segment.Speaker,
			}
			if len(segment.ID) > 0 && !isJSON(segment.ID, "null") {
				wireSeg.ID = segment.ID
			}
			rawSeg, err := marshalWithExtra(wireSeg, segment.Extra)
			if err != nil {
				return nil, ProtocolError("The provider transcription could not be encoded.")
			}
			items[i] = rawSeg
		}
		encodedSegments = &items
	}
	doc := transcriptionResponseWire{
		Text:     result.Text,
		Language: result.Language,
		Duration: result.DurationSeconds,
		Segments: encodedSegments,
	}
	body, err := marshalWithExtra(doc, result.Extra)
	if err != nil {
		return nil, ProtocolError("The provider transcription could not be encoded.")
	}
	return body, nil
}
