package media

import (
	"time"

	"github.com/tyk-swe/olp/internal/staging"
)

type Response interface{ response() }

type StageImage func(b64 string, index int) (*staging.Artifact, *Error)

type ImageReply struct {
	Decode func(body []byte, stage StageImage) (*ImageResult, *Error)
	Poll   *Poll
}

type AudioReply struct{ CharacterHeader string }

type InlineAudioReply struct {
	Decode func(body []byte) (*AudioResult, *Error)
}

type TranscriptReply struct {
	Format string
	Decode func(body []byte) (*TranscriptionResult, *Error)
	Poll   *Poll
}

type EventReply struct{}

type VideoJobReply struct {
	Decode func(body []byte) (*VideoJobResult, *Error)
}

type VideoDeletedReply struct{ NoContent bool }

type VideoContentReply struct{ Poll *Poll }

func (ImageReply) response()        {}
func (AudioReply) response()        {}
func (InlineAudioReply) response()  {}
func (TranscriptReply) response()   {}
func (EventReply) response()        {}
func (VideoJobReply) response()     {}
func (VideoDeletedReply) response() {}
func (VideoContentReply) response() {}

func (r ImageReply) Images(body []byte, stage StageImage) (*ImageResult, *Error) {
	if r.Decode == nil {
		return DecodeImageResponse(body, stage)
	}
	return r.Decode(body, stage)
}

func (r TranscriptReply) Raw() bool {
	return TranscriptionFormatIsText(r.Format) && r.Decode == nil
}

func (r TranscriptReply) Transcript(body []byte) (*TranscriptionResult, *Error) {
	if r.Decode == nil {
		return DecodeTranscriptionJSON(body)
	}
	result, failure := r.Decode(body)
	if failure != nil {
		return nil, failure
	}
	translated := *result
	translated.TextOnly = r.Format == "json"
	return &translated, nil
}

func (r VideoJobReply) Video(body []byte) (*VideoJobResult, *Error) {
	if r.Decode == nil {
		return DecodeVideoObject(body)
	}
	return r.Decode(body)
}

type Poll struct {
	Next    func(body []byte) (Step, *Error)
	Domains []string
}

type Step interface{ step() }

type Visit struct {
	Method      string
	URL         string
	JSON        []byte
	Credentials bool
	Wait        time.Duration
}

type Again struct{}

type Fetch struct {
	URL    string
	Prefix string
}

type Finished struct{ Cleanup *Visit }

func (Visit) step()    {}
func (Again) step()    {}
func (Fetch) step()    {}
func (Finished) step() {}
