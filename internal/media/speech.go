package media

import (
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/staging"
)

type Speech struct {
	Input        string   `json:"input"`
	Voice        string   `json:"voice"`
	Format       *string  `json:"response_format"`
	Instructions *string  `json:"instructions"`
	Speed        *float64 `json:"speed"`
	StreamFormat *string  `json:"stream_format"`
}

func (Speech) Operation() Operation    { return OpSpeech }
func (Speech) uploads() []staging.Part { return nil }
func (p Speech) streaming() bool       { return valueOr(p.StreamFormat, "") == "sse" }

func DecodeSpeech(body []byte) (*Request, *Error) {
	var p Speech
	route, source, extra, failure := decodeJSON(body, &p)
	switch {
	case failure != nil:
		return nil, failure
	case p.Input == "":
		return nil, invalidMedia("The speech input is required.")
	case p.Voice == "":
		return nil, invalidMedia("The speech voice is required.")
	case p.Speed != nil && (*p.Speed < 0.25 || *p.Speed > 4.0):
		return nil, invalidMedia("The speech speed must be between 0.25 and 4.0.")
	}
	return newRequest(route, p, extra, source)
}

func encodeSpeech(r *Request, p Speech, _ connectors.Config, model string) (*UpstreamCall, *Error) {
	fields := map[string]any{
		"model": model, "input": p.Input, "voice": p.Voice, "response_format": p.Format,
		"instructions": p.Instructions, "speed": p.Speed, "stream_format": p.StreamFormat,
	}
	body, failure := jsonDoc(fields, r.Extra())
	if failure != nil {
		return nil, failure
	}
	kind := ResponseBinary
	if p.streaming() {
		kind = ResponseSSE
	}
	return &UpstreamCall{Method: "POST", Path: "audio/speech", Accept: "application/json",
		JSON: body, Stream: p.streaming(), Kind: kind, Ambiguous: true}, nil
}
